package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// ---------------------------------------------------------------------------
// The hybrid SITE role.
//
// These are DB-backed because the model IS the database's: two source columns and
// an effective role generated as coalesce(greatest(oidc, local), 'user'). A fake
// that agreed with the handler would prove nothing about the one thing that matters
// here -- that the two halves cannot clobber each other.
// ---------------------------------------------------------------------------

// siteAdmins fetches the listing through the real chain.
func (h *harness) siteAdmins() []SiteAdmin {
	h.t.Helper()

	status, body := h.req(http.MethodGet, Prefix+"/site-admins", "", nil)
	if status != http.StatusOK {
		h.t.Fatalf("list site admins: status = %d, body %s", status, body)
	}

	var listed ListResponse[SiteAdmin]
	if err := json.Unmarshal(body, &listed); err != nil {
		h.t.Fatalf("decode site admins from %q: %v", body, err)
	}

	return listed.Items
}

// user reads the whole users row, both sources of the site role included.
func (h *harness) user(email string) repository.User {
	h.t.Helper()

	u, err := h.store.GetUserByEmail(h.t.Context(), email)
	if err != nil {
		h.t.Fatalf("get user %q: %v", email, err)
	}

	return u
}

func siteAdminOf(t *testing.T, admins []SiteAdmin, email string) (SiteAdmin, bool) {
	t.Helper()

	for _, a := range admins {
		if a.Email == email {
			return a, true
		}
	}

	return SiteAdmin{}, false
}

func decodeSiteAdmin(t *testing.T, body []byte) SiteAdmin {
	t.Helper()

	var a SiteAdmin
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode site admin from %q: %v", body, err)
	}

	return a
}

// ---------------------------------------------------------------------------

// TestEndToEndLocalSiteAdminGrant: a site admin grants another user the site-admin
// role, the grant carries provenance, and THE LISTING REPORTS THE SOURCE OF EACH.
//
// The listing assertion is the one that matters. A hybrid site role is a trade, and
// the trade is only sound while it is visible: the console has to be able to say
// `ldap: dev-login` for the claim-derived admin and `local: granted by dev@...` for
// the granted one. Reporting only the effective role would render the two
// identically, which is what turns a local grant into a backdoor.
func TestEndToEndLocalSiteAdminGrant(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	h.seedUser("marko@acme.dev", "Marko Ilic")

	status, body := h.req(http.MethodPut, Prefix+"/site-admins/marko@acme.dev", "", nil)
	if status != http.StatusOK {
		t.Fatalf("grant: status = %d, want 200 (body %s)", status, body)
	}

	got := decodeSiteAdmin(t, body)

	if got.SiteRole != string(auth.SiteRoleAdmin) {
		t.Errorf("site_role = %q, want admin", got.SiteRole)
	}

	if got.LocalRole != string(auth.SiteRoleAdmin) {
		t.Errorf("site_role_local = %q, want admin", got.LocalRole)
	}

	if got.OIDCRole != "" {
		t.Errorf("site_role_oidc = %q, want empty: a local grant is not a claim, and forging "+
			"one would be reconciled away at the user's next login", got.OIDCRole)
	}

	if got.Source != SiteRoleSourceLocal {
		t.Errorf("site_role_source = %q, want %q", got.Source, SiteRoleSourceLocal)
	}

	if got.GrantedAt == nil {
		t.Error("granted_at is null: the grant has no provenance")
	}

	if got.GrantedByEmail != auth.DevEmail {
		t.Errorf("granted_by_email = %q, want %q", got.GrantedByEmail, auth.DevEmail)
	}

	// The database agrees, and the claim half was not touched.
	marko := h.user("marko@acme.dev")

	if !marko.SiteRoleLocal.Valid || marko.SiteRoleLocal.SiteRole != auth.SiteRoleAdmin {
		t.Errorf("site_role_local = %+v, want admin", marko.SiteRoleLocal)
	}

	if marko.SiteRoleOidc.Valid {
		t.Errorf("the grant wrote site_role_oidc = %+v; the API must never forge a claim",
			marko.SiteRoleOidc)
	}

	if !marko.SiteGrantedBy.Valid || !marko.SiteGrantedAt.Valid {
		t.Errorf("provenance is missing: granted_by=%v granted_at=%v",
			marko.SiteGrantedBy, marko.SiteGrantedAt)
	}

	if marko.SiteRole != auth.SiteRoleAdmin {
		t.Errorf("effective site_role = %q, want admin", marko.SiteRole)
	}

	// THE LISTING, WITH THE SOURCE OF EACH ADMIN. Both admins are on it, and they are
	// distinguishable -- which is the entire mitigation.
	admins := h.siteAdmins()

	dev, ok := siteAdminOf(t, admins, auth.DevEmail)
	if !ok {
		t.Fatalf("the claim-derived site admin is not on the listing: %+v", admins)
	}

	if dev.Source != SiteRoleSourceOIDC {
		t.Errorf("the dev admin's site_role_source = %q, want %q", dev.Source, SiteRoleSourceOIDC)
	}

	if dev.OIDCGroup != auth.DevLoginGroup {
		t.Errorf("the dev admin's site_oidc_group = %q, want %q -- the listing cannot say "+
			"`ldap: <group>` if it does not know the group", dev.OIDCGroup, auth.DevLoginGroup)
	}

	if dev.LocalRole != "" || dev.GrantedAt != nil {
		t.Errorf("the claim-derived admin is reported as locally granted: %+v", dev)
	}

	listed, ok := siteAdminOf(t, admins, "marko@acme.dev")
	if !ok {
		t.Fatalf("the locally-granted site admin is not on the listing: %+v", admins)
	}

	if listed.Source != SiteRoleSourceLocal {
		t.Errorf("the local admin's site_role_source = %q, want %q", listed.Source, SiteRoleSourceLocal)
	}

	if listed.GrantedByEmail != auth.DevEmail || listed.GrantedAt == nil {
		t.Errorf("the listing does not say WHO granted this admin, or WHEN: %+v", listed)
	}

	if listed.OIDCGroup != "" {
		t.Errorf("a local grant is reported as coming from group %q", listed.OIDCGroup)
	}
}

// TestEndToEndALocalSiteAdminGrantOUTLIVESTheLDAPGroupAndIsVISIBLE.
//
// THE ONE REAL RISK IN THE HYBRID DESIGN, and the test that says the mitigation
// works. A user is a site admin by group claim AND by local grant. The IdP then
// revokes the group -- the next login reconciles site_role_oidc to NULL. The local
// grant survives, so THE USER IS STILL A SITE ADMIN even though the directory says
// they are not.
//
// That state is inherent in having two sources; it cannot be designed away without
// giving up the bootstrap. So the requirement is not that it cannot happen -- it is
// that it CANNOT HAPPEN INVISIBLY. The listing must show the survivor as `local`,
// with who granted it and when, so a review can find it. A backdoor you can see on a
// screen is not much of a backdoor; an identical-looking row is one.
func TestEndToEndALocalSiteAdminGrantOutlivesTheLDAPGroupAndIsVisible(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	// The dev user is a site admin by CLAIM (site_role_oidc, group dev-login). Give
	// them a local grant as well: now both halves justify the role.
	status, body := h.req(http.MethodPut, Prefix+"/site-admins/"+auth.DevEmail, "", nil)
	if status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	if got := decodeSiteAdmin(t, body); got.Source != SiteRoleSourceBoth {
		t.Fatalf("site_role_source = %q, want %q: both halves justify this admin",
			got.Source, SiteRoleSourceBoth)
	}

	// THE IdP REVOKES THE GROUP. This is exactly what the login reconciler does with
	// the claim half when a user drops out of site_admin_groups: site_role_oidc goes
	// to NULL. It writes nothing else -- it does not name the local column.
	err := h.store.Tx(t.Context(), func(q *repository.Queries) error {
		_, err := q.UpsertUser(t.Context(), repository.UpsertUserParams{
			Issuer: auth.DevIssuer, Subject: auth.DevSubject,
			Email: auth.DevEmail, DisplayName: "Dev",
			SiteRole:  auth.SiteRoleUser,
			SiteGroup: pgtype.Text{String: "", Valid: false},
		})

		return err
	})
	if err != nil {
		t.Fatalf("reconcile the claim half away: %v", err)
	}

	dev := h.user(auth.DevEmail)

	if dev.SiteRoleOidc.Valid {
		t.Fatalf("site_role_oidc = %+v after the group was revoked, want NULL", dev.SiteRoleOidc)
	}

	// The user is STILL a site admin -- by the local grant alone. This is the state the
	// design accepts; it is not a bug, and hiding it would be.
	if dev.SiteRole != auth.SiteRoleAdmin {
		t.Fatalf("effective site_role = %q, want admin: the local grant must SURVIVE the "+
			"LDAP revocation -- otherwise the reconciler is clobbering the half it does not own",
			dev.SiteRole)
	}

	// ...AND IT IS VISIBLE. The listing now reports them as `local`, with the granter
	// and the date, and no group. An access review can see, on a screen, that this
	// person's platform admin is held up by an in-app grant and not by the directory.
	admins := h.siteAdmins()

	survivor, ok := siteAdminOf(t, admins, auth.DevEmail)
	if !ok {
		t.Fatalf("the surviving site admin is not on the listing at all: %+v", admins)
	}

	if survivor.Source != SiteRoleSourceLocal {
		t.Errorf("site_role_source = %q, want %q. A local grant that outlived the LDAP "+
			"revocation is reported as claim-derived: it is now INVISIBLE, which is the "+
			"definition of a backdoor.", survivor.Source, SiteRoleSourceLocal)
	}

	if survivor.OIDCGroup != "" || survivor.OIDCRole != "" {
		t.Errorf("the listing still claims a group justifies this admin: %+v", survivor)
	}

	if survivor.GrantedAt == nil || survivor.GrantedByEmail != auth.DevEmail {
		t.Errorf("the listing does not say who granted the surviving role, or when: %+v", survivor)
	}
}

// TestEndToEndRevokeALocalSiteAdminGrant: nothing else holds the role up, so the
// user is demoted.
func TestEndToEndRevokeALocalSiteAdminGrant(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	h.seedUser("marko@acme.dev", "Marko Ilic")

	if status, body := h.req(http.MethodPut,
		Prefix+"/site-admins/marko@acme.dev", "", nil); status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	status, body := h.req(http.MethodDelete, Prefix+"/site-admins/marko@acme.dev", "", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke: status = %d, want 200 (body %s)", status, body)
	}

	var out SiteAdminRemoval
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !out.LocalRoleRevoked {
		t.Error("local_role_revoked = false, but a local grant was cleared")
	}

	if out.StillASiteAdmin {
		t.Error("still_a_site_admin = true, but nothing justifies the role any more")
	}

	marko := h.user("marko@acme.dev")

	if marko.SiteRole != auth.SiteRoleUser {
		t.Errorf("effective site_role = %q, want user", marko.SiteRole)
	}

	if marko.SiteGrantedAt.Valid || marko.SiteGrantedBy.Valid {
		t.Error("the grant provenance survived the revocation")
	}

	// And they are off the listing.
	if _, ok := siteAdminOf(t, h.siteAdmins(), "marko@acme.dev"); ok {
		t.Error("a demoted admin is still on the site-admin listing")
	}
}

// TestEndToEndRevokingALocalGrantLeavesAClaimDerivedSiteAdminStanding.
//
// THE ONE THAT MUST NOT LIE. The user is a site admin by group claim AND by local
// grant. DELETE clears only the local half -- and they are STILL A SITE ADMIN, with
// every privilege in the installation. An operator who sees a bare 204 and believes
// they have demoted someone who is still an admin has a security incident.
func TestEndToEndRevokingALocalGrantLeavesAClaimDerivedSiteAdminStanding(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	// The dev user is a site admin by claim; give them a local grant on top.
	if status, body := h.req(http.MethodPut,
		Prefix+"/site-admins/"+auth.DevEmail, "", nil); status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	status, body := h.req(http.MethodDelete, Prefix+"/site-admins/"+auth.DevEmail, "", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke: status = %d, want 200 (body %s)", status, body)
	}

	var out SiteAdminRemoval
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !out.StillASiteAdmin {
		t.Fatalf("still_a_site_admin = false, but the user IS still a site admin by claim. "+
			"An operator has just been told they demoted someone who still administers the "+
			"whole installation. body: %s", body)
	}

	if out.Admin == nil || out.Admin.OIDCGroup != auth.DevLoginGroup {
		t.Errorf("the response does not name the group still holding the role up: %s", body)
	}

	if out.Admin != nil && out.Admin.Source != SiteRoleSourceOIDC {
		t.Errorf("surviving site_role_source = %q, want %q", out.Admin.Source, SiteRoleSourceOIDC)
	}

	// The database: the local half is gone, the claim half is intact, and the effective
	// role is still admin.
	dev := h.user(auth.DevEmail)

	if dev.SiteRoleLocal.Valid {
		t.Errorf("site_role_local = %+v, want NULL", dev.SiteRoleLocal)
	}

	if !dev.SiteRoleOidc.Valid || dev.SiteRoleOidc.SiteRole != auth.SiteRoleAdmin {
		t.Errorf("site_role_oidc = %+v: the revoke clobbered the claim half", dev.SiteRoleOidc)
	}

	if dev.SiteRole != auth.SiteRoleAdmin {
		t.Errorf("effective site_role = %q, want admin", dev.SiteRole)
	}
}

// TestEndToEndRevokingAPurelyClaimDerivedSiteAdminIsRefused: LDAP owns it, the API
// cannot remove it, and it says so rather than pretending. A 200 here would be the
// same lie as a bare 204 above -- the user administers the installation either way.
func TestEndToEndRevokingAPurelyClaimDerivedSiteAdminIsRefused(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	status, body := h.req(http.MethodDelete, Prefix+"/site-admins/"+auth.DevEmail, "", nil)
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

	if dev := h.user(auth.DevEmail); dev.SiteRole != auth.SiteRoleAdmin || !dev.SiteRoleOidc.Valid {
		t.Errorf("the refused revoke mutated the site role: %+v", dev)
	}
}

// TestEndToEndRevokeIsNotGatedByTheFlag.
//
// --allow-local-site-admins=false closes the GRANT path. It must NOT close the
// revoke path: the day an operator turns the flag off is exactly the day they need to
// clean up the local grants that predate it, and a lockdown switch that entrenches
// what it was meant to remove is worse than no switch.
func TestEndToEndRevokeIsNotGatedByTheFlag(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	h.seedUser("marko@acme.dev", "Marko Ilic")

	if status, body := h.req(http.MethodPut,
		Prefix+"/site-admins/marko@acme.dev", "", nil); status != http.StatusOK {
		t.Fatalf("grant: status = %d, body %s", status, body)
	}

	// The installation is locked down AFTER the grant was made.
	h.api.allowLocalSiteAdmins = false

	// A new grant is refused...
	if status, _ := h.req(http.MethodPut,
		Prefix+"/site-admins/marko@acme.dev", "", nil); status != http.StatusForbidden {
		t.Errorf("grant with the flag off: status = %d, want 403", status)
	}

	// ...and the pre-existing one can still be cleaned up.
	status, body := h.req(http.MethodDelete, Prefix+"/site-admins/marko@acme.dev", "", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke with the flag off: status = %d, want 200 (body %s).\n"+
			"Turning off local site admins must not strand the local site admins that already "+
			"exist -- that is precisely who the operator is trying to remove.", status, body)
	}

	if marko := h.user("marko@acme.dev"); marko.SiteRole != auth.SiteRoleUser {
		t.Errorf("effective site_role = %q, want user", marko.SiteRole)
	}
}

// ---------------------------------------------------------------------------
// The authorization boundary, on the fakes.
// ---------------------------------------------------------------------------

// TestLocalSiteAdminGrantsCanBeTurnedOff: with --allow-local-site-admins=false the
// path is closed for EVERYONE, site admins included -- and a refused grant touches
// the store not at all.
func TestLocalSiteAdminGrantsCanBeTurnedOff(t *testing.T) {
	tests := []struct {
		name     string
		allow    bool
		wantDeny bool
	}{
		{"on: a site admin may grant a local site-admin role", true, false},
		{"OFF: even a site admin is refused", false, true},
	}

	admin := principals(t)["site_admin"]

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)
			a.allowLocalSiteAdmins = tt.allow

			w := do(t, a, admin, http.MethodPut, Prefix+"/site-admins/marko@acme.dev", "")

			if tt.wantDeny {
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
				}

				if len(store.calls) != 0 {
					t.Errorf("a refused grant touched the store: %v", store.calls)
				}

				return
			}

			// Permitted: the handler runs and dies in the fake's hybrid write. What is
			// asserted here is only that it was not REFUSED.
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d: an authorized site admin was refused (body %s)",
					w.Code, w.Body.String())
			}

			var reached bool

			for _, call := range store.calls {
				if call == "GrantSiteAdminLocal" {
					reached = true
				}
			}

			if !reached {
				t.Errorf("the grant never reached the store: %v", store.calls)
			}
		})
	}
}

// TestOnlyASiteAdminCanGrantASiteRole, AND AN API KEY NEVER CAN.
//
// The key case is the one with teeth. An API key is a DELEGATION -- one project, one
// scope, minted so a CI runner can push to a cache. If it could grant a site role it
// would be a master key with a succession plan: a leaked CI token would buy the whole
// installation, permanently, and revoking the token would not take the site admin
// back.
//
// Two independent things refuse it, and this asserts the outcome of both: the guard
// admits a key to AccessAuthenticated and nowhere else, and an API-key principal's
// IsSiteAdmin() is false even when the human who minted it IS a site admin (the
// fixture key's owner is one, deliberately).
func TestOnlyASiteAdminCanGrantASiteRole(t *testing.T) {
	tests := []struct {
		role string
		want int
	}{
		{"site_admin", http.StatusOK},
		{"org_owner", http.StatusForbidden},
		{"org_admin", http.StatusForbidden},
		{"org_member", http.StatusForbidden},
		{"proj_admin", http.StatusForbidden},
		{"outsider", http.StatusForbidden},
		{"api_key", http.StatusForbidden},
		{"anonymous", http.StatusUnauthorized},
	}

	cast := principals(t)

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, tt := range tests {
			t.Run(method+"/"+tt.role, func(t *testing.T) {
				store := fixtureStore(t)
				a := testAPI(t, store, nil)
				a.allowLocalSiteAdmins = true

				w := do(t, a, cast[tt.role], method, Prefix+"/site-admins/marko@acme.dev", "")

				if tt.want == http.StatusOK {
					if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
						t.Fatalf("status = %d: an authorized site admin was refused", w.Code)
					}

					return
				}

				if w.Code != tt.want {
					t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.want, w.Body.String())
				}

				// The stronger assertion: a refused caller did not reach the write. A 403
				// that granted the site role anyway and merely lied about the status is
				// exactly what a status-only test cannot see.
				for _, call := range store.calls {
					if call == "GrantSiteAdminLocal" || call == "RevokeSiteAdminLocal" {
						t.Errorf("a refused caller reached the site-role write: %v", store.calls)
					}
				}
			})
		}
	}
}

// TestListingSiteAdminsRequiresASiteAdmin: the listing names every administrator of
// the installation and how each got there. It is not a public directory.
func TestListingSiteAdminsRequiresASiteAdmin(t *testing.T) {
	tests := map[string]int{
		"site_admin": http.StatusOK,
		"org_owner":  http.StatusForbidden,
		"org_member": http.StatusForbidden,
		"api_key":    http.StatusForbidden,
		"anonymous":  http.StatusUnauthorized,
	}

	cast := principals(t)

	for role, want := range tests {
		t.Run(role, func(t *testing.T) {
			a := testAPI(t, fixtureStore(t), nil)

			w := do(t, a, cast[role], http.MethodGet, Prefix+"/site-admins", "")
			if w.Code != want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, want, w.Body.String())
			}
		})
	}
}
