// Package api is the control-plane REST API at /api/v1: organizations, projects,
// memberships, API keys, cache-backend configs, the current principal, and the
// OIDC login endpoints.
//
// # Authorization is structural, not remembered
//
// Two rules carry the whole security story of this package, and both are enforced
// by the shape of the code rather than by review:
//
//  1. A route cannot be registered without stating its required role. `route` is
//     the only function that touches the mux, the mux is unexported, and `route`
//     takes an Access as a required positional argument. "Someone added an
//     endpoint and forgot the authorization check" is a compile error here.
//
//  2. A handler never sees a path identifier. `guard` resolves {org} and
//     {project} from slugs to database ids, checks them against the caller's
//     roles, and puts the result in the context. Handlers read `scopeFrom(ctx)`.
//     The classic IDOR -- read an id from the path, load the object, forget to ask
//     whether this caller may have it -- is not expressible, because the id in the
//     path is not available to the handler at all.
//
// Everything else (the error envelope, the JSON content-type CSRF gate, the
// metrics labels) is likewise centralised in the guard and in writeError, so a new
// endpoint inherits it rather than reimplementing it.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Prefix is the mount point. Every pattern below is registered with it, so
// r.Pattern -- which is what the metrics middleware labels on -- reads as the full
// route.
const Prefix = "/api/v1"

// Config is what the API needs. All fields are required except Log.
type Config struct {
	// Store is the control-plane repository. Narrow, consumer-side (see store.go).
	Store Store
	// Auth is the auth service: it authenticates requests and mints API keys.
	Auth *auth.Service
	// Metrics supplies the HTTP middleware. Labels are on r.Pattern -- never
	// r.URL.Path, which would mint a time series per org/project/key id.
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// API is the control-plane API.
type API struct {
	store Store
	auth  authService
	keys  keyMinter
	log   *slog.Logger

	metrics *metrics.Metrics

	// routes is the registered table, kept for TestEveryRouteDeclaresAnAccess and
	// for the authorization-matrix test's coverage assertion.
	routes []routeSpec
}

// authService is the slice of *auth.Service this package uses. It exists so the
// dependency is explicit and reviewable, not so it can be faked -- Authenticate
// returns an auth.Principal, which no test can construct. Tests inject a fake
// Principal into the context directly and never run authenticate().
type authService interface {
	Authenticate(ctx context.Context, r *http.Request) (auth.Principal, error)
	HandleAuthConfig(w http.ResponseWriter, r *http.Request)
	HandleLogin(w http.ResponseWriter, r *http.Request)
	HandleCallback(w http.ResponseWriter, r *http.Request)
	HandleLogout(w http.ResponseWriter, r *http.Request)
	HandleDevLogin(w http.ResponseWriter, r *http.Request)
	DevLoginEnabled() bool
}

// New builds the API.
func New(cfg Config) (*API, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: Config.Store is required")
	}

	if cfg.Auth == nil {
		return nil, errors.New("api: Config.Auth is required")
	}

	if cfg.Metrics == nil {
		return nil, errors.New("api: Config.Metrics is required")
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	return &API{
		store:   cfg.Store,
		auth:    cfg.Auth,
		keys:    serviceKeyMinter{svc: cfg.Auth},
		log:     log,
		metrics: cfg.Metrics,
		routes:  nil,
	}, nil
}

// routeSpec is one row of the route table.
type routeSpec struct {
	Access  Access
	Pattern string
}

// Handler returns the /api/v1 subtree, ready to mount on the root mux:
//
//	root.Handle(api.Prefix+"/", a.Handler())
//
// The chain is, outermost first:
//
//	metrics.HTTPMiddleware  -- reads r.Pattern AFTER the inner mux has set it
//	auth.Service.LoadAndSave -- scs; MUST be on this subtree only, never the root
//	                            mux (it does a DB Find per cookie-bearing request,
//	                            adds Vary: Cookie, and its writer drops
//	                            io.ReaderFrom, killing sendfile on blob responses)
//	authenticate            -- bridges auth.Principal into this package's context
//	mux                     -- sets r.Pattern
//	guard                   -- authorization; see authz.go
//	handler
//
// The ordering of the first two is what makes the Prometheus labels correct.
// ServeMux mutates r.Pattern IN PLACE on the request it dispatches, so a
// middleware that captured that same *http.Request before the mux ran sees the
// matched pattern once next.ServeHTTP returns. Label on r.URL.Path instead and you
// mint one time series per org, project and key id.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	a.mount(mux)

	// LoadAndSave must be the Service's, not the raw scs one: it marks the context
	// so Authenticate can tell "no session here" from "this context never went
	// through scs", which otherwise panics.
	return a.metrics.HTTPMiddleware(
		a.authSvc().LoadAndSave(
			a.authenticate(mux),
		),
	)
}

// authSvc narrows back to the concrete service for the one thing the interface
// cannot express (LoadAndSave returns an http.Handler wrapper tied to scs).
func (a *API) authSvc() *auth.Service {
	svc, ok := a.auth.(*auth.Service)
	if !ok {
		// Only reachable if a test built an API with a fake authService and then
		// called Handler(). Tests exercise the mux directly instead.
		panic("api: Handler requires the real *auth.Service")
	}

	return svc
}

// route is the ONLY way a handler reaches the mux.
//
// Access is a required positional argument, so a new endpoint cannot be added
// without answering "who may call this?". That is the whole design: the check is
// not something to remember, it is something the compiler demands.
func (a *API) route(mux *http.ServeMux, access Access, pattern string, h handlerFunc) {
	a.routes = append(a.routes, routeSpec{Access: access, Pattern: pattern})
	mux.HandleFunc(pattern, a.guard(access, h))
}

// raw registers a handler that writes its own response: the OIDC endpoints, which
// live in internal/auth and redirect rather than return JSON. They are still
// declared with an Access, still go through the guard, and every one of them is
// AccessPublic by nature -- you cannot be logged in while logging in.
func (a *API) raw(mux *http.ServeMux, access Access, pattern string, h http.HandlerFunc) {
	a.route(mux, access, pattern, func(w http.ResponseWriter, r *http.Request) error {
		h(w, r)

		return nil
	})
}

// mount registers the route table. This function IS the authorization policy of
// the control plane; read it as a table.
func (a *API) mount(mux *http.ServeMux) {
	p := Prefix

	// ---- auth. Public by necessity: you cannot be authenticated while
	// authenticating. /auth/config carries no secret -- see handleAuthConfig.
	a.raw(mux, AccessPublic, "GET "+p+"/auth/config", a.auth.HandleAuthConfig)
	a.raw(mux, AccessPublic, "GET "+p+"/auth/login", a.auth.HandleLogin)
	a.raw(mux, AccessPublic, "GET "+p+"/auth/callback", a.auth.HandleCallback)
	a.raw(mux, AccessPublic, "POST "+p+"/auth/logout", a.auth.HandleLogout)

	// dev-login is registered ONLY when the flag is on. When it is off the route
	// does not exist, so the mux 404s it -- there is nothing to probe and nothing
	// to flip. A 403 would confirm the endpoint exists and tell a scanner exactly
	// what to come back for. The flag itself is env/CLI only; no API or UI path can
	// set it, and this is a read of it, never a write.
	if a.auth.DevLoginEnabled() {
		a.raw(mux, AccessPublic, "POST "+p+"/auth/dev-login", a.auth.HandleDevLogin)
	}

	// ---- me
	a.route(mux, AccessAuthenticated, "GET "+p+"/me", a.handleMe)

	// ---- organizations
	a.route(mux, AccessAuthenticated, "GET "+p+"/orgs", a.handleListOrgs)
	a.route(mux, AccessSiteAdmin, "POST "+p+"/orgs", a.handleCreateOrg)
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}", a.handleGetOrg)
	a.route(mux, AccessOrgAdmin, "PATCH "+p+"/orgs/{org}", a.handleUpdateOrg)
	a.route(mux, AccessOrgOwner, "DELETE "+p+"/orgs/{org}", a.handleDeleteOrg)

	// ---- org memberships. READ-ONLY: org roles are reconciled from OIDC group
	// claims on every login, so a hand-edit here is either reverted at the user's
	// next login (a lie that looks like it worked) or, worse, grants authority the
	// IdP never granted and that survives until then. The write routes exist and
	// return 409 with the reason, rather than 404 -- a 404 would leave an operator
	// hunting for the endpoint they are sure should be there.
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}/members", a.handleListOrgMembers)
	a.route(mux, AccessOrgAdmin, "PUT "+p+"/orgs/{org}/members/{user}", a.handleOrgMemberImmutable)
	a.route(mux, AccessOrgAdmin, "DELETE "+p+"/orgs/{org}/members/{user}", a.handleOrgMemberImmutable)

	// ---- projects
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}/projects", a.handleListProjects)
	a.route(mux, AccessOrgAdmin, "POST "+p+"/orgs/{org}/projects", a.handleCreateProject)
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}", a.handleGetProject)
	a.route(mux, AccessProjectAdmin, "PATCH "+p+"/orgs/{org}/projects/{project}", a.handleUpdateProject)
	a.route(mux, AccessOrgAdmin, "DELETE "+p+"/orgs/{org}/projects/{project}", a.handleDeleteProject)

	// ---- project memberships. Managed IN-APP, freely editable by an authorized
	// caller -- the reconciler never touches project_memberships.
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/members",
		a.handleListProjectMembers)
	a.route(mux, AccessProjectAdmin, "PUT "+p+"/orgs/{org}/projects/{project}/members/{user}",
		a.handlePutProjectMember)
	a.route(mux, AccessProjectAdmin, "DELETE "+p+"/orgs/{org}/projects/{project}/members/{user}",
		a.handleDeleteProjectMember)

	// ---- API keys. Create returns the plaintext exactly once; nothing else ever
	// returns it, and the schema cannot even store it.
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/keys", a.handleListKeys)
	a.route(mux, AccessProjectRead, "POST "+p+"/orgs/{org}/projects/{project}/keys", a.handleCreateKey)
	a.route(mux, AccessProjectRead, "DELETE "+p+"/orgs/{org}/projects/{project}/keys/{key}",
		a.handleRevokeKey)

	// ---- cache backends. Config rows only; no backend serves traffic in M1.
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/backends",
		a.handleListBackends)
	a.route(mux, AccessProjectAdmin, "POST "+p+"/orgs/{org}/projects/{project}/backends",
		a.handleCreateBackend)
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/backends/{kind}",
		a.handleGetBackend)
	a.route(mux, AccessProjectAdmin, "PATCH "+p+"/orgs/{org}/projects/{project}/backends/{kind}",
		a.handleUpdateBackend)
	a.route(mux, AccessProjectAdmin, "DELETE "+p+"/orgs/{org}/projects/{project}/backends/{kind}",
		a.handleDeleteBackend)
}
