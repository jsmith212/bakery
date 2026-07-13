package httpblob

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// RouteStore is the CONSUMER-SIDE database surface the resolver needs: the two cold
// route-fill queries and nothing else. *db.Store satisfies it. Declaring it here keeps
// the resolver testable against a hand-written fake and makes its blast radius legible.
type RouteStore interface {
	ResolveRoute(ctx context.Context, arg repository.ResolveRouteParams) (repository.ResolveRouteRow, error)
	GetBackend(ctx context.Context, arg repository.GetBackendParams) (repository.GetBackendRow, error)
}

// routeKey is the in-process cache key: the resolved-route triple. Comparable, so it
// is a legal map key with no string building on the hot path.
type routeKey struct {
	org     string
	project string
	kind    repository.BackendKind
}

// CachedResolver fronts ResolveRoute + GetBackend (two cold index probes) with an
// in-process cache. The boot advisory lock refuses a second instance, which is what
// makes an in-process cache sound: exactly one process writes this database.
//
// It caches POSITIVE resolutions only. A miss (org/project/backend absent) is not
// cached, so a backend created after its first probe takes effect immediately rather
// than being pinned absent. A resolution that later changes -- a backend disabled or
// deleted through the API -- is stale until Invalidate is called or the process
// restarts; wiring Invalidate into the control-plane mutations is a follow-up.
type CachedResolver struct {
	store RouteStore
	log   *slog.Logger

	mu    sync.RWMutex
	cache map[routeKey]cache.Route
}

// NewCachedResolver builds a resolver over store.
func NewCachedResolver(store RouteStore, log *slog.Logger) *CachedResolver {
	if log == nil {
		log = slog.Default()
	}

	return &CachedResolver{
		store: store,
		log:   log,
		cache: make(map[routeKey]cache.Route),
	}
}

// Resolve returns the cache.Route for (org, project, kind), or ok=false when it does
// not exist. A cache hit issues zero queries.
func (c *CachedResolver) Resolve(
	ctx context.Context, org, project string, kind repository.BackendKind,
) (cache.Route, bool) {
	k := routeKey{org: org, project: project, kind: kind}

	c.mu.RLock()
	route, ok := c.cache[k]
	c.mu.RUnlock()

	if ok {
		return route, true
	}

	route, ok = c.load(ctx, org, project, kind)
	if !ok {
		return cache.Route{}, false
	}

	c.mu.Lock()
	c.cache[k] = route
	c.mu.Unlock()

	return route, true
}

// load does the two cold probes and assembles a Route. A genuinely absent org, project
// or backend (pgx.ErrNoRows) returns ok=false silently -- that is a 404, the normal
// answer for a route that does not exist. Any OTHER error is logged (the RouteResolver
// contract has no error return, so a DB outage still renders 404, but it must not do so
// silently) and also returns false.
func (c *CachedResolver) load(
	ctx context.Context, org, project string, kind repository.BackendKind,
) (cache.Route, bool) {
	rr, err := c.store.ResolveRoute(ctx, repository.ResolveRouteParams{Slug: org, Slug_2: project})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			c.log.ErrorContext(ctx, "resolve route", slog.String("org", org),
				slog.String("project", project), slog.Any("error", err))
		}

		return cache.Route{}, false
	}

	backend, err := c.store.GetBackend(ctx, repository.GetBackendParams{ProjectID: rr.ProjectID, Kind: kind})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			c.log.ErrorContext(ctx, "get backend", slog.String("org", org),
				slog.String("project", project), slog.String("kind", string(kind)), slog.Any("error", err))
		}

		return cache.Route{}, false
	}

	return cache.Route{
		OrgID:            rr.OrgID,
		ProjectID:        rr.ProjectID,
		Org:              org,
		Project:          project,
		BackendID:        backend.ID,
		Kind:             kind,
		Enabled:          backend.Enabled,
		ReadAuthRequired: backend.ReadAuthRequired,
		Config:           backend.Config,
	}, true
}

// Invalidate drops every cached kind for one project, so the next request re-reads its
// backend rows. The control-plane handlers that create, update, enable/disable or
// delete a project's backends should call it; that wiring is a follow-up.
func (c *CachedResolver) Invalidate(org, project string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.cache {
		if k.org == org && k.project == project {
			delete(c.cache, k)
		}
	}
}
