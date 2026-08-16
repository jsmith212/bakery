package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// discardLogger keeps the test output readable; every handler logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixtureStore builds the org/project graph the whole suite shares:
//
//	org acme  -> project firmware
//	org other -> project secret     (a different tenant; nobody in acme may touch it)
func fixtureStore(t *testing.T) *fakeStore {
	t.Helper()

	acme := mustUUID(t, orgAcmeID)
	other := mustUUID(t, orgOtherID)
	firmware := mustUUID(t, projFirmwareID)
	secret := mustUUID(t, projOtherID)
	anna := mustUUID(t, userAnnaID)
	marko := mustUUID(t, userMarkoID)

	return &fakeStore{
		orgs: []repository.Organization{
			{ID: acme, Slug: "acme", Name: "Acme"},
			{ID: other, Slug: "other", Name: "Other"},
		},
		projects: []repository.Project{
			{ID: firmware, OrgID: acme, Slug: "firmware", Name: "Firmware"},
			{ID: secret, OrgID: other, Slug: "secret", Name: "Secret"},
		},
		orgMembers: map[pgtype.UUID][]repository.ListOrgMembersRow{
			acme: {
				{UserID: anna, Role: auth.OrgRoleAdmin, Email: "anna@acme.dev", DisplayName: "Anna Keller"},
				{UserID: marko, Role: auth.OrgRoleMember, Email: "marko@acme.dev", DisplayName: "Marko Ilic"},
			},
		},
		projectMembers: map[pgtype.UUID][]repository.ListProjectMembersRow{
			firmware: {
				{UserID: marko, Role: auth.ProjectRoleWriter, Email: "marko@acme.dev", DisplayName: "Marko Ilic"},
			},
		},
		users: []repository.User{
			{ID: anna, Email: "anna@acme.dev", DisplayName: "Anna Keller"},
			{ID: marko, Email: "marko@acme.dev", DisplayName: "Marko Ilic"},
		},
		siteAdmins:           nil,
		backends:             nil,
		keys:                 nil,
		calls:                nil,
		revokedForMembership: nil,
		revokedKeys:          nil,
		desiredErr:           nil,
	}
}

// testAPI builds an API over the fakes.
//
// auth is devLoginAuth, which is only ever asked DevLoginEnabled() at mount time --
// the tests inject their Principal straight into the context via withPrincipal and
// never run authenticate(). They cannot do otherwise: authenticate() gets its
// principal from auth.Service, whose Authenticate returns an auth.Principal, and no
// test can construct one of those. The sealed-Principal invariant is why the
// consumer-side api.Principal interface exists.
func testAPI(t *testing.T, store Store, minter keyMinter) *API {
	t.Helper()

	return &API{
		store: store, auth: devLoginAuth{enabled: false}, keys: minter,
		log: discardLogger(), allowSelfServeOrgs: true, allowLocalSiteAdmins: true,
		metrics: nil, routes: nil,
	}
}

// ---------------------------------------------------------------------------
// The role cast
// ---------------------------------------------------------------------------

// principals builds one principal per role, all against the `acme` / `firmware`
// fixture. `outsider` belongs to a DIFFERENT org, which is the case that proves
// cross-tenant access is refused rather than merely undocumented.
func principals(t *testing.T) map[string]Principal {
	t.Helper()

	acme := mustUUID(t, orgAcmeID)
	other := mustUUID(t, orgOtherID)
	firmware := mustUUID(t, projFirmwareID)
	anna := mustUUID(t, userAnnaID)

	base := func(method auth.Method) *fakePrincipal {
		return &fakePrincipal{
			userID: anna, email: "anna@acme.dev", displayName: "Anna Keller",
			method: method, siteRole: auth.SiteRoleUser,
			orgs:     map[pgtype.UUID]auth.OrgRole{},
			projects: map[pgtype.UUID]auth.ProjectRole{},
			key:      nil,
		}
	}

	siteAdmin := base(auth.MethodSession)
	siteAdmin.siteRole = auth.SiteRoleAdmin

	orgOwner := base(auth.MethodSession)
	orgOwner.orgs[acme] = auth.OrgRoleOwner

	orgAdmin := base(auth.MethodSession)
	orgAdmin.orgs[acme] = auth.OrgRoleAdmin

	orgMember := base(auth.MethodSession)
	orgMember.orgs[acme] = auth.OrgRoleMember

	// Project roles require an org membership -- the composite FK from
	// project_memberships into org_memberships makes that a database fact -- so
	// every project role below also carries plain org membership.
	projAdmin := base(auth.MethodSession)
	projAdmin.orgs[acme] = auth.OrgRoleMember
	projAdmin.projects[firmware] = auth.ProjectRoleAdmin

	projWriter := base(auth.MethodSession)
	projWriter.orgs[acme] = auth.OrgRoleMember
	projWriter.projects[firmware] = auth.ProjectRoleWriter

	projReader := base(auth.MethodSession)
	projReader.orgs[acme] = auth.OrgRoleMember
	projReader.projects[firmware] = auth.ProjectRoleReader

	outsider := base(auth.MethodSession)
	outsider.orgs[other] = auth.OrgRoleOwner

	// An API-key principal for `firmware`, at write scope. Even a WRITE key is
	// refused everywhere but /me: the control plane is not what a key is for.
	apiKey := base(auth.MethodAPIKey)
	apiKey.siteRole = auth.SiteRoleAdmin // the OWNER is a site admin; the key is not
	apiKey.key = &auth.KeyGrant{
		KeyID: mustUUID(t, keyAnnaID), ProjectID: firmware, Scope: auth.ScopeWrite,
	}

	return map[string]Principal{
		"anonymous":  nil,
		"site_admin": siteAdmin,
		"org_owner":  orgOwner,
		"org_admin":  orgAdmin,
		"org_member": orgMember,
		"proj_admin": projAdmin,
		"proj_write": projWriter,
		"proj_read":  projReader,
		"outsider":   outsider,
		"api_key":    apiKey,
	}
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

const (
	allow = http.StatusOK
	// A caller who may not even SEE the org gets 404, not 403 -- a 403 confirms
	// the org exists and turns the endpoint into a tenant-name oracle.
	hidden = http.StatusNotFound
	denied = http.StatusForbidden
	anon   = http.StatusUnauthorized
)

// TestGuardAuthorizationMatrix is the authorization matrix: every Access level,
// every role, allow or deny, with the exact status.
//
// It drives the GUARD rather than each handler, because the guard is where the
// decision is made -- and because the route table (TestRouteTable, below) pins
// every endpoint to an Access. The two together are the per-endpoint,
// per-role assertion: this test says what each Access permits, that one says which
// Access each endpoint carries, and neither can drift without a failure.
func TestGuardAuthorizationMatrix(t *testing.T) {
	// pattern per Access, so the guard has real path values to resolve.
	patterns := map[Access]struct{ method, pattern, target string }{
		AccessPublic:        {http.MethodGet, "GET /x", "/x"},
		AccessAuthenticated: {http.MethodGet, "GET /x", "/x"},
		AccessUserScoped:    {http.MethodGet, "GET /x", "/x"},
		AccessUser:          {http.MethodPost, "POST /x", "/x"},
		AccessSiteAdmin:     {http.MethodPost, "POST /x", "/x"},
		AccessOrgView:       {http.MethodGet, "GET /orgs/{org}", "/orgs/acme"},
		AccessOrgAdmin:      {http.MethodPost, "POST /orgs/{org}", "/orgs/acme"},
		AccessOrgOwner:      {http.MethodDelete, "DELETE /orgs/{org}", "/orgs/acme"},
		AccessProjectRead: {
			http.MethodGet, "GET /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
		AccessProjectCredential: {
			http.MethodPut, "PUT /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
		AccessProjectAdmin: {
			http.MethodPost, "POST /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
	}

	matrix := map[Access]map[string]int{
		AccessPublic: {
			"anonymous": allow, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": allow, "api_key": allow,
		},
		AccessAuthenticated: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": allow, "api_key": allow,
		},
		// Acts-as-a-user. Every human role passes and so would a personal access token;
		// an API key does not, because its answer would be an empty list it had no
		// business asking for -- and because "a robot reaches /me and nothing else"
		// has to be true of the shipped route table, not just of the door's table.
		AccessUserScoped: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": allow, "api_key": denied,
		},
		// A verified HUMAN. Every role passes; an API KEY does not, and that is the
		// whole reason the level exists: the one route on it (creating an org) hands
		// the caller a local owner grant, and a delegation must not become a master key.
		AccessUser: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": allow, "api_key": denied,
		},
		AccessSiteAdmin: {
			"anonymous": anon, "site_admin": allow, "org_owner": denied, "org_admin": denied,
			"org_member": denied, "proj_admin": denied, "proj_write": denied, "proj_read": denied,
			"outsider": denied, "api_key": denied,
		},
		AccessOrgView: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": hidden, "api_key": denied,
		},
		AccessOrgAdmin: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": denied, "proj_admin": denied, "proj_write": denied, "proj_read": denied,
			"outsider": hidden, "api_key": denied,
		},
		AccessOrgOwner: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": denied,
			"org_member": denied, "proj_admin": denied, "proj_write": denied, "proj_read": denied,
			"outsider": hidden, "api_key": denied,
		},
		AccessProjectRead: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": hidden, "api_key": denied,
		},
		// The CAPABILITY floor is CanReadProject, identical to AccessProjectRead
		// above: a project reader may mint a read-scoped key for themselves, and the
		// write cap lives in auth.CreateAPIKey. What this level narrows is the DOOR
		// -- see TestControlPlaneDoorIsAnAllowlist and TestUserTokenCannotManageAPIKeys.
		AccessProjectCredential: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": allow, "proj_admin": allow, "proj_write": allow, "proj_read": allow,
			"outsider": hidden, "api_key": denied,
		},
		AccessProjectAdmin: {
			"anonymous": anon, "site_admin": allow, "org_owner": allow, "org_admin": allow,
			"org_member": denied, "proj_admin": allow, "proj_write": denied, "proj_read": denied,
			"outsider": hidden, "api_key": denied,
		},
	}

	cast := principals(t)
	store := fixtureStore(t)
	a := testAPI(t, store, nil)

	for access, route := range patterns {
		expected, ok := matrix[access]
		if !ok {
			t.Fatalf("Access %s has no row in the matrix; every Access must be covered", access)
		}

		for role, want := range expected {
			t.Run(access.String()+"/"+role, func(t *testing.T) {
				var reached bool

				mux := http.NewServeMux()
				mux.HandleFunc(route.pattern, a.guard(access, func(w http.ResponseWriter, _ *http.Request) error {
					reached = true
					w.WriteHeader(http.StatusOK)

					return nil
				}))

				r := httptest.NewRequest(route.method, route.target, nil)
				if p := cast[role]; p != nil {
					r = r.WithContext(withPrincipal(r.Context(), p))
				}

				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)

				if w.Code != want {
					t.Errorf("status = %d, want %d (body %s)", w.Code, want, strings.TrimSpace(w.Body.String()))
				}

				// The stronger assertion: a denied request must not have reached the
				// handler at all. A 403 that still ran the handler -- and so still did
				// the write, and merely lied about the status -- is exactly the bug a
				// status-code-only test cannot see.
				if wantReached := want == allow; reached != wantReached {
					t.Errorf("handler reached = %v, want %v", reached, wantReached)
				}
			})
		}
	}
}

// TestEveryAccessIsInTheMatrix fails when a new Access constant is added without a
// row in the matrix above -- otherwise a new privilege level could ship with no
// test at all, which is the failure mode this whole file exists to prevent.
func TestEveryAccessIsInTheMatrix(t *testing.T) {
	for access := AccessPublic; access <= AccessProjectAdmin; access++ {
		if access.String() == "unknown" {
			t.Errorf("Access(%d) has no String(); add it, and add a matrix row", int(access))
		}
	}

	// If someone appends a constant after AccessProjectAdmin, this catches it.
	if AccessProjectAdmin+1 != 11 {
		t.Errorf("a new Access constant was added: extend patterns{} and matrix{} in "+
			"TestGuardAuthorizationMatrix, then update this bound. Highest is now %d",
			int(AccessProjectAdmin)+1)
	}
}

// TestGuardRejectsCrossTenantProject proves the project resolution is scoped to the
// org in the path, not merely to "some project with this slug".
//
// Without this, `/orgs/acme/projects/secret` would resolve `secret` globally and
// hand an acme admin a project in another tenant. ResolveRoute takes BOTH slugs for
// exactly this reason, and the guard is the only caller.
func TestGuardRejectsCrossTenantProject(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, nil)

	cast := principals(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{org}/projects/{project}",
		a.guard(AccessProjectRead, func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusOK)

			return nil
		}))

	// An acme org OWNER naming a project that exists, but in `other`.
	r := httptest.NewRequest(http.MethodGet, "/orgs/acme/projects/secret", nil)
	r = r.WithContext(withPrincipal(r.Context(), cast["org_owner"]))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d: a project in another org must not resolve", w.Code, http.StatusNotFound)
	}
}

// TestProjectRoutesResolveProjectID pins finding: EVERY route whose pattern
// carries a {project} segment must hand the handler a non-zero scope.ProjectID.
//
// The bug it guards: DELETE /orgs/{org}/projects/{project} is registered
// AccessOrgAdmin, and the guard used to resolve {project} only for access levels at
// or above AccessProjectRead. AccessOrgAdmin sits below it, so scope.ProjectID
// stayed the zero (NULL) UUID and handleDeleteProject issued `DELETE ... WHERE id =
// NULL`, matching no row -- a dead endpoint for every caller, site admin included.
//
// The matrix test only checks status codes, so it could never have caught a handler
// that authorized correctly and then operated on the wrong (null) id. This drives
// the real mounted route table and asserts the resolved scope directly.
func TestProjectRoutesResolveProjectID(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, nil)
	a.routes = nil
	a.mount(http.NewServeMux())

	// A site admin passes every authorization check, so any non-zero ProjectID
	// afterwards is the guard's resolution, not a role accident.
	admin := principals(t)["site_admin"]
	firmware := mustUUID(t, projFirmwareID)

	replacer := strings.NewReplacer(
		"{org}", "acme",
		"{project}", "firmware",
		"{user}", "someone@acme.dev",
		"{kind}", "sstate",
		"{key}", keyAnnaID,
	)

	var covered int

	for _, route := range a.routes {
		if !strings.Contains(route.Pattern, "{project}") {
			continue
		}

		covered++

		t.Run(route.Pattern, func(t *testing.T) {
			method, pattern, ok := strings.Cut(route.Pattern, " ")
			if !ok {
				t.Fatalf("malformed route pattern %q", route.Pattern)
			}

			var (
				got     scope
				reached bool
			)

			mux := http.NewServeMux()
			mux.HandleFunc(pattern, a.guard(route.Access, func(_ http.ResponseWriter, r *http.Request) error {
				got = scopeFrom(r.Context())
				reached = true

				return nil
			}))

			body := "{}"

			req := httptest.NewRequest(method, replacer.Replace(pattern), strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(withPrincipal(req.Context(), admin))

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if !reached {
				t.Fatalf("guard did not reach the handler: status=%d body=%s",
					w.Code, strings.TrimSpace(w.Body.String()))
			}

			if !got.ProjectID.Valid || got.ProjectID == (pgtype.UUID{}) {
				t.Fatalf("scope.ProjectID is the zero/NULL uuid for a {project} route; " +
					"the handler would operate on WHERE id = NULL")
			}

			if got.ProjectID != firmware {
				t.Errorf("scope.ProjectID = %v, want firmware %v", got.ProjectID, firmware)
			}
		})
	}

	if covered == 0 {
		t.Fatal("no {project} routes were exercised; the route table or this test is wrong")
	}
}

// TestStateChangingRequestsRequireJSON is the CSRF gate.
//
// The session cookie is SameSite=Lax, which blocks a cross-site POST from carrying
// it -- but the defence in depth is that a cross-site <form> can only send
// urlencoded, multipart or text/plain, none of which this API will act on. Setting
// application/json from another origin requires a preflight, which we answer with
// no CORS headers at all.
func TestStateChangingRequestsRequireJSON(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		access      Access
		want        int
	}{
		{"json is accepted", http.MethodPost, "application/json", `{}`, AccessAuthenticated, http.StatusOK},
		{
			"json with a charset is accepted", http.MethodPost,
			"application/json; charset=utf-8", `{}`, AccessAuthenticated, http.StatusOK,
		},
		{
			"a cross-site form post is refused", http.MethodPost,
			"application/x-www-form-urlencoded", "a=b", AccessAuthenticated, http.StatusUnsupportedMediaType,
		},
		{
			"a multipart form post is refused", http.MethodPost,
			"multipart/form-data; boundary=x", "x", AccessAuthenticated, http.StatusUnsupportedMediaType,
		},
		{
			"a text/plain post is refused", http.MethodPost,
			"text/plain", "x", AccessAuthenticated, http.StatusUnsupportedMediaType,
		},
		{"a bodyless DELETE is accepted", http.MethodDelete, "", "", AccessAuthenticated, http.StatusOK},
		{"a GET is never gated", http.MethodGet, "", "", AccessAuthenticated, http.StatusOK},
		// AccessPublic covers POST /auth/logout and POST /auth/dev-login: "public"
		// means "no credential required", not "exempt from the CSRF defence" -- the
		// guard must run requireJSON ahead of its AccessPublic short-circuit.
		{
			"an AccessPublic cross-site form post is refused (dev-login/logout share this guard)",
			http.MethodPost, "application/x-www-form-urlencoded", "a=b", AccessPublic, http.StatusUnsupportedMediaType,
		},
		{
			"an AccessPublic bodyless POST is accepted (the dev-login/logout shape)",
			http.MethodPost, "", "", AccessPublic, http.StatusOK,
		},
		{"an AccessPublic GET is never gated", http.MethodGet, "", "", AccessPublic, http.StatusOK},
	}

	a := testAPI(t, fixtureStore(t), nil)
	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/x", a.guard(tt.access, func(w http.ResponseWriter, _ *http.Request) error {
				w.WriteHeader(http.StatusOK)

				return nil
			}))

			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}

			r := httptest.NewRequest(tt.method, "/x", body)
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}

			r = r.WithContext(withPrincipal(r.Context(), cast["org_admin"]))

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestControlPlaneDoorIsAnAllowlist is the api layer's half of the allowlist
// inversion, stated as a full table: every Method x every Access, allowed or
// refused, with no zero-valued row.
//
// The guard used to ask `p.Method() == auth.MethodAPIKey`, which is a denylist:
// declaring a new credential kind admitted it to the ENTIRE control plane by
// default -- member lists, key rosters, org settings -- with no compiler error and
// no failing test. A robot token minted for CI would have been able to enumerate
// every project's members on the day the constant landed.
//
// The principals below are deliberately maximal (site admin, org owner, project
// admin) and their capability methods are the fake's, not production's. Even so
// they must be stopped at the door where the table says so: this gate runs BEFORE
// any capability is consulted, which is what makes it a policy rather than a
// second opinion.
//
// The three rows are the product decision, in one place:
//
//   - a session/bearer/dev human: everything;
//   - a personal access token: the READ ladder, because it acts as its owner and
//     `bakery org list` is the friction it exists to remove -- but never the
//     admin ladder, never AccessUser (creating an org grants the creator a local
//     OWNER role) and never site admin;
//   - an API key and a robot: /me and nothing else, so `bakery whoami` can verify
//     a credential without performing a push.
func TestControlPlaneDoorIsAnAllowlist(t *testing.T) {
	acme := mustUUID(t, orgAcmeID)
	firmware := mustUUID(t, projFirmwareID)

	// Every Access, with a pattern the guard can actually resolve.
	routes := map[Access]struct{ method, pattern, target string }{
		AccessAuthenticated: {http.MethodGet, "GET /me", "/me"},
		AccessUserScoped:    {http.MethodGet, "GET /x", "/x"},
		AccessUser:          {http.MethodGet, "GET /x", "/x"},
		AccessSiteAdmin:     {http.MethodGet, "GET /x", "/x"},
		AccessOrgView:       {http.MethodGet, "GET /orgs/{org}", "/orgs/acme"},
		AccessOrgAdmin:      {http.MethodGet, "GET /orgs/{org}", "/orgs/acme"},
		AccessOrgOwner:      {http.MethodGet, "GET /orgs/{org}", "/orgs/acme"},
		AccessProjectRead: {
			http.MethodGet, "GET /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
		AccessProjectCredential: {
			http.MethodGet, "GET /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
		AccessProjectAdmin: {
			http.MethodGet, "GET /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
		},
	}

	// `true` means the DOOR admits this method at this level. It does not mean the
	// request succeeds -- the capability check still runs afterwards.
	door := controlPlaneDoorTable()

	store := fixtureStore(t)
	a := testAPI(t, store, nil)

	for method, expected := range door {
		principal := &fakePrincipal{
			userID: mustUUID(t, userAnnaID), email: "anna@example.com", displayName: "Anna",
			method: method, siteRole: auth.SiteRoleAdmin,
			orgs:     map[pgtype.UUID]auth.OrgRole{acme: auth.OrgRoleOwner},
			projects: map[pgtype.UUID]auth.ProjectRole{firmware: auth.ProjectRoleAdmin},
			key:      nil, robot: nil, maxScope: auth.ScopeWrite,
		}

		for access, route := range routes {
			admit, stated := expected[access]
			if !stated {
				t.Fatalf("%s has no entry for %s; every Method must state every Access",
					method, access)
			}

			t.Run(string(method)+"/"+access.String(), func(t *testing.T) {
				var reached bool

				mux := http.NewServeMux()
				mux.HandleFunc(route.pattern,
					a.guard(access, func(w http.ResponseWriter, _ *http.Request) error {
						reached = true
						w.WriteHeader(http.StatusOK)

						return nil
					}))

				r := httptest.NewRequest(route.method, route.target, nil)
				r = r.WithContext(withPrincipal(r.Context(), principal))

				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)

				if admit {
					if w.Code != allow || !reached {
						t.Errorf("status = %d (reached %v), want 200: the door refused a credential "+
							"the table admits (body %s)", w.Code, reached, strings.TrimSpace(w.Body.String()))
					}

					return
				}

				if w.Code != denied {
					t.Errorf("status = %d, want %d: a credential reached a level the table refuses "+
						"(body %s)", w.Code, denied, strings.TrimSpace(w.Body.String()))
				}

				if reached {
					t.Error("the handler ran for a credential the door refuses")
				}
			})
		}
	}
}

// declaredAuthMethods reads every `Method` constant out of internal/auth's
// source, so the door tests cannot be driven by a hand-written list.
//
// It is the api-side twin of internal/auth's own declaredMethods, duplicated
// rather than exported because a test helper is not part of a package's API --
// and this seam is exactly where the duplication has to be paid. internal/auth
// declares the Methods; internal/api decides which doors each may come through.
// A gate on the second half that restates the first half's list fails in
// precisely the case it exists to catch: declaring MethodDeployKey in
// internal/auth must fail a test in THIS package with no edit here.
//
// The whole PACKAGE is scanned, not principal.go alone: credential code in
// internal/auth is already spread over five files, so a Method declared beside
// its own validator would otherwise be invisible.
func declaredAuthMethods(t *testing.T) []auth.Method {
	t.Helper()

	dir := filepath.Join("..", "auth")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()

	var out []auth.Method

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}

			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != "Method" || len(value.Values) == 0 {
					continue
				}

				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("Method constant %v is not a string literal; the scan cannot read it",
						value.Names)
				}

				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}

				out = append(out, auth.Method(unquoted))
			}
		}
	}

	if len(out) == 0 {
		t.Fatal("found no auth.Method constants; the source scan has lost its inputs")
	}

	return out
}

// TestEveryMethodIsInTheControlPlaneDoor fails when a Method is declared without
// a decision about which doors it may come through.
//
// methodMayReach's switch has no default arm and a trailing `return false`, so an
// unclassified Method is refused everywhere -- fail-closed, which is right. This
// makes it LOUD as well, so a new credential kind cannot silently ship with no
// control-plane access at all when it was meant to have some.
//
// IT IS DRIVEN BY THE SOURCE, not by a literal. The previous shape declared its
// own six-entry map and then ranged over it, so declaring a Method in
// internal/auth changed nothing here -- the test its own doc-comment describes
// did not exist. It also checks the ALLOWLIST TABLE in
// TestControlPlaneDoorIsAnAllowlist above, which is the table that says what each
// credential may actually reach; a Method missing from it has no per-level
// assertion at all.
func TestEveryMethodIsInTheControlPlaneDoor(t *testing.T) {
	declared := declaredAuthMethods(t)

	for _, m := range declared {
		if !methodMayReach(m, AccessAuthenticated) {
			t.Errorf("%s cannot reach /me; every credential must be able to verify itself.\n"+
				"A newly declared Method is refused everywhere by methodMayReach's trailing "+
				"`return false` -- fail-closed, and deliberately loud here: add an arm for it.", m)
		}

		if _, stated := controlPlaneDoorTable()[m]; !stated {
			t.Errorf("%s has no row in the door table.\n"+
				"Add one to controlPlaneDoorTable so TestControlPlaneDoorIsAnAllowlist "+
				"asserts what this credential may reach at every Access level.", m)
		}
	}

	// An unrecognised Method is refused at every level, including /me.
	for access := AccessPublic; access <= AccessProjectAdmin; access++ {
		if access == AccessPublic {
			continue
		}

		if methodMayReach(auth.Method("brand_new_credential"), access) {
			t.Errorf("an unclassified Method reached %s; the door is not an allowlist", access)
		}
	}
}

// TestRobotRejectedOnEveryAPIRouteExceptMe drives the REAL route table.
//
// The two tests above assert the door's table; this one asserts that the table is
// what the shipped routes are actually behind. It walks every registered route,
// presents a maximal robot principal, and requires a refusal everywhere but
// GET /me -- at the ROUTER SEAM, before any handler runs, which is why the
// assertion is on `reached` as much as on the status.
//
// It needs no per-route knowledge and no maintenance: a route added tomorrow is
// covered the moment it is registered.
func TestRobotRejectedOnEveryAPIRouteExceptMe(t *testing.T) {
	acme := mustUUID(t, orgAcmeID)
	firmware := mustUUID(t, projFirmwareID)

	robot := &fakePrincipal{
		userID: pgtype.UUID{}, email: "", displayName: "",
		method: auth.MethodOrgToken, siteRole: auth.SiteRoleAdmin,
		// Deliberately maximal in every field a robot cannot really have, so that a
		// leak through any capability branch shows up as an allow.
		orgs:     map[pgtype.UUID]auth.OrgRole{acme: auth.OrgRoleOwner},
		projects: map[pgtype.UUID]auth.ProjectRole{firmware: auth.ProjectRoleAdmin},
		key:      nil,
		robot:    &auth.RobotGrant{RobotID: uuidOf("robot"), OrgID: acme, Scope: auth.ScopeWrite},
		maxScope: auth.ScopeWrite,
	}

	store := fixtureStore(t)
	a := testAPI(t, store, nil)
	a.mount(http.NewServeMux())

	// Path values the guard needs, substituted into each registered pattern.
	values := strings.NewReplacer(
		"{org}", "acme",
		"{project}", "firmware",
		"{user}", "anna@example.com",
		"{key}", keyAnnaID,
		"{token}", keyAnnaID,
		"{robot}", keyAnnaID,
		"{kind}", "sstate",
		"{id}", "1",
	)

	for _, spec := range a.routes {
		if spec.Access == AccessPublic {
			continue // no credential is consulted at all
		}

		t.Run(spec.Pattern, func(t *testing.T) {
			method, pattern, ok := strings.Cut(spec.Pattern, " ")
			if !ok {
				t.Fatalf("route %q has no method", spec.Pattern)
			}

			var reached bool

			mux := http.NewServeMux()
			mux.HandleFunc(spec.Pattern,
				a.guard(spec.Access, func(w http.ResponseWriter, _ *http.Request) error {
					reached = true
					w.WriteHeader(http.StatusOK)

					return nil
				}))

			r := httptest.NewRequest(method, values.Replace(pattern), nil)
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(withPrincipal(r.Context(), robot))

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			isMe := pattern == Prefix+"/me"

			if isMe {
				if w.Code != allow || !reached {
					t.Errorf("GET /me = %d (reached %v), want 200: a robot must be able to verify "+
						"its own token without performing a push", w.Code, reached)
				}

				return
			}

			if w.Code != denied || reached {
				t.Errorf("status = %d (reached %v), want %d and no handler: a robot reached a "+
					"control-plane route (body %s)",
					w.Code, reached, denied, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

// TestUserTokenCannotManageAPIKeys drives the REAL route table with a personal
// access token and asserts the credential-management routes are shut at the door.
//
// The rule is stated twice in this wave -- usertokens.go's "a token that could
// REVOKE them would hand it a denial of service" and api.go's `/user/tokens`
// being AccessUser -- and the API keys under a project are the same class of
// object as the tokens under a user. Before AccessProjectCredential existed the
// three credential routes sat at AccessProjectRead, which the door deliberately
// opens to a PAT: the two mints were refused one layer in by requireMintAuthority,
// and the REVOKE was refused nowhere at all. `keys.go`'s owner check passes for a
// PAT because a PAT's UserID() IS its owner's, so a leaked token could enumerate
// its owner's key ids (GET .../keys, which stays open on purpose) and delete every
// one of them -- no escalation, and every CI job in the org broken, attributed to
// the owner.
//
// The assertion is on `reached` as much as on the status: this must be refused at
// the router seam, before any handler or any inner check runs.
func TestUserTokenCannotManageAPIKeys(t *testing.T) {
	acme := mustUUID(t, orgAcmeID)
	firmware := mustUUID(t, projFirmwareID)

	// Maximal in every field, so a leak through any capability branch shows up as
	// an allow rather than as a coincidence of this principal being weak.
	pat := &fakePrincipal{
		userID: mustUUID(t, userAnnaID), email: "anna@example.com", displayName: "Anna",
		method: auth.MethodUserToken, siteRole: auth.SiteRoleAdmin,
		orgs:     map[pgtype.UUID]auth.OrgRole{acme: auth.OrgRoleOwner},
		projects: map[pgtype.UUID]auth.ProjectRole{firmware: auth.ProjectRoleAdmin},
		key:      nil, robot: nil, maxScope: auth.ScopeWrite,
	}

	// The three routes that MINT or REVOKE a credential, and the one that merely
	// lists metadata -- which stays reachable, because that is what `bakery key
	// list` does with a PAT and it hands a leaked token nothing it did not have.
	cases := map[string]bool{
		"POST " + Prefix + "/orgs/{org}/projects/{project}/keys":         false,
		"DELETE " + Prefix + "/orgs/{org}/projects/{project}/keys/{key}": false,
		"POST " + Prefix + "/orgs/{org}/projects/{project}/snippet":      false,
		"GET " + Prefix + "/orgs/{org}/projects/{project}/keys":          true,
	}

	store := fixtureStore(t)
	a := testAPI(t, store, nil)
	a.mount(http.NewServeMux())

	values := strings.NewReplacer(
		"{org}", "acme",
		"{project}", "firmware",
		"{key}", keyAnnaID,
	)

	// Every case must name a route that is actually registered: a test that
	// silently stops covering a renamed route is worse than no test.
	registered := map[string]Access{}
	for _, spec := range a.routes {
		registered[spec.Pattern] = spec.Access
	}

	for pattern, admit := range cases {
		access, ok := registered[pattern]
		if !ok {
			t.Fatalf("%q is not a registered route; this test has drifted from the route table", pattern)
		}

		t.Run(pattern, func(t *testing.T) {
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("route %q has no method", pattern)
			}

			var reached bool

			mux := http.NewServeMux()
			mux.HandleFunc(pattern, a.guard(access, func(w http.ResponseWriter, _ *http.Request) error {
				reached = true
				w.WriteHeader(http.StatusOK)

				return nil
			}))

			r := httptest.NewRequest(method, values.Replace(path), nil)
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(withPrincipal(r.Context(), pat))

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if admit {
				if w.Code != allow || !reached {
					t.Errorf("status = %d (reached %v), want 200: reading your own key metadata "+
						"with a personal access token is what `bakery key list` does (body %s)",
						w.Code, reached, strings.TrimSpace(w.Body.String()))
				}

				return
			}

			if w.Code != denied || reached {
				t.Errorf("status = %d (reached %v), want %d and no handler: a personal access "+
					"token reached a credential-management route (body %s)",
					w.Code, reached, denied, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

// controlPlaneDoorTable is THE PRODUCT DECISION, in one place: which Access
// levels each credential kind may come through at all.
//
// It is a function rather than a literal inside one test because TWO tests need
// it: TestControlPlaneDoorIsAnAllowlist asserts each row against the real guard,
// and TestEveryMethodIsInTheControlPlaneDoor asserts that every Method declared
// in internal/auth HAS a row here. A table only one of them can see is a table
// the other cannot police -- which is how the door test came to restate its own
// six-entry list and stop noticing new credentials altogether.
func controlPlaneDoorTable() map[auth.Method]map[Access]bool {
	return map[auth.Method]map[Access]bool{
		auth.MethodSession: {
			AccessAuthenticated: true, AccessUserScoped: true, AccessUser: true,
			AccessSiteAdmin: true, AccessOrgView: true, AccessOrgAdmin: true,
			AccessOrgOwner: true, AccessProjectRead: true, AccessProjectCredential: true,
			AccessProjectAdmin: true,
		},
		auth.MethodBearer: {
			AccessAuthenticated: true, AccessUserScoped: true, AccessUser: true,
			AccessSiteAdmin: true, AccessOrgView: true, AccessOrgAdmin: true,
			AccessOrgOwner: true, AccessProjectRead: true, AccessProjectCredential: true,
			AccessProjectAdmin: true,
		},
		auth.MethodDev: {
			AccessAuthenticated: true, AccessUserScoped: true, AccessUser: true,
			AccessSiteAdmin: true, AccessOrgView: true, AccessOrgAdmin: true,
			AccessOrgOwner: true, AccessProjectRead: true, AccessProjectCredential: true,
			AccessProjectAdmin: true,
		},
		auth.MethodUserToken: {
			AccessAuthenticated: true, AccessUserScoped: true, AccessUser: false,
			AccessSiteAdmin: false, AccessOrgView: true, AccessOrgAdmin: false,
			AccessOrgOwner: false, AccessProjectRead: true, AccessProjectCredential: false,
			AccessProjectAdmin: false,
		},
		auth.MethodAPIKey: {
			AccessAuthenticated: true, AccessUserScoped: false, AccessUser: false,
			AccessSiteAdmin: false, AccessOrgView: false, AccessOrgAdmin: false,
			AccessOrgOwner: false, AccessProjectRead: false, AccessProjectCredential: false,
			AccessProjectAdmin: false,
		},
		auth.MethodOrgToken: {
			AccessAuthenticated: true, AccessUserScoped: false, AccessUser: false,
			AccessSiteAdmin: false, AccessOrgView: false, AccessOrgAdmin: false,
			AccessOrgOwner: false, AccessProjectRead: false, AccessProjectCredential: false,
			AccessProjectAdmin: false,
		},
	}
}
