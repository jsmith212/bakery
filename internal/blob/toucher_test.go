package blob

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// THE TOUCHER.
//
// accessed_at is the column that separates "nobody has read this in ninety days" from
// "this was created ninety days ago and is read on every build". The whole M6 retention
// policy is decided by it, so what these tests protect is not that a write happens --
// it is HOW MANY happen (one per key per staleness window, never one per read) and
// that the last one is not lost at shutdown.

// fakeClock drives the toucher without sleeping through a one-hour window.
type fakeClock struct{ nanos atomic.Int64 }

func (c *fakeClock) now() int64 { return c.nanos.Load() }

func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(d.Nanoseconds()) }

// newToucherService is newTestService plus a write half: the real generated queries
// over a fake DBTX.
func newToucherService(tb testing.TB, r *fakeReader, db *fakeDBTX) (*Service, *fakeClock) {
	tb.Helper()

	st, err := storage.NewLocal(tb.TempDir())
	if err != nil {
		tb.Fatalf("NewLocal() error = %v", err)
	}

	svc, err := New(Config{
		Reader:    r,
		Tx:        &fakeTxer{db: db},
		Storage:   st,
		Metrics:   metrics.New(),
		CacheSize: 4096,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		tb.Fatalf("New() error = %v", err)
	}

	clk := &fakeClock{nanos: atomic.Int64{}}
	clk.nanos.Store(time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC).UnixNano())
	svc.lru.nowNano = clk.now

	return svc, clk
}

// N READS IN T MUST COST ONE UPDATE.
//
// This is the property the whole design turns on. An inline accessed_at write would
// funnel a BB_NUMBER_THREADS-parallel HEAD storm into a row-lock convoy on the hottest
// rows in the database; a flusher that wrote every tick would still issue one statement
// a minute per backend forever. The mark is stamped only when unset and collected only
// once it is older than T, so 300 reads of 3 keys inside the window produce exactly one
// statement -- and the second window's reads produce exactly one more.
func TestToucherFlushIsCoalesced(t *testing.T) {
	const (
		keys   = 3
		reads  = 100
		window = time.Hour
	)

	repo := newFakeReader()
	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(1000+i))
	}

	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, clk := newToucherService(t, repo, db)

	for range reads {
		for i := range keys {
			if ok, err := svc.Exists(t.Context(), testRef(sstateKey(i))); err != nil || !ok {
				t.Fatalf("Exists(%d) = %v, %v; want true, nil", i, ok, err)
			}
		}
	}

	// Inside the window nothing is due: a mark younger than T is a read the SQL
	// staleness guard would refuse to write anyway.
	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	if got := len(db.calls("TouchObjectsAccessed")); got != 0 {
		t.Fatalf("flush inside the staleness window issued %d UPDATEs, want 0", got)
	}

	clk.advance(2 * window)

	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	calls := db.calls("TouchObjectsAccessed")
	if len(calls) != 1 {
		t.Fatalf("%d reads of %d keys issued %d UPDATEs, want 1", reads*keys, keys, len(calls))
	}

	touched, _ := calls[0].args[2].([]string)
	if len(touched) != keys {
		t.Errorf("one UPDATE carried %d keys, want %d -- the batch is not batching", len(touched), keys)
	}

	// The marks are cleared, so a flush with no reads behind it writes nothing.
	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	if got := len(db.calls("TouchObjectsAccessed")); got != 1 {
		t.Fatalf("a flush with no new reads issued %d UPDATEs total, want 1", got)
	}

	// A read after the flush re-marks: coalescing must not become suppression.
	if _, err := svc.Exists(t.Context(), testRef(sstateKey(0))); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	clk.advance(2 * window)

	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	if got := len(db.calls("TouchObjectsAccessed")); got != 2 {
		t.Errorf("a read in the second window issued %d UPDATEs total, want 2", got)
	}
}

// THE FINAL FLUSH, which StartKeyToucher does not have -- and that is a recorded bug,
// not a pattern. Without it every mark taken since the last tick dies with the process,
// so a deployment that restarts often can lose every read record it ever takes, and its
// whole corpus ages out on created_at.
//
// It also asserts the flush ran under context.WithoutCancel: the shutdown context is
// already cancelled when the flush starts, and a write that carried it would fail with
// context.Canceled at the pool and lose exactly the marks it was added to save.
func TestToucherFinalFlushOnShutdown(t *testing.T) {
	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 4096)

	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, _ := newToucherService(t, repo, db)

	if _, err := svc.Exists(t.Context(), testRef(sstateKey(1))); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	// An interval far longer than the test: the ONLY thing that can flush here is the
	// shutdown path.
	go func() {
		defer close(done)

		svc.StartAccessToucher(ctx, time.Hour, func() time.Duration { return time.Hour })
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartAccessToucher did not return after its context was cancelled")
	}

	calls := db.calls("TouchObjectsAccessed")
	if len(calls) != 1 {
		t.Fatalf("shutdown issued %d UPDATEs, want 1 -- the final flush is missing", len(calls))
	}

	if calls[0].ctxErr != nil {
		t.Errorf("the final flush ran under a cancelled context (%v) -- it must use context.WithoutCancel", calls[0].ctxErr)
	}
}

// THE VETO (spec 6.2). A pending touch is invisible to the sweep's SELECT: the row
// still carries the accessed_at it had ninety days ago, so a key read seconds ago is
// selected as cold. PendingTouch is the intersection that saves it, and it must answer
// for BOTH mark sources -- the LRU's stamp and the aux map's reachability marks -- and
// must keep answering true for the whole time the UPDATE is in flight.
func TestPendingTouchVetoesTheSweep(t *testing.T) {
	const window = time.Hour

	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 4096)

	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, clk := newToucherService(t, repo, db)

	read := testRef(sstateKey(1))
	unread := testRef(sstateKey(2))
	reachable := testRef(sstateKey(3))
	reachable.Namespace = "cas"

	if svc.PendingTouch(read.BackendID, read.Namespace, read.Key) {
		t.Fatal("PendingTouch() = true before anything was read")
	}

	if _, err := svc.Exists(t.Context(), read); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	// The reachability half: an AC hit names outputs nobody read (spec 6.3).
	svc.MarkAccessed(reachable)

	for _, tc := range []struct {
		name string
		ref  Ref
		want bool
	}{
		{name: "read through the LRU", ref: read, want: true},
		{name: "named by an ActionResult", ref: reachable, want: true},
		{name: "never touched", ref: unread, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.PendingTouch(tc.ref.BackendID, tc.ref.Namespace, tc.ref.Key); got != tc.want {
				t.Errorf("PendingTouch(%q) = %v, want %v", tc.ref.Key, got, tc.want)
			}
		})
	}

	// While the UPDATE is in flight the marks must still veto: clearing them at collect
	// time would open a window in which the sweep deletes a row whose accessed_at write
	// has not landed yet.
	var duringFlush atomic.Bool

	db.onExec = func(string) {
		duringFlush.Store(svc.PendingTouch(read.BackendID, read.Namespace, read.Key))
	}

	clk.advance(2 * window)

	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	if !duringFlush.Load() {
		t.Error("PendingTouch() = false while the accessed_at UPDATE was in flight -- the veto has a hole")
	}

	db.onExec = nil

	for _, ref := range []Ref{read, reachable} {
		if svc.PendingTouch(ref.BackendID, ref.Namespace, ref.Key) {
			t.Errorf("PendingTouch(%q) = true after a successful flush", ref.Key)
		}
	}
}

// A FAILED FLUSH KEEPS ITS MARKS. Clearing on the way out would drop the reads a
// transient database error swallowed -- and the veto would stop protecting them too.
func TestToucherKeepsMarksWhenTheFlushFails(t *testing.T) {
	const window = time.Hour

	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 4096)

	db := &fakeDBTX{execs: nil, errs: []error{io.ErrUnexpectedEOF}, reader: repo, onExec: nil}
	svc, clk := newToucherService(t, repo, db)

	ref := testRef(sstateKey(1))

	if _, err := svc.Exists(t.Context(), ref); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	clk.advance(2 * window)

	if _, err := svc.flushAccess(t.Context(), window, false); err == nil {
		t.Fatal("flushAccess() error = nil, want the injected failure")
	}

	if !svc.PendingTouch(ref.BackendID, ref.Namespace, ref.Key) {
		t.Fatal("PendingTouch() = false after a FAILED flush -- the mark was dropped")
	}

	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("retry flushAccess() error = %v", err)
	}

	if got := len(db.calls("TouchObjectsAccessed")); got != 2 {
		t.Errorf("UPDATEs = %d, want 2 (the failure and its retry)", got)
	}
}

// OCI `tags` IS EXCLUDED. Structurally it already is -- tags are read through
// StatUncached, which never traverses the LRU -- so this asserts the filter that keeps
// the exclusion true if some future caller reads a tag through Stat. A tag's freshness
// is updated_at, maintained by the SWR revalidation and read by sweep stage 7;
// accessed_at would be a second answer to the same question.
func TestToucherExcludesTheOCITagsNamespace(t *testing.T) {
	const window = time.Hour

	repo := newFakeReader()
	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, clk := newToucherService(t, repo, db)

	tag := testRef("docker.io/library/busybox:latest")
	tag.Namespace = nsTags

	other := testRef(sstateKey(1))
	other.Namespace = "cas"

	svc.MarkAccessed(tag)
	svc.MarkAccessed(other)

	clk.advance(2 * window)

	if _, err := svc.flushAccess(t.Context(), window, false); err != nil {
		t.Fatalf("flushAccess() error = %v", err)
	}

	calls := db.calls("TouchObjectsAccessed")
	if len(calls) != 1 {
		t.Fatalf("UPDATEs = %d, want 1 (cas only)", len(calls))
	}

	if ns, _ := calls[0].args[1].(string); ns == nsTags {
		t.Error("the toucher wrote accessed_at for an OCI tag")
	}
}

// The flusher parses the cache key back into its three parts, so the encoding and the
// parse have to agree exactly -- a namespace of "" (sstate and downloads) is the case
// that breaks a naive split.
func TestSplitCacheKeyRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		ref  Ref
	}{
		{name: "empty namespace", ref: Ref{BackendID: 1, Namespace: "", Key: sstateKey(3)}},
		{name: "cas", ref: Ref{BackendID: 4096, Namespace: "cas", Key: "e3b0c44298fc1c14"}},
		{name: "multi-segment key", ref: Ref{BackendID: 7, Namespace: "sccache", Key: "a/b/c/deadbeef"}},
		{name: "large backend id", ref: Ref{BackendID: 9007199254740993, Namespace: "ac-grpc", Key: "k"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ck := string(tc.ref.appendCacheKey(nil))

			backendID, namespace, key, ok := splitCacheKey(ck)
			if !ok {
				t.Fatalf("splitCacheKey(%q) failed", ck)
			}

			if backendID != tc.ref.BackendID || namespace != tc.ref.Namespace || key != tc.ref.Key {
				t.Errorf("splitCacheKey() = %d, %q, %q; want %d, %q, %q",
					backendID, namespace, key, tc.ref.BackendID, tc.ref.Namespace, tc.ref.Key)
			}
		})
	}
}
