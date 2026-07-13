package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		log: discardLogger(), allowSelfServeOrgs: true, metrics: nil, routes: nil,
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
		AccessUser:          {http.MethodPost, "POST /x", "/x"},
		AccessSiteAdmin:     {http.MethodPost, "POST /x", "/x"},
		AccessOrgView:       {http.MethodGet, "GET /orgs/{org}", "/orgs/acme"},
		AccessOrgAdmin:      {http.MethodPost, "POST /orgs/{org}", "/orgs/acme"},
		AccessOrgOwner:      {http.MethodDelete, "DELETE /orgs/{org}", "/orgs/acme"},
		AccessProjectRead: {
			http.MethodGet, "GET /orgs/{org}/projects/{project}", "/orgs/acme/projects/firmware",
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
	if AccessProjectAdmin+1 != 9 {
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
		want        int
	}{
		{"json is accepted", http.MethodPost, "application/json", `{}`, http.StatusOK},
		{
			"json with a charset is accepted", http.MethodPost,
			"application/json; charset=utf-8", `{}`, http.StatusOK,
		},
		{
			"a cross-site form post is refused", http.MethodPost,
			"application/x-www-form-urlencoded", "a=b", http.StatusUnsupportedMediaType,
		},
		{
			"a multipart form post is refused", http.MethodPost,
			"multipart/form-data; boundary=x", "x", http.StatusUnsupportedMediaType,
		},
		{
			"a text/plain post is refused", http.MethodPost,
			"text/plain", "x", http.StatusUnsupportedMediaType,
		},
		{"a bodyless DELETE is accepted", http.MethodDelete, "", "", http.StatusOK},
		{"a GET is never gated", http.MethodGet, "", "", http.StatusOK},
	}

	a := testAPI(t, fixtureStore(t), nil)
	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/x", a.guard(AccessAuthenticated, func(w http.ResponseWriter, _ *http.Request) error {
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
