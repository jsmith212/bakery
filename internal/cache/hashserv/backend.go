package hashserv

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// RouteResolver turns the {org}/{project} wildcards into a resolved cache.Route. It returns
// ok=false when the org, project or backend does not exist.
type RouteResolver interface {
	Resolve(ctx context.Context, org, project string, kind repository.BackendKind) (cache.Route, bool)
}

// upstreamProvider hands out the upstream client and backfill worker for one backend, or
// (nil, nil) when chaining is off for it. Chaining is per-backend (a config key) with a
// server-wide kill switch, so "off" is the common case and must cost nothing.
type upstreamProvider interface {
	For(ctx context.Context, route cache.Route) (upstreamLookup, backfiller)
}

// Backend is the hashserv cache backend: a WebSocket upgrade on the shared mux.
//
// It implements cache.StreamBackend rather than plain cache.Backend because its connection is
// long-lived and bidirectional, which exempts it from things that are right for every other
// route and fatal here -- a write timeout, a response-buffering middleware, a body-size limit.
//
// It is also the ONE backend that does not route through blob.Service: it stores hash rows,
// not objects. That is why it carries its own Queries (a narrow, hashserv-only surface -- see
// store.go) and owns its own metrics.
type Backend struct {
	deps      cache.Deps
	routes    RouteResolver
	authn     Authenticator
	q         Queries
	upstreams upstreamProvider
}

// New builds the hashserv backend. upstreams may be nil, which disables chaining entirely.
func New(deps cache.Deps, routes RouteResolver, authn Authenticator, q Queries, upstreams upstreamProvider) *Backend {
	return &Backend{deps: deps, routes: routes, authn: authn, q: q, upstreams: upstreams}
}

// Kind reports the DB enum this backend serves.
func (b *Backend) Kind() repository.BackendKind { return repository.BackendKindHashserv }

// Register mounts the WebSocket endpoint.
//
// "hashserv" is a LITERAL 4th segment, like "sstate" and "downloads". A wildcard {kind} there
// alongside sstate's {path...} panics at registration.
//
// No method is named: the upgrade is a GET, but the handler does its own check, and pinning
// GET here would make the route collide with the SPA catch-all in the way the M1 notes warn
// about.
func (b *Backend) Register(mux *http.ServeMux) {
	mux.Handle("/cache/{org}/{project}/hashserv", http.HandlerFunc(b.serveHTTP))
}

func (b *Backend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := b.routes.Resolve(r.Context(), r.PathValue("org"), r.PathValue("project"),
		repository.BackendKindHashserv)
	if !ok || !route.Enabled {
		// An unconfigured or disabled backend is a 404, exactly as an unconfigured sstate mount
		// is. There is nothing here to serve, and saying so plainly is honest.
		//
		// Note what this is NOT: it is not an auth denial. bitbake will turn this into a
		// ConnectionError, warn, and build on with unihash = taskhash -- a degraded but
		// correct build, which is the right outcome for a mount that does not exist. An auth
		// denial must NOT take this path; it goes in-band, so the build HALTS. See ServeStream.
		http.NotFound(w, r)

		return
	}

	b.ServeStream(w, r, route)
}

// ServeStream takes over the connection for its lifetime.
//
// # There is no authentication here, and that is the point
//
// A stock bitbake client sends NO Authorization header on the WebSocket upgrade
// (asyncrpc/client.py calls websockets.connect(uri, ping_interval=None) with no headers).
// Credentials travel IN BAND, in the `auth` RPC, after the upgrade has completed. So:
//
//   - Gating the upgrade on a credential rejects the connection before the client has had any
//     chance to present one. It cannot work.
//   - Answering 401 makes it worse than not working: the Python client raises ConnectionError,
//     bb.siggen CATCHES it, warns, and completes the build with unihash = taskhash. The build
//     goes green with a silently degraded cache.
//
// So we accept every upgrade to a configured backend, and deny in band, where a denial is an
// invoke-error that HALTS the build. The connection opens with whatever an anonymous caller is
// entitled to -- nothing at all when read_auth_required is set -- and the first RPC that needs
// more than that is refused, loudly, unless an `auth` RPC has arrived first.
func (b *Backend) ServeStream(w http.ResponseWriter, r *http.Request, route cache.Route) {
	ctx := r.Context()
	log := b.deps.Logger

	c, err := accept(w, r)
	if err != nil {
		log.InfoContext(ctx, "hashserv: upgrade failed",
			slog.String("org", route.Org), slog.String("project", route.Project),
			slog.Any("error", err))

		return
	}

	defer func() { _ = c.CloseNow() }()

	rec := b.deps.Metrics.Hashserv(route.Org, route.Project)
	rec.ConnOpened()

	defer rec.ConnClosed()

	var (
		up upstreamLookup
		bf backfiller
	)

	if b.upstreams != nil {
		up, bf = b.upstreams.For(ctx, route)
	}

	s := &session{
		conn:  wsConn{c: c},
		store: store{q: b.q, backendID: route.BackendID},
		up:    up,
		bf:    bf,
		authn: b.authn,
		route: route,
		rec:   rec,
		log:   log,

		// The connection starts with exactly what an anonymous caller gets: permRead on an open
		// mirror, nothing at all otherwise. A successful `auth` REPLACES this.
		perms: grant(nil, route),
	}

	// One goroutine, start to finish. The protocol has no request ids, so responses must be
	// strictly ordered -- and the cheapest way to guarantee that is to make concurrency
	// impossible rather than to coordinate it.
	if err := s.serve(ctx); err != nil && !isExpectedEnd(err) {
		log.InfoContext(ctx, "hashserv: connection ended",
			slog.String("org", route.Org), slog.String("project", route.Project),
			slog.Any("error", err))
	}
}

// isExpectedEnd reports whether an error is just a build finishing and going away. Every
// connection ends in an error of some kind; almost all of them are this one.
func isExpectedEnd(err error) bool {
	return errors.Is(err, errClosed) || errors.Is(err, context.Canceled)
}
