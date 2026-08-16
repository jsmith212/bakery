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
	"strings"
	"sync"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/gc"
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
	// GC triggers M6 sweeps for POST /api/v1/gc/run (spec §9.10). The two listing
	// routes read gc_runs straight off Store; only the trigger needs the running
	// engine itself, to answer 409 the instant a second real run is attempted
	// rather than after writing a row that would violate gc_runs' partial unique
	// index.
	GC  *gc.Engine
	Log *slog.Logger

	// AllowSelfServeOrgs lets ANY signed-in human create an organization (and become
	// its local owner). Off restricts creation to site admins.
	//
	// The zero value is OFF, which is the restrictive one: a caller who forgets this
	// field gets the M1 behaviour, not an open door. The PRODUCT default is on, and
	// it lives where a deployment decision belongs -- the Kong flag
	// (--allow-self-serve-orgs / ALLOW_SELF_SERVE_ORGS), which defaults to true.
	AllowSelfServeOrgs bool

	// AllowLocalSiteAdmins lets a site admin grant ANOTHER user the site-admin role
	// in-app, recorded with provenance. Off closes that path for everyone -- site
	// admins included -- so the platform-admin roster lives in the directory and
	// nowhere else.
	//
	// Same convention as above: the zero value is OFF (restrictive), and the product
	// default (on) lives on the Kong flag. It never gates REVOCATION of an existing
	// local grant, which is exactly what an operator needs on the day they turn it
	// off.
	AllowLocalSiteAdmins bool

	// Instance is B6's GET /instance body, resolved ONCE by server.Boot from the
	// same cmd/BootParams every other Config field above reads from, and served
	// back verbatim -- see instance.go's package doc for why this is a static
	// echo and never touches Prometheus.
	Instance InstanceInfo

	// ExternalURL is --external-url / EXTERNAL_URL: the server's public base URL.
	//
	// B1 (spec 2026-08-15). It is the FIRST term of the snippet generator's origin
	// precedence -- config, then X-Forwarded-*, then the request host -- because a
	// snippet carries a live credential and an operator who has already told us the
	// public origin should not be able to have it overridden by a header. Empty is
	// supported and normal for a direct-connection deployment.
	ExternalURL string

	// GRPCExternalEndpoint is --grpc-external-endpoint / GRPC_EXTERNAL_ENDPOINT: the
	// PUBLIC gRPC authority Bazel and moon should dial, e.g. "grpcs://bakery.corp:9092".
	//
	// Used VERBATIM when set. It exists because the REAPI listener is a separate
	// listener on a separate port (GRPCAddr) that may be exposed on a different host
	// or port than the console -- and neither the request nor GRPCAddr can tell us
	// what an ingress did with it.
	GRPCExternalEndpoint string

	// GRPCAddr is --grpc-addr / GRPC_ADDR, verbatim, and the snippet generator reads
	// exactly one thing off it: THE PORT. Deriving the gRPC endpoint from the HTTP
	// request's port instead (what M4 shipped) emits an endpoint nothing listens on
	// under EVERY configuration including a plain `bakery serve`, and moon's response
	// to an unreachable cache is to disable it silently. Empty means the REAPI
	// listener is off, which is a 409 on a bazel/moon snippet -- see grpcEndpoint.
	GRPCAddr string
}

// API is the control-plane API.
type API struct {
	store Store
	auth  authService
	keys  keyMinter
	// gc is the NARROWED gcTrigger, not *gc.Engine directly -- so a test can inject
	// a fake without a database or a real sweep. It may be nil (see New): a nil
	// Config.GC leaves POST /gc/run REGISTERED but refusing every call, rather
	// than failing every embedder that has not wired M6 yet. server.Boot always
	// wires a real engine.
	gc  gcTrigger
	log *slog.Logger

	// allowSelfServeOrgs: see Config.AllowSelfServeOrgs. Read only by
	// handleCreateOrg, and never written after New.
	allowSelfServeOrgs bool

	// allowLocalSiteAdmins: see Config.AllowLocalSiteAdmins. Read only by
	// handlePutSiteAdmin -- never by the revoke or the listing -- and never written
	// after New.
	allowLocalSiteAdmins bool

	// instance: see Config.Instance. Read only by handleGetInstance, and never
	// written after New -- a boot-time echo, not a live value.
	instance InstanceInfo

	// The three B1 origin inputs. Read only by snippets.go, never written after New.
	externalURL          string
	grpcExternalEndpoint string
	grpcAddr             string

	// warnGRPCOnce / warnExternalURLOnce mirror oci.Backend.warnRealmOnce: a
	// DERIVED public endpoint is a guess, and a guess that is wrong makes moon
	// silently disable its cache. It is worth exactly one log line per process --
	// per request would be a log flood on the highest-traffic misconfiguration.
	warnGRPCOnce        sync.Once
	warnExternalURLOnce sync.Once

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

	// cfg.GC is *gc.Engine, a concrete pointer -- assigning a nil one straight into
	// the gcTrigger interface field would produce a NON-nil interface wrapping a
	// nil pointer (Go's classic typed-nil trap), and a.gc == nil in the handler
	// would then never be true. This is the one place that distinction is made.
	var gcT gcTrigger
	if cfg.GC != nil {
		gcT = cfg.GC
	}

	return &API{
		store:                cfg.Store,
		auth:                 cfg.Auth,
		keys:                 serviceKeyMinter{svc: cfg.Auth},
		gc:                   gcT,
		log:                  log,
		allowSelfServeOrgs:   cfg.AllowSelfServeOrgs,
		allowLocalSiteAdmins: cfg.AllowLocalSiteAdmins,
		instance:             cfg.Instance,
		externalURL:          strings.TrimSpace(cfg.ExternalURL),
		grpcExternalEndpoint: strings.TrimSpace(cfg.GRPCExternalEndpoint),
		grpcAddr:             strings.TrimSpace(cfg.GRPCAddr),
		metrics:              cfg.Metrics,
		routes:               nil,
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

	// ---- me. The ONE route every credential kind may reach, so `bakery whoami`
	// works with a session, a key, a personal access token or a robot.
	a.route(mux, AccessAuthenticated, "GET "+p+"/me", a.handleMe)

	// ---- personal access tokens (`bkru_`).
	//
	// AccessUser: a verified HUMAN. A token that could list its owner's other tokens
	// would hand a leaked credential a map of every credential that human holds, and
	// one that could revoke them would hand it a denial of service -- neither needs a
	// capability to be dangerous, so the door is shut rather than the capability.
	//
	// No {user} segment: a token is always the caller's, and the queries carry
	// user_id in their own predicates so there is nothing to plumb another id into.
	a.route(mux, AccessUser, "GET "+p+"/user/tokens", a.handleListUserTokens)
	a.route(mux, AccessUser, "POST "+p+"/user/tokens", a.handleCreateUserToken)
	a.route(mux, AccessUser, "DELETE "+p+"/user/tokens/{token}", a.handleRevokeUserToken)

	// ---- site admins. HYBRID, like org membership: an OIDC half the reconciler owns
	// and a local half these routes own.
	//
	// All three are AccessSiteAdmin, which the guard admits no API key to at any
	// scope -- so an API-key principal can NEVER grant a site role. That is belt and
	// braces with the principal itself, whose IsSiteAdmin() is false for a key even
	// when the owning human is an admin. A delegation must not become a master key.
	//
	// GET reports the SOURCE of every admin, and that is the mitigation the whole
	// hybrid site role rests on, not a nicety: a local grant that outlives an LDAP
	// revocation is a backdoor precisely and only while it is invisible.
	//
	// There is no route that grants the FIRST site admin, and there cannot be -- it
	// would have to be reachable by someone who is not one. That is the CLI
	// break-glass (`bakery user site-admin`), which needs DB_URL and not a session.
	a.route(mux, AccessSiteAdmin, "GET "+p+"/site-admins", a.handleListSiteAdmins)
	a.route(mux, AccessSiteAdmin, "PUT "+p+"/site-admins/{user}", a.handlePutSiteAdmin)
	a.route(mux, AccessSiteAdmin, "DELETE "+p+"/site-admins/{user}", a.handleDeleteSiteAdmin)

	// ---- organizations
	a.route(mux, AccessUserScoped, "GET "+p+"/orgs", a.handleListOrgs)
	// AccessUser, not AccessAuthenticated: creating an org grants the creator a local
	// OWNER role on it, so an API key that could reach this route would become the
	// owner of a brand-new tenant. AccessAuthenticated is the one level the guard
	// admits a key to, and this route must not sit on it.
	a.route(mux, AccessUser, "POST "+p+"/orgs", a.handleCreateOrg)
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}", a.handleGetOrg)
	a.route(mux, AccessOrgAdmin, "PATCH "+p+"/orgs/{org}", a.handleUpdateOrg)
	a.route(mux, AccessOrgOwner, "DELETE "+p+"/orgs/{org}", a.handleDeleteOrg)

	// ---- org memberships. HYBRID: the reconciler owns the OIDC half, these routes
	// own the local half, and the effective role is greatest(oidc, local), generated
	// by the database. PUT grants a local role; DELETE clears one -- and says so when
	// the membership survives it because a group claim still justifies the user.
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}/members", a.handleListOrgMembers)
	a.route(mux, AccessOrgAdmin, "PUT "+p+"/orgs/{org}/members/{user}", a.handlePutOrgMember)
	a.route(mux, AccessOrgAdmin, "DELETE "+p+"/orgs/{org}/members/{user}", a.handleDeleteOrgMember)

	// ---- robots: org-owned machine identities and their `bkro_` tokens.
	//
	// AccessOrgAdmin throughout, and the control-plane door (methodMayReach) admits
	// only an interactive human to that level -- so no token of any kind can manage
	// robots. {robot} is resolved SCOPED BY THE ORG inside the handlers, exactly as
	// the guard resolves {project}, so an id from another tenant 404s.
	a.route(mux, AccessOrgAdmin, "GET "+p+"/orgs/{org}/robots", a.handleListRobots)
	a.route(mux, AccessOrgAdmin, "POST "+p+"/orgs/{org}/robots", a.handleCreateRobot)
	a.route(mux, AccessOrgAdmin, "DELETE "+p+"/orgs/{org}/robots/{robot}", a.handleDeleteRobot)
	a.route(mux, AccessOrgAdmin, "POST "+p+"/orgs/{org}/robots/{robot}/tokens",
		a.handleCreateOrgToken)
	a.route(mux, AccessOrgAdmin, "DELETE "+p+"/orgs/{org}/robots/{robot}/tokens/{token}",
		a.handleRevokeOrgToken)

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
	//
	// The two MUTATING routes are AccessProjectCredential, not AccessProjectRead:
	// same capability floor (a reader may mint a read key), but a door no
	// credential may come through. A leaked personal access token could otherwise
	// list its owner's key ids here and DELETE each one -- no escalation, a clean
	// denial of service against every CI job in the org, logged as its owner. The
	// LISTING stays at ProjectRead: reading your own key metadata is what
	// `bakery key list` does.
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/keys", a.handleListKeys)
	a.route(mux, AccessProjectCredential, "POST "+p+"/orgs/{org}/projects/{project}/keys",
		a.handleCreateKey)
	a.route(mux, AccessProjectCredential, "DELETE "+p+"/orgs/{org}/projects/{project}/keys/{key}",
		a.handleRevokeKey)

	// ---- config-snippet generator. DESIGN.md's highest-value screen: it mints a key
	// and emits a ready-to-paste client config with it embedded. AccessProjectCredential
	// for the same reason as the mint above -- a project READER passes the capability
	// check and gets a read snippet; the write-scope cap lives in auth.CreateAPIKey --
	// and because a route that mints a credential must not be reachable BY one.
	a.route(mux, AccessProjectCredential, "POST "+p+"/orgs/{org}/projects/{project}/snippet",
		a.handleGenerateSnippet)

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

	// ---- GC (M6, spec §9.10). AccessSiteAdmin, the same level as site-admins
	// above and for the same reason: a sweep can delete a tenant's cache
	// wholesale, so triggering one is at least as dangerous as granting a site
	// role, and the guard admits no API-key principal to this level at all. The
	// two listing routes read gc_runs straight off Store; the trigger route needs
	// the running engine itself (see gcTrigger) to answer 409 without first
	// writing a row that would violate gc_runs' partial unique index.
	a.route(mux, AccessSiteAdmin, "GET "+p+"/gc/runs", a.handleListGCRuns)
	a.route(mux, AccessSiteAdmin, "GET "+p+"/gc/runs/{id}", a.handleGetGCRun)
	a.route(mux, AccessSiteAdmin, "POST "+p+"/gc/run", a.handleTriggerGCRun)

	// ---- GC org visibility (B7, spec 2026-08-15). OrgView, deliberately BELOW
	// AccessSiteAdmin above: this is the org's own retention history, read-only,
	// scoped to its own projects by the query's own join -- the instance-wide
	// runs list and the trigger stay site-admin-only.
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}/gc/activity", a.handleGetOrgGCActivity)

	// ---- usage (B2, first readers of cache_backend_usage). OrgView / ProjectRead,
	// matching the projects/backends routes they sit beside.
	a.route(mux, AccessOrgView, "GET "+p+"/orgs/{org}/usage", a.handleGetOrgUsage)
	a.route(mux, AccessProjectRead, "GET "+p+"/orgs/{org}/projects/{project}/usage",
		a.handleGetProjectUsage)

	// ---- object browser (B3). ProjectRead, the same floor as the backend detail
	// route it extends. {kind} is a LITERAL-adjacent path segment, not a second
	// {kind}-shaped wildcard alongside backends/{kind} above -- ServeMux is fine
	// with a longer, more specific pattern sharing a prefix with a shorter one.
	a.route(mux, AccessProjectRead,
		"GET "+p+"/orgs/{org}/projects/{project}/backends/{kind}/objects", a.handleListCacheObjects)

	// ---- instance (B6). SiteAdmin: a boot-config echo, never Prometheus.
	a.route(mux, AccessSiteAdmin, "GET "+p+"/instance", a.handleGetInstance)
}
