package httpblob

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/db/repository"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRouteStore counts the two cold probes so the cache behavior can be asserted:
// a cache HIT must issue ZERO queries.
type fakeRouteStore struct {
	resolveCalls atomic.Int64
	backendCalls atomic.Int64

	resolveErr error
	backendErr error

	row     repository.ResolveRouteRow
	backend repository.GetBackendRow
}

func (f *fakeRouteStore) ResolveRoute(
	_ context.Context, _ repository.ResolveRouteParams,
) (repository.ResolveRouteRow, error) {
	f.resolveCalls.Add(1)

	if f.resolveErr != nil {
		return repository.ResolveRouteRow{}, f.resolveErr
	}

	return f.row, nil
}

func (f *fakeRouteStore) GetBackend(
	_ context.Context, _ repository.GetBackendParams,
) (repository.GetBackendRow, error) {
	f.backendCalls.Add(1)

	if f.backendErr != nil {
		return repository.GetBackendRow{}, f.backendErr
	}

	return f.backend, nil
}

func newHitStore() *fakeRouteStore {
	return &fakeRouteStore{
		row:     repository.ResolveRouteRow{ProjectID: uuid(0x1a), OrgID: uuid(0x0a)},
		backend: repository.GetBackendRow{ID: 7, Enabled: true, ReadAuthRequired: true, Config: []byte(`{}`)},
	}
}

// TestResolveCachesPositive: the first Resolve issues both probes; a second, identical
// Resolve issues NONE. That zero-query property is what the boot advisory lock buys and
// what keeps the route layer off the sstate HEAD storm.
func TestResolveCachesPositive(t *testing.T) {
	store := newHitStore()
	r := NewCachedResolver(store, nil)

	route, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)
	if !ok {
		t.Fatal("first Resolve = not found, want found")
	}

	if route.BackendID != 7 || !route.Enabled || !route.ReadAuthRequired {
		t.Errorf("route = %+v, want backend 7 enabled read-auth", route)
	}

	if route.Org != "acme" || route.Project != "widget" || route.Kind != repository.BackendKindSstate {
		t.Errorf("route slugs/kind = %q/%q/%q", route.Org, route.Project, route.Kind)
	}

	if _, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate); !ok {
		t.Fatal("second Resolve = not found")
	}

	if got := store.resolveCalls.Load(); got != 1 {
		t.Errorf("ResolveRoute called %d times, want 1 (second lookup must be a cache hit)", got)
	}

	if got := store.backendCalls.Load(); got != 1 {
		t.Errorf("GetBackend called %d times, want 1", got)
	}
}

// TestResolveMissNotCached: an absent org/project (ErrNoRows) is NOT cached, so a
// backend created after its first probe takes effect immediately rather than staying
// pinned absent.
func TestResolveMissNotCached(t *testing.T) {
	store := &fakeRouteStore{resolveErr: pgx.ErrNoRows}
	r := NewCachedResolver(store, nil)

	if _, ok := r.Resolve(t.Context(), "ghost", "widget", repository.BackendKindSstate); ok {
		t.Fatal("Resolve of an absent route = found, want not found")
	}

	if _, ok := r.Resolve(t.Context(), "ghost", "widget", repository.BackendKindSstate); ok {
		t.Fatal("second Resolve = found")
	}

	if got := store.resolveCalls.Load(); got != 2 {
		t.Errorf("ResolveRoute called %d times, want 2 (a miss must not be cached)", got)
	}
}

// TestResolveBackendMissNotCached: the org/project resolve, but the backend kind does
// not exist -> not found, not cached.
func TestResolveBackendMissNotCached(t *testing.T) {
	store := newHitStore()
	store.backendErr = pgx.ErrNoRows

	r := NewCachedResolver(store, nil)

	if _, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindDownloads); ok {
		t.Fatal("Resolve of an absent backend = found, want not found")
	}

	if got := store.backendCalls.Load(); got != 1 {
		t.Errorf("GetBackend called %d times, want 1", got)
	}
}

// TestInvalidate drops the cached entry so the next Resolve re-queries -- the hook the
// control plane will call when a backend is enabled/disabled or deleted.
func TestInvalidate(t *testing.T) {
	store := newHitStore()
	r := NewCachedResolver(store, nil)

	r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)
	r.Invalidate("acme", "widget")
	r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)

	if got := store.resolveCalls.Load(); got != 2 {
		t.Errorf("ResolveRoute called %d times, want 2 (Invalidate must force a re-query)", got)
	}
}

// TestResolveTTLExpiresStaleRoute: a control-plane change to a backend's ReadAuthRequired
// must not be served out of cache forever. A public mirror flipped private (the
// post-leak / offboarding case) is served stale only until the TTL, after which the next
// Resolve re-reads the row and reflects the new, private policy. Without a TTL the stale
// open route -- ReadAuthRequired=false -- would keep the "private" cache world-readable
// until the process restarts.
func TestResolveTTLExpiresStaleRoute(t *testing.T) {
	store := newHitStore()
	store.backend.ReadAuthRequired = false // a public mirror

	r := NewCachedResolver(store, nil)

	clock := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return clock }

	route, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)
	if !ok || route.ReadAuthRequired {
		t.Fatalf("first Resolve ok=%v ReadAuthRequired=%v, want ok=true public", ok, route.ReadAuthRequired)
	}

	// The admin makes the cache private (e.g. after a leak). The DB row now says so.
	store.backend.ReadAuthRequired = true

	// Within the TTL the change is not yet visible -- the bounded staleness window.
	clock = clock.Add(defaultRouteTTL - time.Second)

	route, _ = r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)
	if route.ReadAuthRequired {
		t.Fatal("within the TTL the route re-queried early; the cache should still be serving the prior entry")
	}

	if got := store.backendCalls.Load(); got != 1 {
		t.Errorf("within TTL GetBackend called %d times, want 1 (still a cache hit)", got)
	}

	// Past the TTL the entry is stale and must be re-read -- the flip to private now takes
	// effect. This is the assertion that goes red if the TTL expiry is removed.
	clock = clock.Add(2 * time.Second)

	route, ok = r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate)
	if !ok {
		t.Fatal("post-TTL Resolve = not found, want found")
	}

	if !route.ReadAuthRequired {
		t.Error("after the TTL a cache flipped private still resolved ReadAuthRequired=false: " +
			"the private cache stayed world-readable")
	}

	if got := store.backendCalls.Load(); got != 2 {
		t.Errorf("post-TTL GetBackend called %d times, want 2 (a stale entry must be re-read)", got)
	}
}

// TestResolveDBErrorIsNotFound: a non-ErrNoRows failure still renders not-found (the
// RouteResolver interface has no error channel), and it is not cached.
func TestResolveDBErrorIsNotFound(t *testing.T) {
	store := &fakeRouteStore{resolveErr: errors.New("connection refused")}
	r := NewCachedResolver(store, discardLogger())

	if _, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate); ok {
		t.Fatal("Resolve on a DB error = found, want not found")
	}

	if _, ok := r.Resolve(t.Context(), "acme", "widget", repository.BackendKindSstate); ok {
		t.Fatal("second Resolve = found")
	}

	if got := store.resolveCalls.Load(); got != 2 {
		t.Errorf("ResolveRoute called %d times, want 2 (an error must not be cached)", got)
	}
}
