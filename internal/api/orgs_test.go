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

			detail := decodeErr(t, w)
			if detail.Code != CodeReservedSlug {
				t.Errorf("error code = %q, want %q", detail.Code, CodeReservedSlug)
			}

			// The message must name the OFFENDING SLUG, not the field. The bug was
			// fmt.Sprintf(..., field), which produced the self-referential nonsense
			// `"slug" is reserved`, telling the user the word "slug" is reserved
			// rather than that "cache" is.
			if !strings.Contains(detail.Message, `"`+reserved+`"`) {
				t.Errorf("message %q does not name the offending slug %q", detail.Message, reserved)
			}

			if strings.Contains(detail.Message, `"slug"`) && reserved != "slug" {
				t.Errorf("message %q names the field, not the value: %q", detail.Message, reserved)
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

// TestTheClaimHalfOfAnOrgRoleIsStillNotEditable.
//
// This test was TestOrgRolesAreNotEditable, and it asserted that BOTH writes to an
// org membership were a 409: in M1 an org role was 100% claim-derived, so a
// hand-edit would have been silently reverted at the user's next login. It is
// INVERTED rather than deleted, so what changed in M1.5 -- and what did NOT -- is
// recorded rather than erased.
//
// What changed: an org role now has a LOCAL half, which the reconciler cannot touch,
// so PUT grants one and it survives the next login (asserted DB-backed in
// orgmembers_test.go -- the hybrid model lives in the schema and only a real
// Postgres can prove it).
//
// What did NOT change: the CLAIM half is still owned by the IdP, and a DELETE
// against a purely claim-derived membership is still a 409 with the same code. The
// API cannot remove a membership LDAP is holding up, and it must not pretend it can.
func TestTheClaimHalfOfAnOrgRoleIsStillNotEditable(t *testing.T) {
	cast := principals(t)

	// marko's fixture membership is claim-derived (fakeStore's GetOrgMembership
	// reports oidc_role set, local_role NULL), so there is no local grant to remove.
	t.Run("DELETE against a claim-derived membership is still a 409", func(t *testing.T) {
		store := fixtureStore(t)
		a := testAPI(t, store, nil)

		w := do(t, a, cast["org_admin"], http.MethodDelete,
			Prefix+"/orgs/acme/members/marko@acme.dev", "")

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
		}

		if got := decodeErr(t, w).Code; got != CodeClaimDerived {
			t.Errorf("error code = %q, want %q", got, CodeClaimDerived)
		}

		// The read happened; no WRITE did. A 409 that had already deleted the row is
		// exactly the failure a status-code-only assertion cannot see.
		for _, call := range store.calls {
			if call != "GetOrgMembership" {
				t.Errorf("the refused DELETE wrote to the store: %v", store.calls)
			}
		}
	})

	// An org ADMIN may grant, but may not mint an OWNER: an owner can delete the org
	// and everything cached in it, so that would be a self-service escalation to an
	// authority the granter does not hold.
	t.Run("an org admin may not grant the owner role", func(t *testing.T) {
		store := fixtureStore(t)
		a := testAPI(t, store, nil)

		w := do(t, a, cast["org_admin"], http.MethodPut,
			Prefix+"/orgs/acme/members/marko@acme.dev", `{"role":"owner"}`)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d (body %s)", w.Code, http.StatusForbidden, w.Body.String())
		}

		if len(store.calls) != 0 {
			t.Errorf("the refused grant wrote to the store: %v", store.calls)
		}
	})

	// And an UNAUTHORIZED caller learns nothing from any of it: they get the ordinary
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
