package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// fakeEngine wires an Engine onto the in-memory fakes.
func fakeEngine(t *testing.T, cfg Config) (*Engine, *fakeQueries, *fakeBlobs) {
	t.Helper()

	q, b := newFakeQueries(), newFakeBlobs()

	eng, err := New(Deps{
		DB: q, Blobs: b, Metrics: metrics.New(), Log: slog.New(slog.DiscardHandler),
	}, cfg)
	if err != nil {
		t.Fatalf("gc.New() error = %v", err)
	}

	return eng, q, b
}

// seedFakeBackend registers one sweepable backend and n cold objects in it.
func seedFakeBackend(q *fakeQueries, kind repository.BackendKind, namespace string, n int) {
	q.backends = append(q.backends, repository.ListBackendsForGCRow{
		ID: 1, Kind: kind, Enabled: true, RetentionWindow: interval(day(30)),
		QuotaBytes:  pgtype.Int8{Int64: 0, Valid: false},
		ProjectID:   uuidOf(1),
		ProjectSlug: "widget", OrgID: uuidOf(2), OrgSlug: "acme",
	})

	cold := pgtype.Timestamptz{Time: q.startedAt.Add(-400 * 24 * time.Hour), InfinityModifier: 0, Valid: true}

	for i := range n {
		q.addObject(1, repository.ScanObjectsForGCRow{
			Namespace: namespace,
			Key:       fmt.Sprintf("object-%05d", i),
			Digest:    make([]byte, 32),
			SizeBytes: 10,
			CreatedAt: cold,
			AccessedAt: pgtype.Timestamptz{
				Time: time.Time{}, InfinityModifier: 0, Valid: false,
			},
			UpdatedAt:   cold,
			ContentType: pgtype.Text{String: "", Valid: false},
		})
	}
}

// THE PACING IS THE WHOLE OF THE SWEEP'S RATE LIMITING (spec §8, §9.8).
//
// A sweep competes with live builds for the same table, so it moves in
// --gc-batch-size chunks with a --gc-batch-pause between them and reads nothing
// larger. There is no other throttle: no statement-level rate limit, no adaptive
// backoff, no priority. If the chunk size stopped being honoured, the first symptom
// would be a sweep holding a scan over a million rows while a HEAD storm queues
// behind it.
func TestSweepRespectsBatchSizeAndPause(t *testing.T) {
	t.Parallel()

	const (
		objects = 250
		batch   = 100
		pause   = 20 * time.Millisecond
	)

	cfg := testConfig()
	cfg.BatchSize = batch
	cfg.BatchPause = pause

	eng, q, blobs := fakeEngine(t, cfg)
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, objects)

	start := time.Now()

	if _, err := eng.Run(t.Context(), TriggerInterval, false); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	elapsed := time.Since(start)

	if got := q.count("ScanObjectsForGC"); got != 3 {
		t.Errorf("%d scans for %d objects at batch %d, want 3", got, objects, batch)
	}

	for i, limit := range q.scanLimits {
		if limit != batch {
			t.Errorf("scan %d asked for %d rows, want %d", i, limit, batch)
		}
	}

	// Two pauses, not three: the pass stops as soon as a page comes back short
	// rather than sleeping once more to discover the corpus is exhausted.
	if want := 2 * pause; elapsed < want {
		t.Errorf("sweep took %v, want at least %v -- the inter-chunk pause is not being taken", elapsed, want)
	}

	if got := len(blobs.deletedKeys()); got != objects {
		t.Errorf("deleted %d objects, want %d", got, objects)
	}
}

// SHUTDOWN FINISHES THE CHUNK AND THEN FINISHES THE RUN (spec §9.5).
//
// A run left 'running' is not cosmetic: it holds gc_runs' partial unique index slot
// and every later real run collides with it until a boot that holds the advisory lock
// clears it. So the terminal UPDATE runs under context.WithoutCancel, which is the
// only reason it can be written at all -- by the time it is reached, the context that
// carried the sweep is already dead.
func TestShutdownMidSweepFinishesTheRun(t *testing.T) {
	t.Parallel()

	const batch = 50

	cfg := testConfig()
	cfg.BatchSize = batch

	eng, q, blobs := fakeEngine(t, cfg)
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 500)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Cancel while the FIRST page is being answered: the chunk in flight is allowed to
	// finish, and the check that stops the pass is the one at the top of the next
	// iteration.
	q.onScan = func(page int) {
		if page == 0 {
			cancel()
		}
	}

	_, err := eng.Run(ctx, TriggerInterval, false)
	if err == nil {
		t.Fatal("Run() returned nil after its context was cancelled")
	}

	if got := len(blobs.deletedKeys()); got != batch {
		t.Errorf("deleted %d objects, want the %d of the chunk that was already in flight", got, batch)
	}

	if len(q.finished) != 1 {
		t.Fatalf("FinishGCRun called %d times, want 1 -- a run left 'running' holds the active slot",
			len(q.finished))
	}

	done := q.finished[0]
	if done.Status != repository.GcRunStatusFailed {
		t.Errorf("terminal status = %q, want failed", done.Status)
	}

	if done.Error.String != errShutdown {
		t.Errorf("terminal error = %q, want %q -- an operator restart and a broken statement are "+
			"different incidents and the runs list is where they are told apart",
			done.Error.String, errShutdown)
	}

	if done.ObjectsDeleted != int64(batch) {
		t.Errorf("terminal objects_deleted = %d, want %d: a partial sweep still records what it did",
			done.ObjectsDeleted, batch)
	}
}

// THE LOOP REFUSES TO RUN MULTI-INSTANCE, LOUDLY (spec §9.4).
//
// Three reasons, and every one of them silently deletes data another instance is
// still serving: DeleteBatch's LRU invalidation is process-local, the boot reaper
// would fail the other instance's live sweep, and the pending-touch veto is only
// complete because this process is the only one serving reads.
func TestLoopRefusesMultiInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      func(Config) Config
		wantRuns int
	}{
		{
			name:     "multi-instance",
			cfg:      func(c Config) Config { c.AllowMultiInstance = true; return c },
			wantRuns: 0,
		},
		{
			name:     "disabled",
			cfg:      func(c Config) Config { c.Enabled = false; return c },
			wantRuns: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := tc.cfg(testConfig())
			cfg.Interval = time.Millisecond

			eng, q, _ := fakeEngine(t, cfg)

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()

			eng.Loop(ctx)

			if got := q.count("StartGCRun"); got != tc.wantRuns {
				t.Errorf("%d runs started, want %d", got, tc.wantRuns)
			}
		})
	}
}

// A SECOND REAL RUN IS REFUSED IN PROCESS so the API trigger can answer 409 without
// first writing a row that is going to violate gc_runs' partial unique index. Dry
// runs hold no slot and are deliberately not guarded.
func TestConcurrentRealRunIsRefused(t *testing.T) {
	t.Parallel()

	eng, q, _ := fakeEngine(t, testConfig())
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 10)

	release := make(chan struct{})
	entered := make(chan struct{})

	q.onScan = func(page int) {
		if page != 0 {
			return
		}

		close(entered)
		<-release
	}

	go func() {
		_, _ = eng.Run(t.Context(), TriggerInterval, false)
	}()

	<-entered

	if _, err := eng.Run(t.Context(), TriggerInterval, false); err == nil {
		t.Error("a second real run started while one was in flight")
	}

	close(release)
}

// TriggerAsync RETURNS THE MOMENT gc_runs EXISTS, not when the sweep finishes
// (spec §9.10): POST /api/v1/gc/run must answer 202 with an id to poll, never
// block on a pass that can legitimately run for hours.
func TestTriggerAsyncReturnsBeforeTheSweepFinishes(t *testing.T) {
	t.Parallel()

	eng, q, blobs := fakeEngine(t, testConfig())
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 5)

	release := make(chan struct{})

	q.onScan = func(page int) {
		if page == 0 {
			<-release
		}
	}

	id, err := eng.TriggerAsync(t.Context(), TriggerAPI, false)
	if err != nil {
		t.Fatalf("TriggerAsync() error = %v", err)
	}

	if id == 0 {
		t.Fatal("TriggerAsync() returned a zero run id")
	}

	// The sweep is still blocked in its first page: nothing has finished yet, and
	// TriggerAsync returning at all is the proof it did not wait for that.
	q.mu.Lock()
	stillRunning := len(q.finished) == 0
	q.mu.Unlock()

	if !stillRunning {
		t.Fatal("FinishGCRun was already called -- TriggerAsync waited for the sweep instead of detaching it")
	}

	close(release)

	waitForCondition(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()

		return len(q.finished) == 1
	})

	if got := len(blobs.deletedKeys()); got != 5 {
		t.Errorf("deleted %d objects, want 5 -- the detached sweep did not run to completion", got)
	}
}

// A SECOND REAL TRIGGER IS REFUSED while the first is still in flight, the same
// 409 contract TestConcurrentRealRunIsRefused proves for Run: TriggerAsync shares
// Run's CompareAndSwap rather than inventing a second guard that could disagree
// with it.
func TestTriggerAsyncSecondRealRunIsRefused(t *testing.T) {
	t.Parallel()

	eng, q, _ := fakeEngine(t, testConfig())
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 5)

	release := make(chan struct{})
	entered := make(chan struct{})

	var once sync.Once

	q.onScan = func(page int) {
		if page != 0 {
			return
		}

		once.Do(func() { close(entered) })
		<-release
	}

	if _, err := eng.TriggerAsync(t.Context(), TriggerAPI, false); err != nil {
		t.Fatalf("first TriggerAsync() error = %v", err)
	}

	<-entered

	if _, err := eng.TriggerAsync(t.Context(), TriggerAPI, false); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second TriggerAsync() error = %v, want ErrAlreadyRunning", err)
	}

	close(release)

	waitForCondition(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()

		return len(q.finished) == 1
	})
}

// waitForCondition polls cond until it is true or the test times out. Used ONLY
// for TriggerAsync's tests: the sweep it starts genuinely runs on its own
// goroutine, so there is no synchronous return to assert against for "the
// detached run finished".
func waitForCondition(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("condition was never met")
}

// The toucher's T RAMPS (spec §6.4), and it ramps on the state the spec's "7 days
// after migration" was standing in for: how much of the corpus has never been
// touched. 000012's fillfactor change is catalog-only, so the first touch of every
// pre-existing row is a non-HOT update -- opening at 24h spreads that one-time spike.
func TestTouchStalenessRamps(t *testing.T) {
	t.Parallel()

	eng, _, _ := fakeEngine(t, testConfig())

	if got := eng.TouchStaleness(); got != touchStalenessRamped {
		t.Errorf("TouchStaleness() at boot = %v, want %v: nothing has been measured yet and every "+
			"row in the database is untouched", got, touchStalenessRamped)
	}

	eng.noteTouchRamp(&usage{objects: 100, bytes: 0, nullAccessed: 90, stages: nil})

	if got := eng.TouchStaleness(); got != touchStalenessRamped {
		t.Errorf("TouchStaleness() at 90%% untouched = %v, want %v", got, touchStalenessRamped)
	}

	eng.noteTouchRamp(&usage{objects: 100, bytes: 0, nullAccessed: 10, stages: nil})

	if got := eng.TouchStaleness(); got != time.Hour {
		t.Errorf("TouchStaleness() at 10%% untouched = %v, want the configured steady 1h", got)
	}
}
