package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/slug"
)

// do drives one request through a freshly-mounted route table with `p` as the
// principal, and returns the recorder. It is the workhorse of the endpoint tests.
func do(t *testing.T, a *API, p Principal, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	a.routes = nil
	a.mount(mux)

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}

	if p != nil {
		r = r.WithContext(withPrincipal(r.Context(), p))
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	return w
}

// decodeErr pulls the error envelope out of a response.
func decodeErr(t *testing.T, w *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()

	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope from %q: %v", w.Body.String(), err)
	}

	return body.Error
}

// TestCreateOrgRejectsReservedSlugs.
//
// The reserved list is the cache URL grammar itself. An org named `cache` makes
// /cache/cache/{project}/... ambiguous; `v2` collides with BuildKit's registry
// prefix, which lands AFTER /v2; `ac` and `cas` are the Bazel namespaces; `blobs`,
// `uploads` and `manifests` are OCI's.
//
// The list under test is slug.Reserved() itself, not a copy of it -- so a slug
// added to the denylist is automatically covered here, and one silently DROPPED
// from it fails this test rather than shipping. (internal/db, in turn, drives
// slug.Reserved() through the real bakery_slug_ok CHECK against a live Postgres,
// so the Go mirror cannot drift from the database either.)
func TestCreateOrgRejectsReservedSlugs(t *testing.T) {
	a := testAPI(t, fixtureStore(t), nil)
	admin := principals(t)["site_admin"]

	for _, reserved := range slug.Reserved() {
		t.Run(reserved, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)

			w := do(t, a, admin, http.MethodPost, Prefix+"/orgs",
				`{"slug":"`+reserved+`","name":"x"}`)

			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want %d for reserved slug %q",
					w.Code, http.StatusUnprocessableEntity, reserved)
			}

			if got := decodeErr(t, w).Code; got != CodeReservedSlug {
				t.Errorf("error code = %q, want %q", got, CodeReservedSlug)
			}

			// And nothing was written. A 422 that still created the org would be a
			// far worse bug than the 500 it is pretending not to be.
			for _, call := range store.calls {
				if strings.HasPrefix(call, "CreateOrganization") {
					t.Errorf("the store was written to despite the rejection: %v", store.calls)
				}
			}
		})
	}

	// The control: a perfectly ordinary slug is accepted, so the test above is not
	// passing merely because creation is broken.
	t.Run("a normal slug is accepted", func(t *testing.T) {
		store := fixtureStore(t)
		a := testAPI(t, store, nil)

		w := do(t, a, admin, http.MethodPost, Prefix+"/orgs", `{"slug":"widgets","name":"Widgets"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body %s)", w.Code, http.StatusCreated, w.Body.String())
		}
	})

	_ = a
}

// TestCreateProjectRejectsReservedSlugs: the same rule, the same list, the other
// half of the path. A project named `sstate` or `ac` breaks the grammar just as
// thoroughly as an org named `cache`.
func TestCreateProjectRejectsReservedSlugs(t *testing.T) {
	orgAdmin := principals(t)["org_admin"]

	for _, reserved := range slug.Reserved() {
		t.Run(reserved, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)

			w := do(t, a, orgAdmin, http.MethodPost, Prefix+"/orgs/acme/projects",
				`{"slug":"`+reserved+`","name":"x"}`)

			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want %d for reserved slug %q",
					w.Code, http.StatusUnprocessableEntity, reserved)
			}

			if got := decodeErr(t, w).Code; got != CodeReservedSlug {
				t.Errorf("error code = %q, want %q", got, CodeReservedSlug)
			}
		})
	}
}

// TestCreateOrgRejectsMalformedSlugs covers the grammar, not the denylist.
func TestCreateOrgRejectsMalformedSlugs(t *testing.T) {
	tests := []struct {
		name string
		slug string
	}{
		{"empty", ""},
		{"uppercase", "Acme"},
		{"leading hyphen", "-acme"},
		{"trailing hyphen", "acme-"},
		{"underscore", "ac_me"},
		{"slash: would forge a path segment", "acme/evil"},
		{"space", "ac me"},
		{"too long", strings.Repeat("a", 64)},
	}

	admin := principals(t)["site_admin"]

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAPI(t, fixtureStore(t), nil)

			body, err := json.Marshal(CreateOrgRequest{Slug: tt.slug, Name: "x"})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			w := do(t, a, admin, http.MethodPost, Prefix+"/orgs", string(body))

			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
			}

			if got := decodeErr(t, w).Code; got != CodeInvalidSlug {
				t.Errorf("error code = %q, want %q", got, CodeInvalidSlug)
			}
		})
	}
}

// TestListOrgsIsScopedToTheCaller: the list endpoint returns what you may see, not
// everything with a client-side filter.
func TestListOrgsIsScopedToTheCaller(t *testing.T) {
	tests := []struct {
		name string
		role string
		want []string
	}{
		{"a site admin sees every org", "site_admin", []string{"acme", "other"}},
		{"an acme member sees only acme", "org_member", []string{"acme"}},
		{"an outsider sees only their own org", "outsider", []string{"other"}},
	}

	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAPI(t, fixtureStore(t), nil)

			w := do(t, a, cast[tt.role], http.MethodGet, Prefix+"/orgs", "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}

			var body ListResponse[Org]
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			got := make([]string, 0, len(body.Items))
			for _, o := range body.Items {
				got = append(got, o.Slug)
			}

			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("orgs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOrgRolesAreNotEditable: an org membership write is refused with 409, because
// the OIDC reconciler owns that table and would revert the edit at the user's next
// login -- silently, hours later.
func TestOrgRolesAreNotEditable(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{"PUT is refused", http.MethodPut},
		{"DELETE is refused", http.MethodDelete},
	}

	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)

			body := ""
			if tt.method == http.MethodPut {
				body = `{"role":"owner"}`
			}

			w := do(t, a, cast["org_admin"], tt.method,
				Prefix+"/orgs/acme/members/marko@acme.dev", body)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
			}

			if got := decodeErr(t, w).Code; got != CodeClaimDerived {
				t.Errorf("error code = %q, want %q", got, CodeClaimDerived)
			}

			if len(store.calls) != 0 {
				t.Errorf("the store was written to: %v", store.calls)
			}
		})
	}

	// And an UNAUTHORIZED caller does not even learn that: they get the ordinary
	// authorization answer, not a helpful 409 that confirms the org exists.
	t.Run("an unauthorized caller gets the authz answer, not the 409", func(t *testing.T) {
		a := testAPI(t, fixtureStore(t), nil)

		w := do(t, a, cast["org_member"], http.MethodPut,
			Prefix+"/orgs/acme/members/marko@acme.dev", `{"role":"owner"}`)

		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})
}
