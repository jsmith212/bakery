package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// TestEndToEndCreateOrgThenProjectThenKey is THE BUG THIS MILESTONE EXISTS FOR.
//
// Before the creator got a local owner grant, this flow dead-ended at the third
// step, and it dead-ended for a site admin: creating an org granted no membership
// in it (there is no group mapping for a brand-new org, and there could not be),
// so the creator was not a project member, so the API-key scope cap -- which is
// capped at the caller's PROJECT role at grant time -- refused them with
// `scope_exceeds_role`. A site admin could create an org they could not use, and
// the only fix was an LDAP round trip or a psql session.
//
// The whole flow, end to end, through the real chain.
func TestEndToEndCreateOrgThenProjectThenKey(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	status, body := h.req(http.MethodPost, Prefix+"/orgs", `{"slug":"widgets","name":"Widgets"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create org: status = %d, body %s", status, body)
	}

	var org Org
	if err := json.Unmarshal(body, &org); err != nil {
		t.Fatalf("decode org: %v", err)
	}

	// The response already says the creator owns it -- Org.Role is the CALLER's role,
	// and a console that renders the org's admin controls reads exactly this.
	if org.Role != string(auth.OrgRoleOwner) {
		t.Errorf("the creator's role on the new org = %q, want owner", org.Role)
	}

	status, body = h.req(http.MethodPost, Prefix+"/orgs/widgets/projects",
		`{"slug":"firmware","name":"Firmware"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create project: status = %d, body %s", status, body)
	}

	// THE STEP THAT USED TO FAIL. A write key needs a project role of writer or
	// better; the creator has one only because creating the org granted them a local
	// OWNER role in it.
	status, body = h.req(http.MethodPost, Prefix+"/orgs/widgets/projects/firmware/keys",
		`{"name":"ci","scope":"write"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("MINT A KEY IN THE ORG YOU JUST CREATED: status = %d, body %s\n"+
			"This is the dead-end M1.5 exists to abolish: the creator holds no membership "+
			"in the org they made, so the scope cap refuses them.", status, body)
	}

	var key CreatedAPIKey
	if err := json.Unmarshal(body, &key); err != nil {
		t.Fatalf("decode key: %v", err)
	}

	// And the key WORKS -- the flow is complete, not merely un-refused.
	if status, meBody := h.req(http.MethodGet, Prefix+"/me", "",
		map[string]string{"Authorization": "Bearer " + key.Token}); status != http.StatusOK {
		t.Fatalf("the minted key does not authenticate: status = %d, body %s", status, meBody)
	}
}

// TestEndToEndOrgCreationGrantsTheCreatorALocalOwnerRole: the grant is LOCAL, with
// provenance, and the claim half is untouched -- so the next login reconciles the
// OIDC half as usual and this grant survives it.
func TestEndToEndOrgCreationGrantsTheCreatorALocalOwnerRole(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	if status, body := h.req(http.MethodPost, Prefix+"/orgs",
		`{"slug":"widgets","name":"Widgets"}`, nil); status != http.StatusCreated {
		t.Fatalf("create org: status = %d, body %s", status, body)
	}

	widgets := h.orgID("widgets")

	var creator pgtype.UUID
	if err := h.store.Pool().QueryRow(t.Context(),
		`SELECT id FROM users WHERE email = $1`, auth.DevEmail).Scan(&creator); err != nil {
		t.Fatalf("find the creator: %v", err)
	}

	m, ok := h.membership(creator, widgets)
	if !ok {
		t.Fatal("the creator holds NO membership in the org they just created")
	}

	if !m.LocalRole.Valid || m.LocalRole.OrgRole != auth.OrgRoleOwner {
		t.Errorf("local_role = %+v, want owner", m.LocalRole)
	}

	if m.OidcRole.Valid {
		t.Errorf("oidc_role = %+v: org creation forged a CLAIM. No group says this.", m.OidcRole)
	}

	if !m.GrantedBy.Valid || m.GrantedBy != creator {
		t.Errorf("granted_by = %v, want the creator %v", m.GrantedBy, creator)
	}

	if !m.GrantedAt.Valid {
		t.Error("granted_at is NULL: the grant has no provenance")
	}

	if m.Role != auth.OrgRoleOwner {
		t.Errorf("effective role = %q, want owner", m.Role)
	}
}

// TestOrgCreationAndTheOwnerGrantAreOneTransaction.
//
// A crash between the two would leave EXACTLY the orphaned, unusable org this
// milestone exists to abolish -- an org with no members, which no API call can ever
// add one to, because adding a member requires being an admin of it.
//
// The assertion is structural: the handler must not call store.CreateOrganization
// at all. It creates the org through the transaction's Queries, so a failed
// transaction leaves NO org behind. fakeStore.Tx refuses (a fake cannot demonstrate
// atomicity), which is precisely the failure this test drives.
func TestOrgCreationAndTheOwnerGrantAreOneTransaction(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, nil)

	w := do(t, a, principals(t)["site_admin"], http.MethodPost, Prefix+"/orgs",
		`{"slug":"widgets","name":"Widgets"}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: the fake's Tx refuses, so a handler that "+
			"went THROUGH the transaction must fail here. A 201 means the org was "+
			"created outside it.", w.Code)
	}

	var sawTx bool

	for _, call := range store.calls {
		if call == "Tx" {
			sawTx = true
		}

		if strings.HasPrefix(call, "CreateOrganization") {
			t.Errorf("the org was created OUTSIDE the transaction (%v): a crash before the "+
				"owner grant leaves an org nobody can ever join", store.calls)
		}
	}

	if !sawTx {
		t.Errorf("no transaction was opened: %v", store.calls)
	}

	for _, o := range store.orgs {
		if o.Slug == "widgets" {
			t.Error("the failed transaction left an org behind")
		}
	}
}

// TestSelfServeOrgsCanBeTurnedOff: with --allow-self-serve-orgs=false, creating an
// org is site-admin only again. The flag restricts; it never widens.
func TestSelfServeOrgsCanBeTurnedOff(t *testing.T) {
	tests := []struct {
		name      string
		selfServe bool
		role      string
		wantDeny  bool
	}{
		{"self-serve on: an ordinary user may create an org", true, "org_member", false},
		{"self-serve on: a site admin may too", true, "site_admin", false},
		{"self-serve OFF: an ordinary user is refused", false, "org_member", true},
		{"self-serve OFF: a site admin still may", false, "site_admin", false},
		{"an API key may never create an org", true, "api_key", true},
	}

	cast := principals(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)
			a.allowSelfServeOrgs = tt.selfServe

			w := do(t, a, cast[tt.role], http.MethodPost, Prefix+"/orgs", `{"slug":"widgets"}`)

			if tt.wantDeny {
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
				}

				if len(store.calls) != 0 {
					t.Errorf("a refused creation touched the store: %v", store.calls)
				}

				return
			}

			// Permitted: the handler runs and dies in the fake's transaction. What is
			// asserted here is only that it was not REFUSED.
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d: an authorized creator was refused (body %s)",
					w.Code, w.Body.String())
			}
		})
	}
}
