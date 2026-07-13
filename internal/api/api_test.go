package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// devLoginAuth is a stand-in for *auth.Service that reports only whether dev-login
// is on. mount() reads nothing else off it at registration time.
//
// Note it can satisfy authService only because Authenticate may return a NIL
// auth.Principal -- it cannot return a real one, because auth.Principal is sealed
// and no test can construct one. That is the invariant holding, not a gap.
type devLoginAuth struct{ enabled bool }

func (d devLoginAuth) DevLoginEnabled() bool { return d.enabled }

func (d devLoginAuth) HandleDevLogin(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (d devLoginAuth) HandleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
func (d devLoginAuth) HandleLogin(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
func (d devLoginAuth) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
func (d devLoginAuth) HandleLogout(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (d devLoginAuth) Authenticate(_ context.Context, _ *http.Request) (auth.Principal, error) {
	return nil, auth.ErrUnauthenticated
}

// TestRouteTable pins every endpoint to its required role.
//
// This is the OTHER half of the authorization guarantee. TestGuardAuthorizationMatrix
// says what each Access permits; this says which Access each endpoint carries. A new
// endpoint cannot ship without an Access (the compiler demands one), and it cannot
// ship with the WRONG Access without failing here -- a reviewer is confronted with a
// diff that says, in one line, "this endpoint is now readable by every org member".
func TestRouteTable(t *testing.T) {
	want := []routeSpec{
		{AccessPublic, "GET /api/v1/auth/config"},
		{AccessPublic, "GET /api/v1/auth/login"},
		{AccessPublic, "GET /api/v1/auth/callback"},
		{AccessPublic, "POST /api/v1/auth/logout"},

		{AccessAuthenticated, "GET /api/v1/me"},

		// Site admins. All three are AccessSiteAdmin, which the guard admits no API key
		// to -- an API-key principal can never grant a site role. There is deliberately
		// no route that can mint the FIRST site admin; that is `bakery user site-admin`,
		// which needs DB_URL and no session.
		{AccessSiteAdmin, "GET /api/v1/site-admins"},
		{AccessSiteAdmin, "PUT /api/v1/site-admins/{user}"},
		{AccessSiteAdmin, "DELETE /api/v1/site-admins/{user}"},

		{AccessAuthenticated, "GET /api/v1/orgs"},
		{AccessUser, "POST /api/v1/orgs"},
		{AccessOrgView, "GET /api/v1/orgs/{org}"},
		{AccessOrgAdmin, "PATCH /api/v1/orgs/{org}"},
		{AccessOrgOwner, "DELETE /api/v1/orgs/{org}"},

		{AccessOrgView, "GET /api/v1/orgs/{org}/members"},
		{AccessOrgAdmin, "PUT /api/v1/orgs/{org}/members/{user}"},
		{AccessOrgAdmin, "DELETE /api/v1/orgs/{org}/members/{user}"},

		{AccessOrgView, "GET /api/v1/orgs/{org}/projects"},
		{AccessOrgAdmin, "POST /api/v1/orgs/{org}/projects"},
		{AccessProjectRead, "GET /api/v1/orgs/{org}/projects/{project}"},
		{AccessProjectAdmin, "PATCH /api/v1/orgs/{org}/projects/{project}"},
		{AccessOrgAdmin, "DELETE /api/v1/orgs/{org}/projects/{project}"},

		{AccessProjectRead, "GET /api/v1/orgs/{org}/projects/{project}/members"},
		{AccessProjectAdmin, "PUT /api/v1/orgs/{org}/projects/{project}/members/{user}"},
		{AccessProjectAdmin, "DELETE /api/v1/orgs/{org}/projects/{project}/members/{user}"},

		{AccessProjectRead, "GET /api/v1/orgs/{org}/projects/{project}/keys"},
		{AccessProjectRead, "POST /api/v1/orgs/{org}/projects/{project}/keys"},
		{AccessProjectRead, "DELETE /api/v1/orgs/{org}/projects/{project}/keys/{key}"},

		{AccessProjectRead, "POST /api/v1/orgs/{org}/projects/{project}/snippet"},

		{AccessProjectRead, "GET /api/v1/orgs/{org}/projects/{project}/backends"},
		{AccessProjectAdmin, "POST /api/v1/orgs/{org}/projects/{project}/backends"},
		{AccessProjectRead, "GET /api/v1/orgs/{org}/projects/{project}/backends/{kind}"},
		{AccessProjectAdmin, "PATCH /api/v1/orgs/{org}/projects/{project}/backends/{kind}"},
		{AccessProjectAdmin, "DELETE /api/v1/orgs/{org}/projects/{project}/backends/{kind}"},
	}

	a := &API{
		store: fixtureStore(t), auth: devLoginAuth{enabled: false}, keys: nil,
		log: discardLogger(), allowSelfServeOrgs: true, allowLocalSiteAdmins: true,
		metrics: nil, routes: nil,
	}
	a.mount(http.NewServeMux())

	if len(a.routes) != len(want) {
		t.Fatalf("registered %d routes, expected %d.\n"+
			"A route was added or removed. Add it here WITH ITS REQUIRED ROLE.\ngot:  %v",
			len(a.routes), len(want), a.routes)
	}

	for i, got := range a.routes {
		if got != want[i] {
			t.Errorf("route %d = {%s %s}, want {%s %s}",
				i, got.Access, got.Pattern, want[i].Access, want[i].Pattern)
		}
	}
}

// TestDevLoginRouteDoesNotExistWhenDisabled: when DEV_LOGIN_ENABLED is off, the
// route is NOT REGISTERED -- so the mux 404s it.
//
// A 404, never a 403. A 403 confirms the endpoint exists and is merely switched
// off, which tells a scanner exactly what to come back for and exactly which flag
// to hunt for a way to flip. A 404 is indistinguishable from a binary that was
// never built with the route at all.
//
// Note this is enforced TWICE, independently: mount() does not register the route,
// and auth.Service.HandleDevLogin also 404s if it is somehow reached. The flag
// itself is env/CLI only -- there is no request, no database column and no config
// file that can set it.
func TestDevLoginRouteDoesNotExistWhenDisabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled: the route does not exist", false, http.StatusNotFound},
		{"enabled: the route exists", true, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &API{
				store: fixtureStore(t), auth: devLoginAuth{enabled: tt.enabled}, keys: nil,
				log: discardLogger(), allowSelfServeOrgs: true, allowLocalSiteAdmins: true,
				metrics: nil, routes: nil,
			}

			mux := http.NewServeMux()
			a.mount(mux)

			r := httptest.NewRequest(http.MethodPost, Prefix+"/auth/dev-login", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}

			// And the route table itself must not even mention it when off.
			var registered bool

			for _, route := range a.routes {
				if route.Pattern == "POST "+Prefix+"/auth/dev-login" {
					registered = true
				}
			}

			if registered != tt.enabled {
				t.Errorf("dev-login registered = %v, want %v", registered, tt.enabled)
			}
		})
	}
}
