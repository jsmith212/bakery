package hashserv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// THE UNIHASH TOUCHER (spec 6.1's hashserv paragraph). hashserv_unihashes is the GC
// root, so what these tests protect is that a HIT here is observable to the §6.2
// veto immediately, and durable in accessed_at after a flush -- without ever
// disturbing the write barrier columns (created_at, live_xid) a touch must leave
// alone.

// touchFixture wraps newBackend's shape but also hands the test the underlying
// *Backend (for its unexported toucher) and the raw pool (for reading
// hashserv_unihashes columns no exported query surfaces).
type touchFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	route cache.Route
	b     *Backend
	srv   *httptest.Server
	m     *metrics.Metrics
}

func newTouchFixture(t *testing.T) *touchFixture {
	t.Helper()

	pool := dbtest.New(t)
	s := db.NewStore(pool)
	ctx := t.Context()

	org, err := s.CreateOrganization(ctx, repository.CreateOrganizationParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	project, err := s.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	backend, err := s.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID: project.ID, Kind: repository.BackendKindHashserv, Enabled: true,
		ReadAuthRequired: true, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	route := cache.Route{
		OrgID: org.ID, ProjectID: project.ID, Org: "acme", Project: "widget",
		BackendID: backend.ID, Kind: repository.BackendKindHashserv,
		Enabled: true, ReadAuthRequired: true, Config: []byte(`{}`),
	}

	m := metrics.New()
	deps := cache.Deps{Blobs: &blob.Service{}, Metrics: m, Logger: discardLogger()}

	authn := fakeAuthenticator{
		writeToken: {read: true, write: true},
	}

	b := New(deps, staticRoutes{route: route}, authn, s, nil)

	if b.touch == nil {
		t.Fatal("New() built a Backend with a nil toucher")
	}

	mux := http.NewServeMux()
	b.Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &touchFixture{t: t, pool: pool, route: route, b: b, srv: srv, m: m}
}

func (f *touchFixture) dial() *rawClient {
	f.t.Helper()

	return dial(f.t, f.srv)
}

// row is what the test asserts a touch did and did not do: accessed_at is the ONLY
// column TouchUnihashesAccessed's WHERE/SET may change; created_at and live_xid ARE
// the write barrier and must survive a touch untouched.
type unihashRow struct {
	accessedAt pgtype.Timestamptz
	createdAt  time.Time
	liveXid    int64
}

func (f *touchFixture) row(taskhash string) unihashRow {
	f.t.Helper()

	var r unihashRow

	err := f.pool.QueryRow(f.t.Context(),
		`SELECT accessed_at, created_at, live_xid FROM hashserv_unihashes
		  WHERE backend_id = $1 AND method = $2 AND taskhash = $3`,
		f.route.BackendID, testMethod, taskhash).Scan(&r.accessedAt, &r.createdAt, &r.liveXid)
	if err != nil {
		f.t.Fatalf("read hashserv_unihashes(%q): %v", taskhash, err)
	}

	return r
}

func (f *touchFixture) pending(taskhash string) bool {
	f.t.Helper()

	return f.b.touch.pending(f.route.BackendID, testMethod, taskhash)
}

// flushNow forces an unstaleness-guarded flush directly -- flush is unexported, but
// this file lives in package hashserv, unlike the bazel-side equivalent that has to
// go through StartAccessToucher's cancel dance from outside blob.
func (f *touchFixture) flushNow() {
	f.t.Helper()

	if _, err := f.b.touch.flush(f.t.Context(), time.Hour, true); err != nil {
		f.t.Fatalf("flush() error = %v", err)
	}
}

// seedReport inserts (taskhash, unihash, outhash) via a real `report` RPC, which is
// a WRITE and must never mark anything by itself -- store.insertUnihash is a fresh
// INSERT, not a read of a pre-existing row.
func seedReport(t *testing.T, c *rawClient, taskhash, outhash, unihash string) {
	t.Helper()

	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash, "outhash": outhash, "unihash": unihash,
	}})
	c.recv()
}

// A `get` HIT marks its row immediately (visible to the §6.2 veto before any flush),
// and the flush lands accessed_at in the database without moving created_at or
// live_xid -- the assertion CLAUDE.md and spec 6's write-barrier notes both demand of
// every accessed_at-only UPDATE.
func TestHashservGetHitMarksAndFlushesAccessedAt(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	before := f.row(taskhash1)
	if before.accessedAt.Valid {
		t.Fatal("accessed_at is already set right after report -- a write must never mark")
	}

	if f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = true before any get -- report must not mark")
	}

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
	c.recv()

	if !f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = false after a get HIT -- the veto has a hole")
	}

	f.flushNow()

	after := f.row(taskhash1)

	if !after.accessedAt.Valid {
		t.Fatal("accessed_at is still NULL after the flush")
	}

	if time.Since(after.accessedAt.Time) > time.Minute {
		t.Errorf("accessed_at = %v, not recent", after.accessedAt.Time)
	}

	if !after.createdAt.Equal(before.createdAt) {
		t.Errorf("created_at moved: %v -> %v -- the touch disturbed the write barrier", before.createdAt, after.createdAt)
	}

	if after.liveXid != before.liveXid {
		t.Errorf("live_xid moved: %d -> %d -- the touch disturbed the write barrier", before.liveXid, after.liveXid)
	}

	if f.pending(taskhash1) {
		t.Error("pending(taskhash1) = true after a successful flush")
	}
}

// A `get` MISS -- and its upstream write-through, which is a fresh INSERT -- must
// never mark: there is no pre-existing row to have been read.
func TestHashservGetMissDoesNotMark(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash2}})

	if got := c.recv(); got != "null" {
		t.Fatalf("get on a miss = %s, want null", got)
	}

	if f.pending(taskhash2) {
		t.Error("pending(taskhash2) = true after a MISS -- the miss/write-through path must never mark")
	}
}

// get-stream is the hot path, and it must mark exactly like plain `get` does: one
// pipelined line that hits an existing row lands a mark.
func TestHashservGetStreamHitMarks(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	c.sendJSON(map[string]any{"get-stream": nil})

	if got := c.recv(); got != `"ok"` {
		t.Fatalf("stream entry = %s, want the JSON-quoted \"ok\"", got)
	}

	c.send(testMethod + " " + taskhash1)

	if got := c.recv(); got != unihash1 {
		t.Fatalf("stream reply = %q, want %q", got, unihash1)
	}

	c.send("END")

	if got := c.recv(); got != "ok" {
		t.Fatalf("stream exit = %q, want the raw unquoted ok", got)
	}

	if !f.pending(taskhash1) {
		t.Error("pending(taskhash1) = false after a get-stream HIT")
	}

	f.flushNow()

	if !f.row(taskhash1).accessedAt.Valid {
		t.Error("accessed_at is still NULL after the flush")
	}
}

// get-outhash (with_unihash defaulting true) marks the (method, TASKHASH-OF-THE-ROW)
// pair the join actually read -- which is the outhash's OWN producing task, not
// whatever taskhash the caller happened to send (get-outhash ignores it for lookup).
func TestHashservGetOuthashHitMarks(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	// A caller-supplied taskhash that is NOT the outhash's own producer: get-outhash
	// must still resolve and mark taskhash1, not this one.
	c.sendJSON(map[string]any{"get-outhash": map[string]any{
		"method": testMethod, "outhash": outhash1, "taskhash": taskhash2,
	}})

	var row outhashResponse
	if err := json.Unmarshal([]byte(c.recv()), &row); err != nil {
		t.Fatalf("unmarshal get-outhash: %v", err)
	}

	if row.Taskhash != taskhash1 {
		t.Fatalf("get-outhash returned taskhash %q, want %q", row.Taskhash, taskhash1)
	}

	if !f.pending(taskhash1) {
		t.Error("pending(taskhash1) = false after a get-outhash HIT")
	}

	if f.pending(taskhash2) {
		t.Error("pending(taskhash2) = true -- get-outhash marked the CALLER's taskhash, not the row's own")
	}
}

// THE FINAL FLUSH ON SHUTDOWN. StartUnihashToucher must flush once more on the way
// out -- auth.Service's StartKeyToucher has no equivalent, and blob/toucher.go
// already records that omission as a bug, not a pattern to repeat here.
func TestHashservFinalFlushOnShutdown(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
	c.recv()

	if !f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = false before shutdown")
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	// An interval far longer than the test: the ONLY thing that can flush here is
	// the shutdown path.
	go func() {
		defer close(done)

		f.b.StartUnihashToucher(ctx, time.Hour, func() time.Duration { return time.Hour })
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartUnihashToucher did not return after its context was cancelled")
	}

	if !f.row(taskhash1).accessedAt.Valid {
		t.Fatal("accessed_at is still NULL after shutdown -- the final flush is missing")
	}

	if f.pending(taskhash1) {
		t.Error("pending(taskhash1) = true after the final flush landed")
	}
}

// N reads in T must cost one UPDATE, exactly as blob.Service's flusher: this is what
// keeps the toucher from writing accessed_at on every one-minute tick for a hot
// taskhash.
func TestHashservTouchFlushIsCoalesced(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	for range 25 {
		c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
		c.recv()
	}

	// Inside the staleness window nothing is due -- a flush must not error and must
	// not clear the pending mark it declined to write.
	n, err := f.b.touch.flush(f.t.Context(), time.Hour, false)
	if err != nil {
		t.Fatalf("flush() error = %v", err)
	}

	if n != 0 {
		t.Fatalf("flush() inside the staleness window touched %d rows, want 0", n)
	}

	if !f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = false after a flush that declined to write it")
	}

	f.flushNow()

	if f.row(taskhash1).accessedAt.Time.IsZero() {
		t.Error("accessed_at was never written by the eventual all=true flush")
	}
}

// touchFlushRowsTotal reads bakery_gc_touch_flush_rows_total straight off the
// registry, the same way internal/metrics' own tests do -- there is no exported
// counter-reading API, and prometheus/client_golang's testutil is not a dependency
// this package otherwise needs.
func touchFlushRowsTotal(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, fam := range families {
		if fam.GetName() != "bakery_gc_touch_flush_rows_total" {
			continue
		}

		var total float64

		for _, met := range fam.GetMetric() {
			total += met.GetCounter().GetValue()
		}

		return total
	}

	return 0
}

// R8#2(a): unihashToucher.marks is capped, and a mark refused past the cap costs
// exactly a touch -- never a wrong delete, and never an unbounded map. This is a pure
// in-memory test: no DB and no Backend, because mark()/droppedMarks() touch neither.
func TestUnihashToucherMarksCapDropsAndCounts(t *testing.T) {
	t.Parallel()

	tc := newUnihashToucher(nil, nil)

	for i := range unihashMarksCap {
		tc.mark(1, testMethod, taskhashOf(i))
	}

	if got := len(tc.marks); got != unihashMarksCap {
		t.Fatalf("len(marks) = %d, want %d (the cap should be exactly full)", got, unihashMarksCap)
	}

	if got := tc.droppedMarks(); got != 0 {
		t.Fatalf("droppedMarks() = %d before the cap was exceeded, want 0", got)
	}

	// One more, past the cap: refused, not silently grown past it.
	overflow := taskhashOf(unihashMarksCap)
	tc.mark(1, testMethod, overflow)

	if got := len(tc.marks); got != unihashMarksCap {
		t.Fatalf("len(marks) = %d after an over-cap mark, want %d unchanged", got, unihashMarksCap)
	}

	if got := tc.droppedMarks(); got != 1 {
		t.Fatalf("droppedMarks() = %d, want 1", got)
	}

	// The dropped mark cost a touch, never a wrong delete: it must not appear as
	// pending, because nothing was actually recorded for it.
	if tc.pending(1, testMethod, overflow) {
		t.Error("pending() = true for a mark the cap refused -- it was never recorded")
	}

	// A key already inside the map is unaffected by the cap: re-marking it (the
	// first-mark-wins no-op path) must not count as a drop.
	tc.mark(1, testMethod, taskhashOf(0))

	if got := tc.droppedMarks(); got != 1 {
		t.Fatalf("droppedMarks() = %d after re-marking an existing key, want still 1", got)
	}
}

// taskhashOf renders i as a distinct 64-hex string, hex-only per CLAUDE.md's
// unihash/taskhash rule.
func taskhashOf(i int) string {
	const hex = "0123456789abcdef"

	b := make([]byte, 64)
	for j := range b {
		b[j] = hex[0]
	}

	for j := 0; i > 0 && j < len(b); j++ {
		b[len(b)-1-j] = hex[i%16]
		i /= 16
	}

	return string(b)
}

// R8#2(b): a FAILED flush must keep its marks pending -- clearing them (or leaving
// them un-restored after the swap-based redesign) would drop the reads a transient
// database error swallowed, and the §6.2 veto would stop protecting them too. This
// exercises mergeBack directly: due left `marks` in the swap and never reached
// `flushing`'s clean exit, so it must come back.
func TestHashservToucherKeepsMarksWhenFlushFails(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
	c.recv()

	if !f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = false before the failed flush")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // already done: the query this flush issues must fail

	if _, err := f.b.touch.flush(cancelled, time.Hour, true); err == nil {
		t.Fatal("flush() error = nil, want the cancelled-context failure")
	}

	if !f.pending(taskhash1) {
		t.Fatal("pending(taskhash1) = false after a FAILED flush -- mergeBack dropped the mark")
	}

	if f.row(taskhash1).accessedAt.Valid {
		t.Fatal("accessed_at was written despite the flush failing")
	}

	// A live retry must succeed and durably clear it -- the failure must not have
	// left the toucher in a stuck state.
	f.flushNow()

	if !f.row(taskhash1).accessedAt.Valid {
		t.Fatal("accessed_at is still NULL after the retry")
	}

	if f.pending(taskhash1) {
		t.Error("pending(taskhash1) = true after a successful retry")
	}
}

// A: R6#1/R7#3, hashserv half. Backend.FlushNow must write accessed_at even for a
// mark taken an instant ago, well inside any staleness window a ramp would set --
// that is the entire reason it exists: the GC's write barrier is about to read
// accessed_at off the database row, and staleness=0 is what forces it there.
func TestBackendFlushNowForcesWriteInsideStalenessWindow(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	seedReport(t, c, taskhash1, outhash1, unihash1)

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
	c.recv()

	// Sanity check: an ordinary staleness-guarded flush declines this -- the mark is
	// brand new -- so the contrast with FlushNow below is real.
	if n, err := f.b.touch.flush(f.t.Context(), time.Hour, false); err != nil || n != 0 {
		t.Fatalf("staleness-guarded flush() = (%d, %v), want (0, nil)", n, err)
	}

	n, err := f.b.FlushNow(f.t.Context())
	if err != nil {
		t.Fatalf("FlushNow() error = %v", err)
	}

	if n != 1 {
		t.Fatalf("FlushNow() touched %d rows, want 1", n)
	}

	if !f.row(taskhash1).accessedAt.Valid {
		t.Fatal("accessed_at is still NULL after FlushNow")
	}

	if f.pending(taskhash1) {
		t.Error("pending(taskhash1) = true after FlushNow durably wrote it")
	}
}

// FlushNow on a hand-built Backend (New() not called with a Queries, so b.touch is
// nil) must no-op rather than panic -- every toucher method is nil-safe for exactly
// this reason (backend.go's New doc comment).
func TestBackendFlushNowNilToucher(t *testing.T) {
	t.Parallel()

	deps := cache.Deps{Blobs: &blob.Service{}, Metrics: metrics.New(), Logger: discardLogger()}
	b := New(deps, staticRoutes{}, fakeAuthenticator{}, nil, nil)

	n, err := b.FlushNow(t.Context())
	if err != nil {
		t.Fatalf("FlushNow() on a nil toucher error = %v", err)
	}

	if n != 0 {
		t.Fatalf("FlushNow() on a nil toucher = %d, want 0", n)
	}
}

// R8#2(c)/O: flush() must report rows through the SAME GC touch-flush series
// blob.Service's flusher writes (bakery_gc_touch_flush_rows_total) -- it is the one
// place an operator can tell "the toucher is running and writing" from "running and
// writing nothing", combined across both touchers per that series' own doc comment.
func TestHashservToucherFlushReportsTouchFlushRowsMetric(t *testing.T) {
	t.Parallel()

	f := newTouchFixture(t)
	c := f.dial()
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	before := touchFlushRowsTotal(t, f.m)

	seedReport(t, c, taskhash1, outhash1, unihash1)

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})
	c.recv()

	f.flushNow()

	after := touchFlushRowsTotal(t, f.m)

	if after-before != 1 {
		t.Fatalf("bakery_gc_touch_flush_rows_total advanced by %v, want 1", after-before)
	}
}
