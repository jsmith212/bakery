package api

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Access is the authorization requirement a route carries.
//
// # Why this is a type and not a check inside each handler
//
// The registration function `route` takes an Access as a REQUIRED POSITIONAL
// ARGUMENT. There is no other way to add a route to this API -- the mux is not
// exported, `route` is the only thing that touches it, and it will not compile
// without an Access. So the failure mode "someone added an endpoint and forgot to
// authorize it" is not a code-review question, it is a compile error.
//
// The second half of the guarantee is that the handler never sees the raw path.
// A handler that does `id := r.PathValue("org")` and then loads that org is an
// IDOR, and it is an IDOR that reads as perfectly reasonable code. So the guard
// resolves the org/project slugs itself, checks them against the caller's roles,
// and hands the handler an already-authorized `scope` (resolved database ids).
// The handlers below never call PathValue for an org or a project, because by the
// time they run the answer is already in the context and already checked.
type Access int

const (
	// AccessPublic is a route with no principal at all: /auth/config, the OIDC
	// redirect and callback, dev-login. Anything else being Public is a bug.
	AccessPublic Access = iota
	// AccessAuthenticated needs a verified principal and nothing more (/me). It is
	// the ONLY level an API key may reach -- see guard.
	AccessAuthenticated
	// AccessUserScoped needs a principal that ACTS AS A USER: a session, a CLI
	// login, or a personal access token. Never a bare machine credential.
	//
	// It exists for routes whose whole response is derived from the caller's own
	// memberships -- GET /orgs is the one -- where a credential that IS its owner
	// gets a meaningful answer and a credential that is a GRANT gets an empty list
	// it had no business asking for. AccessAuthenticated would admit the latter,
	// and then "a robot may reach GET /api/v1/me and nothing else" would be false;
	// AccessUser would exclude the former, and then `bakery org list` would not
	// work with a personal access token, which is the friction that credential
	// exists to remove. Neither existing level says the right thing.
	AccessUserScoped
	// AccessUser needs a verified HUMAN principal: a session or a CLI login, never
	// an API key. Creating an organization is the one route at this level.
	//
	// It exists precisely so that route does not have to be AccessAuthenticated,
	// which is the level the guard lets an API key through. A key that could create
	// an org would receive a local OWNER grant on it (see handleCreateOrg), and a
	// delegation would have quietly become a master key -- with a fresh tenant to be
	// master of.
	AccessUser
	// AccessSiteAdmin is site-admin only.
	AccessSiteAdmin
	// AccessOrgView needs {org} and CanViewOrg.
	AccessOrgView
	// AccessOrgAdmin needs {org} and CanAdminOrg.
	AccessOrgAdmin
	// AccessOrgOwner needs {org} and CanOwnOrg: renaming or deleting the org.
	AccessOrgOwner
	// AccessProjectRead needs {org} and {project} and CanReadProject.
	AccessProjectRead
	// AccessProjectCredential needs exactly what AccessProjectRead needs --
	// {org}, {project}, CanReadProject -- and admits only a credential a HUMAN is
	// behind. It is the level the three CREDENTIAL-MANAGING project routes sit
	// on: POST .../keys, DELETE .../keys/{key}, POST .../snippet.
	//
	// It exists because the capability question and the door question have
	// different answers here, and the door's was wrong. A project reader may
	// legitimately mint a read-scoped key and generate a read snippet -- that is
	// the console's headline flow, and the write-scope cap lives in
	// auth.CreateAPIKey, not in the ladder -- so the capability floor is
	// deliberately ProjectRead. But `methodMayReach` opens ProjectRead to a
	// personal access token, and a PAT that can enumerate its owner's key ids
	// (GET .../keys, which stays at ProjectRead) and then DELETE each one is a
	// denial-of-service primitive against every CI job in the org, attributed in
	// the audit log to its owner.
	//
	// The mints were already refused one layer in, by requireMintAuthority, and
	// the revoke was not refused at all. Closing the DOOR rather than leaning on
	// the inner check is the shape the rest of this file uses: `/user/tokens` is
	// AccessUser for exactly this reason ("a token that could REVOKE them would
	// hand it a denial of service"), and a credential route reachable by a
	// credential contradicts that rule one file away from where it is written.
	AccessProjectCredential
	// AccessProjectAdmin needs {org} and {project} and CanAdminProject.
	AccessProjectAdmin
)

// needsOrg reports whether the guard must resolve {org} before deciding.
//
// Note there is deliberately NO needsProject() gate. Whether {project} is resolved
// is keyed on the ROUTE PATTERN (does it carry a {project} segment?), not on the
// Access level -- see resolve(). Keying it on the Access ladder was a latent bug:
// an AccessOrgAdmin route with a {project} segment (deleting a project) sits below
// AccessProjectRead, so the project was never resolved and the handler operated on
// the zero UUID.
func (a Access) needsOrg() bool { return a >= AccessOrgView }

// String is for the route-coverage test's failure messages.
func (a Access) String() string {
	switch a {
	case AccessPublic:
		return "public"
	case AccessAuthenticated:
		return "authenticated"
	case AccessUserScoped:
		return "user_scoped"
	case AccessUser:
		return "user"
	case AccessSiteAdmin:
		return "site_admin"
	case AccessOrgView:
		return "org_view"
	case AccessOrgAdmin:
		return "org_admin"
	case AccessOrgOwner:
		return "org_owner"
	case AccessProjectRead:
		return "project_read"
	case AccessProjectCredential:
		return "project_credential"
	case AccessProjectAdmin:
		return "project_admin"
	default:
		return "unknown"
	}
}

// Principal is the API layer's view of a verified identity.
//
// It is a CONSUMER-SIDE interface (kbi's pattern) over auth.Principal, which this
// package cannot construct and must not try to. Every method here also exists on
// auth.Principal, so a real auth.Principal satisfies this by assignment -- but
// the reverse is not true, and that asymmetry is the point: a test in THIS package
// can supply a fake to exercise the authorization matrix, while nothing anywhere
// can turn that fake back into an auth.Principal. The one place a real principal
// is genuinely required -- minting an API key -- type-asserts for it and fails
// closed (see keyMinter).
type Principal interface {
	UserID() pgtype.UUID
	Email() string
	DisplayName() string
	Method() auth.Method

	SiteRole() auth.SiteRole
	IsSiteAdmin() bool
	OrgRole(orgID pgtype.UUID) (auth.OrgRole, bool)
	ProjectRole(projectID pgtype.UUID) (auth.ProjectRole, bool)
	APIKey() (auth.KeyGrant, bool)
	Robot() (auth.RobotGrant, bool)

	CanViewOrg(orgID pgtype.UUID) bool
	CanAdminOrg(orgID pgtype.UUID) bool
	CanOwnOrg(orgID pgtype.UUID) bool
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
	CanAdminProject(orgID, projectID pgtype.UUID) bool
}

// auth.Principal must remain assignable to api.Principal. If someone adds a
// method here that auth.Principal lacks, this fails to compile rather than
// failing at runtime on the first request.
var _ = func(p auth.Principal) Principal { return p }

// scope is a route's org/project AFTER resolution and AFTER authorization.
//
// A handler receives this instead of the path. The ids in it came from the
// database via the slugs in the URL, and the guard has already asked the
// principal whether it may act on them at the route's Access level. There is no
// path-derived identifier left for a handler to trust.
type scope struct {
	OrgID   pgtype.UUID
	OrgSlug string
	OrgName string

	ProjectID   pgtype.UUID
	ProjectSlug string
}

type (
	principalKey struct{}
	scopeKey     struct{}
	authErrKey   struct{}
)

// withPrincipal is unexported. Production populates it in exactly one place --
// authenticate(), from auth.FromRequest -- and no other package can reach it.
// Tests in this package use it to inject a fake, which is precisely the seam
// that lets the authorization matrix be tested without forging an auth.Principal.
func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || p == nil {
		return nil, false
	}

	return p, true
}

func withScope(ctx context.Context, s scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// scopeFrom returns the authorized scope. It is only ever called from a handler
// the guard already ran, so a miss is a programming error, not a request error.
func scopeFrom(ctx context.Context) scope {
	s, ok := ctx.Value(scopeKey{}).(scope)
	if !ok {
		// Unreachable: the guard sets this before calling any handler whose Access
		// needs an org. Panicking is right -- it is a loud, local, fail-closed bug,
		// and the alternative (a zero-value scope) would authorize against the nil
		// UUID, which is a silent cross-tenant read.
		panic("api: scopeFrom called on a route whose Access does not resolve a scope")
	}

	return s
}

func withAuthErr(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, authErrKey{}, err)
}

func authErrFrom(ctx context.Context) error {
	err, _ := ctx.Value(authErrKey{}).(error)

	return err
}

// authenticate bridges auth.Principal into this package's context.
//
// It deliberately does NOT reject: authentication and authorization are separate,
// and the guard owns every 401 and 403 so that the error envelope is uniform. A
// request with no credential simply arrives anonymous, which the guard turns into
// a 401 on any route that is not Public. A request with a BROKEN credential
// (invalid token, revoked key, login not permitted) stashes the error, so the
// guard can tell "you sent nothing" from "you sent something bad" -- collapsing
// those two would let a revoked key read as merely-anonymous on a public route.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r.Context(), r)

		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		case errors.Is(err, auth.ErrUnauthenticated):
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r.WithContext(withAuthErr(r.Context(), err)))
		}
	})
}

// handlerFunc is what a route handler is: it returns an error instead of writing
// one. Every error goes through writeError, so no handler can invent an envelope
// or forget the status mapping.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// guard is the wrapper EVERY handler goes through. It:
//
//  1. rejects a broken credential;
//  2. enforces the JSON content type on state-changing methods (CSRF) --
//     BEFORE the public branch below, so a public route is covered too;
//  3. requires a principal on any non-public route;
//  4. refuses a MACHINE principal anywhere but /me (see below);
//  5. resolves {org} / {project} from slugs to database ids;
//  6. asks the principal whether it may act at this Access level;
//  7. only then calls the handler, with the resolved scope in the context.
//
// Step 2 runs ahead of the AccessPublic short-circuit on purpose: two of the
// public routes are state-changing (POST /auth/logout, POST /auth/dev-login),
// and "public" means "no credential required", not "exempt from the CSRF
// defence". requireJSON itself is method-gated (GET/HEAD/OPTIONS always pass),
// so moving it here does not touch the public GET routes (/auth/config,
// /auth/login, /auth/callback).
//
// Step 4 is a policy, not a capability: an API key COULD read its own project
// (CanReadProject is true for it), but the control plane is for humans and the
// CLI, and a key that could enumerate a project's other keys and its members is a
// lateral-movement primitive for no benefit. /me stays open so `bakery whoami`
// works with a key.
//
// It is written as an ALLOWLIST of interactive methods, matching
// principal.isInteractive, and for the same reason: keyed the other way round --
// "refuse auth.MethodAPIKey" -- every credential kind added later would be
// admitted to the whole control plane by default, silently, with no compiler
// error and no failing test. A new Method is refused here until someone
// deliberately admits it. (Stage 3 of the credential wave admits
// auth.MethodUserToken to the routes a personal access token is meant to reach;
// a robot stays refused everywhere but /me.)
func (a *API) guard(access Access, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if err := authErrFrom(ctx); err != nil {
			a.writeError(w, r, a.denyAuth(err))

			return
		}

		if err := requireJSON(r); err != nil {
			a.writeError(w, r, err)

			return
		}

		if access == AccessPublic {
			if err := h(w, r); err != nil {
				a.writeError(w, r, err)
			}

			return
		}

		p, ok := principalFrom(ctx)
		if !ok {
			a.writeError(w, r, errUnauthorized("authentication required"))

			return
		}

		if !methodMayReach(p.Method(), access) {
			a.writeError(w, r, errForbidden(methodRefusal(p.Method())))

			return
		}

		s, err := a.resolve(ctx, r, access, p)
		if err != nil {
			a.writeError(w, r, err)

			return
		}

		if err := h(w, r.WithContext(withScope(ctx, s))); err != nil {
			a.writeError(w, r, err)
		}
	}
}

// denyAuth maps an authentication failure onto the envelope.
func (a *API) denyAuth(err error) error {
	switch {
	case errors.Is(err, auth.ErrLoginNotAllowed):
		return errForbidden("this account is not authorized to use Bakery")
	case errors.Is(err, auth.ErrTokenInvalid), errors.Is(err, auth.ErrKeyInvalid):
		return errUnauthorized("the credential is invalid, revoked or expired")
	default:
		return errInternal("authentication failed", err)
	}
}

// resolve turns the route's slugs into ids and checks them against the caller.
//
// The 404-vs-403 ladder matters. If you cannot VIEW an org, you get a 404, not a
// 403 -- a 403 confirms the org exists and turns the endpoint into an
// organization-name oracle for anyone with an account. Once you can see it, a
// failure to ADMIN it is an honest 403, because at that point there is nothing
// left to hide from you.
func (a *API) resolve(ctx context.Context, r *http.Request, access Access, p Principal) (scope, error) {
	if !access.needsOrg() {
		if access == AccessSiteAdmin && !p.IsSiteAdmin() {
			return scope{}, errForbidden("this action requires a site administrator")
		}

		return scope{}, nil
	}

	orgSlug := r.PathValue("org")

	org, err := a.store.GetOrganizationBySlug(ctx, orgSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scope{}, errNotFound("organization not found")
		}

		return scope{}, errInternal("load organization", err)
	}

	s := scope{
		OrgID: org.ID, OrgSlug: org.Slug, OrgName: org.Name,
		ProjectID: pgtype.UUID{}, ProjectSlug: "",
	}

	// Existence is hidden behind view permission, always, before anything else.
	if !p.CanViewOrg(org.ID) {
		return scope{}, errNotFound("organization not found")
	}

	// Org-level authorization. AccessOrgView is already satisfied by CanViewOrg
	// above; the two project levels are checked after {project} resolves below.
	//
	// The org-level requirement is enforced here EVEN for a route that also carries
	// {project}: DELETE .../projects/{project} is AccessOrgAdmin precisely so that
	// only an org admin -- not the recipient of a delegated project-admin role --
	// may destroy a project. Resolving the project (below) does not relax that.
	switch access {
	case AccessOrgAdmin:
		if !p.CanAdminOrg(org.ID) {
			return scope{}, errForbidden("this action requires an organization administrator")
		}
	case AccessOrgOwner:
		if !p.CanOwnOrg(org.ID) {
			return scope{}, errForbidden("this action requires the organization owner")
		}
	case AccessPublic, AccessAuthenticated, AccessUserScoped, AccessUser, AccessSiteAdmin,
		AccessOrgView, AccessProjectRead, AccessProjectCredential, AccessProjectAdmin:
		// AccessOrgView: satisfied by CanViewOrg above. AccessProjectRead,
		// AccessProjectCredential and AccessProjectAdmin are checked after project
		// resolution -- and the first two by the SAME check, CanReadProject: the
		// credential level narrows the DOOR (methodMayReach), not the capability.
		// The rest cannot reach here (needsOrg already returned for them).
	}

	// Resolve {project} whenever the ROUTE carries it -- keyed on the path pattern,
	// NOT on the Access ladder. r.PathValue returns "" when the matched pattern has
	// no {project}, so a project-less route (POST /orgs/{org}/projects, the org
	// members routes) falls through here untouched. A route that DOES carry
	// {project} always gets a resolved id, so no handler can be handed the zero UUID
	// and issue `... WHERE id = NULL`.
	projectSlug := r.PathValue("project")
	if projectSlug == "" {
		return s, nil
	}

	route, err := a.store.ResolveRoute(ctx, repository.ResolveRouteParams{
		Slug: orgSlug, Slug_2: projectSlug,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scope{}, errNotFound("project not found")
		}

		return scope{}, errInternal("resolve project", err)
	}

	s.ProjectID = route.ProjectID
	s.ProjectSlug = projectSlug

	// Project existence is hidden behind read permission, same as the org above. An
	// org admin or site admin passes by virtue of org membership implying read.
	if !p.CanReadProject(org.ID, route.ProjectID) {
		return scope{}, errNotFound("project not found")
	}

	if access == AccessProjectAdmin && !p.CanAdminProject(org.ID, route.ProjectID) {
		return scope{}, errForbidden("this action requires a project administrator")
	}

	return s, nil
}

// requireJSON is the CSRF defence for the cookie-authenticated SPA.
//
// The session cookie is SameSite=Lax, which already blocks a cross-site POST from
// carrying it -- but Lax is a browser policy, not a server one, and the classic
// bypass is a cross-site <form> whose content type a browser will send without a
// CORS preflight: application/x-www-form-urlencoded, multipart/form-data, or
// text/plain. Requiring application/json means a cross-site form CANNOT shape a
// request this API will act on, because setting that content type triggers a
// preflight that we answer with no CORS headers at all.
func requireJSON(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}

	ct := r.Header.Get("Content-Type")

	// A bodyless state-changing request (DELETE, or a POST with nothing to say) is
	// fine: a cross-site form always sends one of the three forbidden types, so
	// "no content type at all" is not reachable from a form.
	if ct == "" && r.ContentLength == 0 {
		return nil
	}

	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return &apiError{
			status: http.StatusUnsupportedMediaType, code: CodeUnsupported, field: "",
			message: "state-changing requests must use Content-Type: application/json",
			cause:   err,
		}
	}

	return nil
}

// methodMayReach is THE CONTROL-PLANE DOOR: which Access levels each credential
// kind may come through at all.
//
// It is separate from -- and coarser than -- the capability methods on the
// principal. Those decide what a caller may DO; this decides whether the request
// gets as far as asking. An API key COULD read its own project, but a key that
// can enumerate that project's other keys and its members is a lateral-movement
// primitive for no benefit, so the door is shut before any capability is
// consulted. That is a policy, not a second opinion.
//
// EVERY ARM IS AN ALLOWLIST, with no default and a trailing `return false`. Keyed
// the other way round -- "refuse auth.MethodAPIKey" -- every credential kind
// added later would be admitted to the whole control plane by default, silently,
// with no compiler error and no failing test. That is exactly the bug this shape
// was inverted to prevent, and it must not be reintroduced by writing an
// exception for one Method instead of an entry for each.
//
// The three rows, and why:
//
//   - INTERACTIVE (session, bearer, dev): everything. A human is present.
//   - USER TOKEN: the READ ladder only -- /me, org view, project read. A personal
//     access token acts as its owner on the cache plane and for `bakery org
//     list`, which is the friction the credential exists to remove. It may not
//     create an organization (AccessUser hands the creator a local OWNER grant),
//     administer one, act as a site admin, or MANAGE CREDENTIALS
//     (AccessProjectCredential: minting one is refused by requireMintAuthority
//     anyway, but revoking one was not refused anywhere, and a leaked PAT that
//     can revoke its owner's API keys breaks every CI job in the org) -- so those
//     doors stay shut even though its principal would answer the capability
//     question the same way a session's would for the read levels.
//   - API KEY and ROBOT: /me and nothing else, so `bakery whoami` can verify a
//     credential without performing a push. Everything else is cache traffic.
func methodMayReach(m auth.Method, access Access) bool {
	switch m {
	case auth.MethodSession, auth.MethodBearer, auth.MethodDev:
		return true

	case auth.MethodUserToken:
		switch access {
		case AccessPublic, AccessAuthenticated, AccessUserScoped, AccessOrgView, AccessProjectRead:
			return true
		case AccessUser, AccessSiteAdmin, AccessOrgAdmin, AccessOrgOwner,
			AccessProjectCredential, AccessProjectAdmin:
			return false
		}

		return false

	case auth.MethodAPIKey, auth.MethodOrgToken:
		return access == AccessPublic || access == AccessAuthenticated
	}

	return false
}

// methodRefusal is the 403 body. It names what the credential IS for rather than
// what it is not, because the caller is a script and the message is the only
// instruction its operator will read.
func methodRefusal(m auth.Method) string {
	if m == auth.MethodUserToken {
		return "a personal access token may read the control plane but not administer it; " +
			"use a browser session or a CLI login"
	}

	return "this credential authorizes cache traffic only; " +
		"use a session or a CLI login for the control plane"
}

// interactiveMethod mirrors auth's own interactive/non-interactive split. It is
// used by this package's test double so the fake principal can never be MORE
// permissive than production, and by nothing else -- the door above is
// methodMayReach.
func interactiveMethod(m auth.Method) bool {
	switch m {
	case auth.MethodSession, auth.MethodBearer, auth.MethodDev:
		return true
	case auth.MethodAPIKey, auth.MethodUserToken, auth.MethodOrgToken:
		return false
	}

	return false
}

// actsAsUserMethod mirrors auth's principal.actsAsUser: which methods read the
// owner's LIVE role maps. Same duplication rationale as interactiveMethod, and
// the same closed-allowlist shape.
func actsAsUserMethod(m auth.Method) bool {
	switch m {
	case auth.MethodSession, auth.MethodBearer, auth.MethodDev, auth.MethodUserToken:
		return true
	case auth.MethodAPIKey, auth.MethodOrgToken:
		return false
	}

	return false
}
