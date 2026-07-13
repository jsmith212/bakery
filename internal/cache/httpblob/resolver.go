package httpblob

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// defaultRouteTTL bounds how long a resolved route -- INCLUDING its ReadAuthRequired
// and Enabled flags -- may keep being served from cache after the control plane rewrites
// the backend row. It converts an auth-policy change (a public mirror flipped private
// after a leak, a backend disabled on decommission) from "stale until the process
// restarts" -- a world-readable "private" cache indefinitely -- into "stale for at most
// this long". The control plane should ALSO call Invalidate for immediate effect (that
// wiring lives in internal/api / internal/server and is a follow-up); this TTL is the
// backstop that bounds the window even when that call is absent. It is comfortably longer
// than one build's BB_NUMBER_THREADS HEAD storm -- every HEAD for one org/project/kind
// shares a single routeKey, so one resolution absorbs the whole storm -- so it keeps the
// route layer off the hot path while never pinning a stale auth decision forever.
const defaultRouteTTL = 30 * time.Second

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

// cachedRoute is a resolved route plus the instant it goes stale. The expiry is what
// keeps an auth-policy change (ReadAuthRequired, Enabled) from being served indefinitely
// out of cache after the control plane rewrites the backend row.
type cachedRoute struct {
	route   cache.Route
	expires time.Time
}

// CachedResolver fronts ResolveRoute + GetBackend (two cold index probes) with an
// in-process cache. The boot advisory lock refuses a second instance, which is what
// makes an in-process cache sound: exactly one process writes this database.
//
// It caches POSITIVE resolutions only, each with a TTL. A miss (org/project/backend
// absent) is not cached, so a backend created after its first probe takes effect
// immediately rather than being pinned absent. A resolution that later changes -- a
// backend disabled, deleted, or flipped between public and private through the API --
// is served from cache for at most ttl before it is re-read from the database.
// Invalidate forces that re-read immediately; wiring it into the control-plane
// mutations is a follow-up, and the TTL is the backstop until then.
type CachedResolver struct {
	store RouteStore
	log   *slog.Logger
	ttl   time.Duration
	now   func() time.Time // injectable clock; time.Now in production, fixed in tests

	mu    sync.RWMutex
	cache map[routeKey]cachedRoute
}

// NewCachedResolver builds a resolver over store.
func NewCachedResolver(store RouteStore, log *slog.Logger) *CachedResolver {
	if log == nil {
		log = slog.Default()
	}

	return &CachedResolver{
		store: store,
		log:   log,
		ttl:   defaultRouteTTL,
		now:   time.Now,
		cache: make(map[routeKey]cachedRoute),
	}
}

// Resolve returns the cache.Route for (org, project, kind), or ok=false when it does
// not exist. A cache hit issues zero queries.
func (c *CachedResolver) Resolve(
	ctx context.Context, org, project string, kind repository.BackendKind,
) (cache.Route, bool) {
	k := routeKey{org: org, project: project, kind: kind}
	now := c.now()

	c.mu.RLock()
	ent, ok := c.cache[k]
	c.mu.RUnlock()

	// A cached entry is only good until it expires. A stale entry is treated exactly
	// like a miss: re-read from the database so an auth-policy change (a cache flipped
	// private, a backend disabled) cannot outlive the TTL. Failing the re-read renders
	// 404 (load returns ok=false), which is the fail-safe direction -- 404 a build's HEAD
	// rather than serve a possibly-private object on a stale open route.
	if ok && now.Before(ent.expires) {
		return ent.route, true
	}

	route, ok := c.load(ctx, org, project, kind)
	if !ok {
		return cache.Route{}, false
	}

	c.mu.Lock()
	c.cache[k] = cachedRoute{route: route, expires: now.Add(c.ttl)}
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
