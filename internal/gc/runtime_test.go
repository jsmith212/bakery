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

	// No flushers: these tests are about the engine's own scheduling and refusals,
	// and gc.New tolerates a missing toucher (a fake harness has nothing to flush).
	// The pre-sweep flush itself is proven against a real database in sweep_test.go.
	eng, err := New(t.Context(), Deps{
		DB: q, Blobs: b, Metrics: metrics.New(), Log: slog.New(slog.DiscardHandler),
		Access: nil, Unihash: nil,
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

// The toucher's T RAMPS OFF A REAL CLOCK (spec §6.4, J: R6#7/R7#6/R8#5).
//
// 000012 stamps gc_state.touch_ramp_until at now() + 7 days, and that timestamp --
// not a fraction of untouched rows recomputed per backend, which was
// last-backend-wins and never converged on a cold corpus -- is what decides whether
// T is the widened 24h or the configured steady value.
func TestTouchStalenessRampsOffGCState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		until time.Time
		err   error
		want  time.Duration
	}{
		{
			name:  "before the ramp expires",
			until: time.Now().Add(48 * time.Hour),
			err:   nil,
			want:  touchStalenessRamped,
		},
		{
			name:  "after the ramp expires",
			until: time.Now().Add(-time.Minute),
			err:   nil,
			want:  time.Hour,
		},
		{
			// A read failure keeps the CONSERVATIVE end: fewer accessed_at writes than
			// asked for costs write amplification; the other way round concentrates the
			// one-time non-HOT first touch of every pre-existing row into one hour.
			name:  "an unreadable clock stays ramped",
			until: time.Now().Add(-time.Minute),
			err:   errors.New("gc_state is on fire"),
			want:  touchStalenessRamped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, q, _ := fakeEngine(t, testConfig())

			if got := eng.TouchStaleness(); got != touchStalenessRamped {
				t.Errorf("TouchStaleness() before LoadTouchRamp = %v, want the ramped %v: an "+
					"unread clock must not tighten T", got, touchStalenessRamped)
			}

			q.mu.Lock()
			q.rampUntil, q.rampErr = tc.until, tc.err
			q.mu.Unlock()

			err := eng.LoadTouchRamp(t.Context())
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("LoadTouchRamp() error = %v, want error presence %v", err, tc.err != nil)
			}

			if got := eng.TouchStaleness(); got != tc.want {
				t.Errorf("TouchStaleness() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TriggerAsync IS GATED THE SAME WAY THE LOOP IS (R7#1), and it has to be: the API
// reaches the engine without going through Loop at all, so a refusal that lives
// only in Loop lets an operator start by hand precisely the sweep the loop declines
// to schedule -- process-local LRU invalidation against another instance's cache, a
// boot reaper that fails a live run, a pending-read veto blind to the other
// instance's reads.
func TestTriggerAsyncRefusesDisabledAndMultiInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  func(Config) Config
		want error
	}{
		{
			name: "multi-instance",
			cfg:  func(c Config) Config { c.AllowMultiInstance = true; return c },
			want: ErrMultiInstance,
		},
		{
			name: "disabled",
			cfg:  func(c Config) Config { c.Enabled = false; return c },
			want: ErrDisabled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, q, _ := fakeEngine(t, tc.cfg(testConfig()))
			seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 5)

			if _, err := eng.TriggerAsync(t.Context(), TriggerAPI, false); !errors.Is(err, tc.want) {
				t.Errorf("TriggerAsync() error = %v, want %v", err, tc.want)
			}

			// A DRY run is refused too: the refusal is about this deployment's
			// configuration, and a dry run still writes a gc_runs row and still reports
			// what a sweep nobody may run would have done.
			if _, err := eng.TriggerAsync(t.Context(), TriggerAPI, true); !errors.Is(err, tc.want) {
				t.Errorf("dry TriggerAsync() error = %v, want %v", err, tc.want)
			}

			if got := q.count("StartGCRun"); got != 0 {
				t.Errorf("%d runs were started by a refused trigger, want 0", got)
			}
		})
	}
}

// THE FIRST SWEEP DOES NOT WAIT A WHOLE INTERVAL (R7#7). At the shipped six-hour
// interval, a deployment that restarts more often than that would otherwise never
// sweep at all -- and nothing says so, because a loop that has not run yet emits
// exactly what a healthy idle one does.
func TestLoopSweepsOnceAtStartup(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Interval = 2 * time.Second

	eng, q, _ := fakeEngine(t, cfg)
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	started := time.Now()

	go eng.Loop(ctx)

	waitForCondition(t, func() bool { return q.count("StartGCRun") > 0 })

	// The ticker's first tick is one full interval out, so a first run that lands
	// before then can only have come from the startup timer.
	if elapsed := time.Since(started); elapsed >= cfg.Interval {
		t.Errorf("the first sweep started after %v, want less than the %v interval -- the "+
			"startup timer is not firing", elapsed, cfg.Interval)
	}
}

// The startup delay is jittered so a fleet restarted together does not start every
// instance's first sweep in the same second, and capped by the interval so a short
// interval does not spend half of itself waiting.
func TestFirstSweepDelayIsBoundedAndJittered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
	}{
		{name: "production interval", interval: 6 * time.Hour},
		{name: "interval shorter than the cap", interval: time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig()
			cfg.Interval = tc.interval

			eng, _, _ := fakeEngine(t, cfg)

			bound := min(sweepStartupDelay, tc.interval)
			seen := map[time.Duration]struct{}{}

			for range 50 {
				d := eng.firstSweepDelay()

				if d < bound/10 || d > bound/2 {
					t.Fatalf("firstSweepDelay() = %v, want within [%v, %v]", d, bound/10, bound/2)
				}

				seen[d] = struct{}{}
			}

			if len(seen) < 2 {
				t.Error("firstSweepDelay() never varied over 50 calls: a fleet restarted together " +
					"would start every instance's first sweep in the same second")
			}
		})
	}
}

// AN API-TRIGGERED SWEEP IS THE SERVER'S TO CANCEL AND THE SERVER'S TO WAIT FOR
// (R7#8).
//
// It cannot run under the REQUEST's context (that dies with the response, often
// before the first chunk) and it must not run under context.WithoutCancel of it
// either: that produced a sweep nothing could cancel and nothing waited for, still
// deleting rows through a pool that was closing, and leaving its gc_runs row
// 'running' for the next boot's reaper to clean up. The lifetime context gives
// both halves -- deaf to the client hanging up, cancelled by shutdown -- and
// AsyncDone is what lets Boot wait for the terminal FinishGCRun.
func TestTriggerAsyncRunsUnderTheLifetimeContext(t *testing.T) {
	t.Parallel()

	q, b := newFakeQueries(), newFakeBlobs()
	seedFakeBackend(q, repository.BackendKindDownloads, nsDefault, 500)

	lifetime, shutdown := context.WithCancel(t.Context())
	defer shutdown()

	eng, err := New(lifetime, Deps{
		DB: q, Blobs: b, Metrics: metrics.New(), Log: slog.New(slog.DiscardHandler),
		Access: nil, Unihash: nil,
	}, func() Config { c := testConfig(); c.BatchSize = 50; return c }())
	if err != nil {
		t.Fatalf("gc.New() error = %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	// Page 0 PARKS until the test releases it. Without the park, the whole
	// fake-backed sweep can complete between `entered` and the assertion below --
	// a sweep that FINISHED also proves the dead request context did not kill it,
	// but the probe cannot tell that apart from a cancelled one. Observed as a
	// real flake under coverage instrumentation in CI; the park makes "still
	// mid-sweep" a fact rather than a race. onScan runs with no fake lock held,
	// so blocking here cannot deadlock the assertion's own q.mu.Lock().
	q.onScan = func(page int) {
		if page == 0 {
			once.Do(func() { close(entered) })
			<-release
		}
	}

	// The REQUEST's context, and it dies immediately -- exactly what an HTTP handler
	// hands over once the response is written.
	reqCtx, endRequest := context.WithCancel(t.Context())

	if _, err := eng.TriggerAsync(reqCtx, TriggerAPI, false); err != nil {
		t.Fatalf("TriggerAsync() error = %v", err)
	}

	endRequest()
	<-entered

	// Still sweeping -- provably, it is parked inside page 0 -- so the dead
	// request context did not take it with it.
	q.mu.Lock()
	finished := len(q.finished)
	q.mu.Unlock()

	if finished != 0 {
		t.Fatal("the detached sweep finished the moment its request context died")
	}

	// Shutdown BEFORE the release: the sweep wakes into an already-cancelled
	// lifetime, notices it at the next between-chunks check, and writes the
	// failed/errShutdown terminal row the assertions below expect.
	shutdown()
	close(release)

	select {
	case <-eng.AsyncDone():
	case <-time.After(10 * time.Second):
		t.Fatal("AsyncDone never closed: shutdown cannot wait for an API-triggered sweep")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.finished) != 1 {
		t.Fatalf("FinishGCRun called %d times, want 1: a cancelled sweep still writes its "+
			"terminal row, or it holds the active slot until the next boot", len(q.finished))
	}

	if q.finished[0].Status != repository.GcRunStatusFailed ||
		q.finished[0].Error.String != errShutdown {
		t.Errorf("terminal row = %v/%q, want failed/%q",
			q.finished[0].Status, q.finished[0].Error.String, errShutdown)
	}
}
