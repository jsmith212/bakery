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

	deps := cache.Deps{Blobs: &blob.Service{}, Metrics: metrics.New(), Logger: discardLogger()}

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

	return &touchFixture{t: t, pool: pool, route: route, b: b, srv: srv}
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
