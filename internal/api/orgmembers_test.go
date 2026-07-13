package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// ---------------------------------------------------------------------------
// DB-backed helpers for the org-membership endpoints
//
// These tests are DB-backed rather than fake-backed, and they have to be: the
// grant is one transaction (fakeStore.Tx refuses on purpose), and the whole point
// of the hybrid model is what the DATABASE does with two source columns and a
// generated effective role. A fake would happily agree with a wrong answer.
// ---------------------------------------------------------------------------

// seedUser inserts a user who has never been near an org. This is the person an
// admin wants to add: provisioned by a login, member of nothing.
func (h *harness) seedUser(email, name string) pgtype.UUID {
	h.t.Helper()

	var id pgtype.UUID

	err := h.store.Pool().QueryRow(h.t.Context(),
		`INSERT INTO users (issuer, subject, email, display_name)
		 VALUES ('https://idp.test', $1, $1, $2) RETURNING id`,
		email, name).Scan(&id)
	if err != nil {
		h.t.Fatalf("seed user %q: %v", email, err)
	}

	return id
}

// seedOIDCMembership writes the CLAIM half directly, exactly as the reconciler
// would. The API cannot do this and must not be able to.
func (h *harness) seedOIDCMembership(userID, orgID pgtype.UUID, role repository.OrgRole, group string) {
	h.t.Helper()

	_, err := h.store.Pool().Exec(h.t.Context(),
		`INSERT INTO org_memberships (user_id, org_id, oidc_role, oidc_group)
		 VALUES ($1, $2, $3, $4)`,
		userID, orgID, role, group)
	if err != nil {
		h.t.Fatalf("seed oidc membership: %v", err)
	}
}

func (h *harness) orgID(slug string) pgtype.UUID {
	h.t.Helper()

	org, err := h.store.GetOrganizationBySlug(h.t.Context(), slug)
	if err != nil {
		h.t.Fatalf("get org %q: %v", slug, err)
	}

	return org.ID
}

// membership returns the WHOLE row -- both sources -- or reports it as gone. A
// missing row is an OUTCOME under test here, never a helper failure.
func (h *harness) membership(userID, orgID pgtype.UUID) (repository.OrgMembership, bool) {
	h.t.Helper()

	m, err := h.store.GetOrgMembership(h.t.Context(), repository.GetOrgMembershipParams{
		UserID: userID, OrgID: orgID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.OrgMembership{}, false
	}

	if err != nil {
		h.t.Fatalf("get org membership: %v", err)
	}

	return m, true
}

func decodeMember(t *testing.T, body []byte) Member {
	t.Helper()

	var m Member
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode member from %q: %v", body, err)
	}

	return m
}

// ---------------------------------------------------------------------------

// TestEndToEndLocalOrgGrant: PUT writes local_role with provenance, and the user
// is a member immediately -- no LDAP round trip, no next login.
func TestEndToEndLocalOrgGrant(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	marko := h.seedUser("marko@acme.dev", "Marko Ilic")
	devOrg := h.orgID(auth.DevOrgSlug)

	status, body := h.req(http.MethodPut,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", `{"role":"admin"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("grant: status = %d, want 200 (body %s)", status, body)
	}

	got := decodeMember(t, body)

	if got.OrgRole != string(auth.OrgRoleAdmin) {
		t.Errorf("org_role = %q, want admin", got.OrgRole)
	}

	if got.LocalRole != string(auth.OrgRoleAdmin) {
		t.Errorf("local_role = %q, want admin", got.LocalRole)
	}

	if got.OIDCRole != "" {
		t.Errorf("oidc_role = %q, want empty: a local grant is not a claim", got.OIDCRole)
	}

	if got.Source != OrgRoleSourceLocal {
		t.Errorf("org_role_source = %q, want %q", got.Source, OrgRoleSourceLocal)
	}

	if got.GrantedAt == nil {
		t.Error("granted_at is null: the grant has no provenance")
	}

	if got.GrantedByEmail != auth.DevEmail {
		t.Errorf("granted_by_email = %q, want %q", got.GrantedByEmail, auth.DevEmail)
	}

	// The database agrees, and the reconciler's half is untouched.
	m, ok := h.membership(marko, devOrg)
	if !ok {
		t.Fatal("no membership row was written")
	}

	if !m.LocalRole.Valid || m.LocalRole.OrgRole != auth.OrgRoleAdmin {
		t.Errorf("local_role = %+v, want admin", m.LocalRole)
	}

	if m.OidcRole.Valid {
		t.Errorf("the grant wrote oidc_role = %+v; the API must never touch the claim half", m.OidcRole)
	}

	if !m.GrantedAt.Valid || !m.GrantedBy.Valid {
		t.Errorf("provenance is missing: granted_by=%v granted_at=%v", m.GrantedBy, m.GrantedAt)
	}

	if m.Role != auth.OrgRoleAdmin {
		t.Errorf("effective role = %q, want admin", m.Role)
	}

	// And the roster shows it, with the source.
	status, listBody := h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/members", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list members: status = %d", status)
	}

	var listed ListResponse[Member]
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	var found bool

	for _, m := range listed.Items {
		if m.Email == "marko@acme.dev" {
			found = true

			if m.Source != OrgRoleSourceLocal || m.LocalRole != string(auth.OrgRoleAdmin) {
				t.Errorf("roster row = %+v, want a local admin grant", m)
			}
		}
	}

	if !found {
		t.Errorf("the new member is not on the roster: %s", listBody)
	}
}

// TestEndToEndDeleteALocalOnlyMembershipRemovesIt: nothing else justifies the
// membership, so the row goes -- and the response says the user is gone.
func TestEndToEndDeleteALocalOnlyMembershipRemovesIt(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	marko := h.seedUser("marko@acme.dev", "Marko Ilic")
	devOrg := h.orgID(auth.DevOrgSlug)

	if status, body := h.req(http.MethodPut,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", `{"role":"member"}`, nil); status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	status, body := h.req(http.MethodDelete,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", "", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke: status = %d, want 200 (body %s)", status, body)
	}

	var out OrgMemberRemoval
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !out.LocalRoleRevoked {
		t.Error("local_role_revoked = false, but a local grant was cleared")
	}

	if out.StillAMember {
		t.Error("still_a_member = true, but nothing justifies the membership any more")
	}

	if out.Membership != nil {
		t.Errorf("a surviving membership was reported for a removed member: %+v", out.Membership)
	}

	if _, ok := h.membership(marko, devOrg); ok {
		t.Error("the membership row survived a removal that had nothing else holding it up")
	}
}

// TestEndToEndDeleteLeavesAClaimDerivedMembershipStanding.
//
// THE ONE THAT INFURIATES AN OPERATOR IF IT IS WRONG. The user is in a mapped LDAP
// group AND holds a local grant. DELETE clears only the local half; the row
// survives and they are still a member. An admin who sees a bare 200/204 here and
// believes the user is gone has a security incident on their hands, so the response
// must say, in the body, that the membership survives and which group is holding it
// up.
func TestEndToEndDeleteLeavesAClaimDerivedMembershipStanding(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	marko := h.seedUser("marko@acme.dev", "Marko Ilic")
	devOrg := h.orgID(auth.DevOrgSlug)

	// The IdP says: member, by way of acme-engineering.
	h.seedOIDCMembership(marko, devOrg, auth.OrgRoleMember, "acme-engineering")

	// An admin locally promotes them to owner.
	status, body := h.req(http.MethodPut,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", `{"role":"owner"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	if got := decodeMember(t, body); got.Source != OrgRoleSourceBoth || got.OrgRole != string(auth.OrgRoleOwner) {
		t.Errorf("member = %+v, want source %q and effective role owner", got, OrgRoleSourceBoth)
	}

	// ...and then removes them.
	status, body = h.req(http.MethodDelete,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", "", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke: status = %d, want 200 (body %s)", status, body)
	}

	var out OrgMemberRemoval
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !out.LocalRoleRevoked {
		t.Error("local_role_revoked = false, but the local grant was cleared")
	}

	if !out.StillAMember {
		t.Fatalf("still_a_member = false, but the user IS still a member by claim. "+
			"An admin has just been told someone is gone who is not. body: %s", body)
	}

	if out.Membership == nil || out.Membership.OIDCGroup != "acme-engineering" {
		t.Errorf("the response does not name the group still holding the membership up: %s", body)
	}

	if out.Membership != nil && out.Membership.OrgRole != string(auth.OrgRoleMember) {
		t.Errorf("surviving org_role = %q, want member: the effective role must fall back "+
			"to the claim", out.Membership.OrgRole)
	}

	// The database: the row lives, the local half is gone, the claim half is intact.
	m, ok := h.membership(marko, devOrg)
	if !ok {
		t.Fatal("the row was DELETED while an OIDC claim still justified the membership")
	}

	if m.LocalRole.Valid {
		t.Errorf("local_role = %+v, want NULL", m.LocalRole)
	}

	if m.GrantedAt.Valid || m.GrantedBy.Valid {
		t.Error("the grant provenance survived the revocation")
	}

	if !m.OidcRole.Valid || m.OidcRole.OrgRole != auth.OrgRoleMember {
		t.Errorf("oidc_role = %+v, want member: the API clobbered the claim half", m.OidcRole)
	}

	if m.Role != auth.OrgRoleMember {
		t.Errorf("effective role = %q, want member", m.Role)
	}
}

// TestEndToEndDeleteAPurelyClaimDerivedMembershipIsRefused: LDAP owns it, the API
// cannot remove it, and it says so rather than pretending. A 200 here would be a
// lie -- the user would still be a member at the next request.
func TestEndToEndDeleteAPurelyClaimDerivedMembershipIsRefused(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	marko := h.seedUser("marko@acme.dev", "Marko Ilic")
	devOrg := h.orgID(auth.DevOrgSlug)
	h.seedOIDCMembership(marko, devOrg, auth.OrgRoleMember, "acme-engineering")

	status, body := h.req(http.MethodDelete,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/members/marko@acme.dev", "", nil)
	if status != http.StatusConflict {
		t.Fatalf("revoke: status = %d, want 409 (body %s)", status, body)
	}

	var envelope ErrorBody
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if envelope.Error.Code != CodeClaimDerived {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, CodeClaimDerived)
	}

	m, ok := h.membership(marko, devOrg)
	if !ok {
		t.Fatal("the refused DELETE removed the membership anyway")
	}

	if !m.OidcRole.Valid || m.Role != auth.OrgRoleMember {
		t.Errorf("the refused DELETE mutated the row: %+v", m)
	}
}

// TestGrantOrgRoleRequiresAnOrgAdmin: the authz matrix is intact, and a denied
// request writes NOTHING. An API-key principal is refused on the control plane
// entirely -- a delegation must not become a master key.
func TestGrantOrgRoleRequiresAnOrgAdmin(t *testing.T) {
	tests := []struct {
		role string
		want int
	}{
		{"site_admin", http.StatusOK},
		{"org_owner", http.StatusOK},
		{"org_admin", http.StatusOK},
		{"org_member", http.StatusForbidden},
		{"proj_admin", http.StatusForbidden},
		{"outsider", http.StatusNotFound},
		{"api_key", http.StatusForbidden},
		{"anonymous", http.StatusUnauthorized},
	}

	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)

			w := do(t, a, cast[tt.role], http.MethodPut,
				Prefix+"/orgs/acme/members/marko@acme.dev", `{"role":"member"}`)

			// The authorized cases reach the handler, which needs a transaction the
			// fake store refuses -- so they surface as a 500, not a 200. What is
			// asserted here is only the AUTHORIZATION boundary; the transactional
			// behaviour is asserted DB-backed above.
			if tt.want == http.StatusOK {
				if w.Code == http.StatusForbidden || w.Code == http.StatusNotFound ||
					w.Code == http.StatusUnauthorized {
					t.Fatalf("status = %d: an authorized caller was refused", w.Code)
				}

				return
			}

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.want, w.Body.String())
			}

			for _, call := range store.calls {
				if call == "Tx" {
					t.Errorf("a refused grant opened a transaction: %v", store.calls)
				}
			}
		})
	}
}
