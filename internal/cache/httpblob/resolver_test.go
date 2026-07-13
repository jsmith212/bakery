package httpblob

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

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
