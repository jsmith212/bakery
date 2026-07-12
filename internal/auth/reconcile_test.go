package auth

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestReconcileJITProvisions: a user Bakery has never seen logs in, and comes out
// the other side with a user row, the orgs their groups map to, and the roles
// those groups grant.
func TestReconcileJITProvisions(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-devs"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	user, err := ts.store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	if user.Email != "dev@acme.example" {
		t.Errorf("Email = %q, want dev@acme.example", user.Email)
	}

	// The identity key is (issuer, subject), never email.
	if user.Issuer != "https://idp.example.com" || user.Subject != "s1" {
		t.Errorf("identity = (%q, %q), want the issuer/subject pair", user.Issuer, user.Subject)
	}

	if user.SiteRole != SiteRoleUser {
		t.Errorf("SiteRole = %q, want %q (they are in no site-admin group)", user.SiteRole, SiteRoleUser)
	}

	role, ok := orgRoleOf(t, ts, userID, "acme")
	if !ok || role != OrgRoleMember {
		t.Errorf("acme role = (%q, %v), want (member, true)", role, ok)
	}

	// globex is in the mapping file, so the org exists -- but this user's groups do
	// not reach it, so they hold no membership in it.
	if _, ok := orgRoleOf(t, ts, userID, "globex"); ok {
		t.Error("the user got a membership in globex, whose groups they are not in")
	}
}

// TestReconcileIsIdempotentAndTracksTheIdP: the IdP is the source of truth for org
// and site roles, and reconciliation makes the database agree on EVERY login --
// adding what appeared and removing what did not.
func TestReconcileTracksTheIdP(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	tests := []struct {
		name         string
		groups       []string
		wantSiteRole SiteRole
		wantAcme     OrgRole // "" means no membership
		wantGlobex   OrgRole
	}{
		{
			name:         "a plain member",
			groups:       []string{"acme-devs"},
			wantSiteRole: SiteRoleUser, wantAcme: OrgRoleMember, wantGlobex: "",
		},
		{
			name:   "promoted to lead in the IdP",
			groups: []string{"acme-devs", "acme-leads"},
			// Several groups mapping to the same org yield the HIGHEST role, not
			// whichever the map happened to iterate last.
			wantSiteRole: SiteRoleUser, wantAcme: OrgRoleAdmin, wantGlobex: "",
		},
		{
			name:         "added to a second org",
			groups:       []string{"acme-owners", "globex-devs"},
			wantSiteRole: SiteRoleUser, wantAcme: OrgRoleOwner, wantGlobex: OrgRoleMember,
		},
		{
			name:         "made a site admin",
			groups:       []string{"acme-devs", "bakery-admins"},
			wantSiteRole: SiteRoleAdmin, wantAcme: OrgRoleMember, wantGlobex: "",
		},
		{
			name:   "demoted: the site-admin group is gone, and so is globex",
			groups: []string{"acme-devs"},
			// The point of the whole exercise. Removing a group in the IdP REVOKES
			// the role here, on the very next login.
			wantSiteRole: SiteRoleUser, wantAcme: OrgRoleMember, wantGlobex: "",
		},
		{
			name:         "groups Bakery does not know about are simply ignored",
			groups:       []string{"acme-devs", "sales", "everyone", "vpn-users"},
			wantSiteRole: SiteRoleUser, wantAcme: OrgRoleMember, wantGlobex: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", tt.groups...))
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			user, err := ts.store.GetUser(ctx, userID)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}

			if user.SiteRole != tt.wantSiteRole {
				t.Errorf("SiteRole = %q, want %q", user.SiteRole, tt.wantSiteRole)
			}

			for slug, want := range map[string]OrgRole{"acme": tt.wantAcme, "globex": tt.wantGlobex} {
				got, ok := orgRoleOf(t, ts, userID, slug)

				if want == "" {
					if ok {
						t.Errorf("%s role = %q, want no membership", slug, got)
					}

					continue
				}

				if !ok || got != want {
					t.Errorf("%s role = (%q, %v), want (%q, true)", slug, got, ok, want)
				}
			}
		})
	}
}

// TestReconcileFailsClosed is the one that protects real users from irreversible
// data loss.
//
// If a login arrives with no group claim, the tempting reading is "zero groups, so
// zero orgs" -- which calls ReconcileOrgMembershipsRemove with an empty keep-set,
// which deletes EVERY org membership the user holds, which CASCADES to every
// project role and every API key they hold in those orgs. Re-adding the group does
// not bring any of it back.
//
// And this is not a hypothetical input: Azure AD replaces a >200-group claim with
// a `_claim_names` overage, so "no groups" happens to real, correctly-configured
// users. The login must be REFUSED and the user's state left untouched.
func TestReconcileFailsClosed(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	// Establish a user with real state to lose.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")

	project, err := ts.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: acme, Slug: "yocto", Name: "Yocto",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := ts.store.UpsertProjectMembership(ctx, repository.UpsertProjectMembershipParams{
		UserID: userID, ID: project.ID, Role: ProjectRoleAdmin,
	}); err != nil {
		t.Fatalf("UpsertProjectMembership: %v", err)
	}

	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if _, err := ts.store.CreateAPIKey(ctx, repository.CreateAPIKeyParams{
		UserID: userID, ProjectID: project.ID, Name: "ci",
		TokenSha256: key.Hash, TokenPrefix: key.Prefix, Scope: ScopeWrite,
		ExpiresAt: nullTimestamp(),
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	tests := []struct {
		name   string
		groups []string
	}{
		{name: "no group claim at all", groups: nil},
		{name: "an empty group claim", groups: []string{}},
		{name: "groups that map to no org", groups: []string{"sales", "everyone"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", tt.groups...))
			if !errors.Is(err, ErrLoginNotAllowed) {
				t.Fatalf("Reconcile() = %v, want ErrLoginNotAllowed -- the login MUST be refused", err)
			}

			// And nothing was destroyed on the way to refusing.
			role, ok := orgRoleOf(t, ts, userID, "acme")
			if !ok || role != OrgRoleOwner {
				t.Fatalf("acme role = (%q, %v), want (owner, true): a REFUSED login deleted the "+
					"user's org membership", role, ok)
			}

			if _, err := ts.store.GetProjectMembership(ctx, repository.GetProjectMembershipParams{
				UserID: userID, ProjectID: project.ID,
			}); err != nil {
				t.Fatalf("the refused login destroyed the user's project membership: %v", err)
			}

			if _, err := ts.keys.validateKey(ctx, key.Hash); err != nil {
				t.Fatalf("the refused login revoked the user's API key: %v", err)
			}
		})
	}
}

// TestLosingAnOrgGroupRevokesEveryKeyInThatOrg proves the revocation half of the
// design.
//
// API key validation is deliberately join-free -- it must be, because it runs on
// every HEAD of a BB_NUMBER_THREADS-parallel sstate storm -- so a key cannot
// notice at validation time that its owner lost the underlying grant. The answer
// is that it never has to: dropping the org membership CASCADES
// org_memberships -> project_memberships -> api_keys. Revocation happens at
// RECONCILIATION time, by the database, in one statement.
//
// If this test ever fails, a self-contained API key grant has become unrevocable
// and the whole scheme is unsound.
func TestLosingAnOrgGroupRevokesEveryKeyInThatOrg(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	// A user in BOTH orgs, with a project and a live key in each.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners", "globex-devs"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acmeKey := seedProjectAndKey(t, ts, userID, orgIDOf(t, ts, "acme"), "yocto")
	globexKey := seedProjectAndKey(t, ts, userID, orgIDOf(t, ts, "globex"), "bazel")

	// Both keys work.
	for name, hash := range map[string][]byte{"acme": acmeKey, "globex": globexKey} {
		if _, err := ts.keys.validateKey(ctx, hash); err != nil {
			t.Fatalf("the %s key does not validate before the change: %v", name, err)
		}
	}

	// The IdP drops them from the acme groups. They keep globex.
	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "globex-devs")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// The acme key is GONE -- not "revoked at validation time by a join we
	// remembered to write", but deleted by the cascade.
	if _, err := ts.keys.validateKey(ctx, acmeKey); !errors.Is(err, ErrKeyInvalid) {
		t.Errorf("the acme key still validates after the user left the acme group: %v", err)
	}

	// And the globex key, in an org they are still in, is untouched. A cascade that
	// took everything would be just as wrong.
	if _, err := ts.keys.validateKey(ctx, globexKey); err != nil {
		t.Errorf("the globex key was revoked, but the user is still in the globex group: %v", err)
	}
}

// seedProjectAndKey gives userID a project and a live write key in org, and
// returns the key's hash.
func seedProjectAndKey(t *testing.T, ts *testService, userID, orgID pgtype.UUID, slug string) []byte {
	t.Helper()

	ctx := t.Context()

	project, err := ts.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: orgID, Slug: slug, Name: slug,
	})
	if err != nil {
		t.Fatalf("CreateProject %q: %v", slug, err)
	}

	if _, err := ts.store.UpsertProjectMembership(ctx, repository.UpsertProjectMembershipParams{
		UserID: userID, ID: project.ID, Role: ProjectRoleAdmin,
	}); err != nil {
		t.Fatalf("UpsertProjectMembership %q: %v", slug, err)
	}

	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if _, err := ts.store.CreateAPIKey(ctx, repository.CreateAPIKeyParams{
		UserID: userID, ProjectID: project.ID, Name: "ci-" + slug,
		TokenSha256: key.Hash, TokenPrefix: key.Prefix, Scope: ScopeWrite,
		ExpiresAt: nullTimestamp(),
	}); err != nil {
		t.Fatalf("CreateAPIKey %q: %v", slug, err)
	}

	return key.Hash
}

// TestEnsureOrgsCreatesTheMappedOrgs: without this, day one deadlocks. The mapping
// file names an org, no org row exists, so the first user maps to zero orgs and is
// refused -- and the only way to create an org is through an API that requires a
// logged-in site admin.
func TestEnsureOrgsCreatesTheMappedOrgs(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	if err := ts.EnsureOrgs(ctx); err != nil {
		t.Fatalf("EnsureOrgs() error = %v", err)
	}

	for _, slug := range []string{"acme", "globex"} {
		if _, err := ts.store.GetOrganizationBySlug(ctx, slug); err != nil {
			t.Errorf("organization %q was not created: %v", slug, err)
		}
	}

	// Idempotent: boot runs it every time.
	if err := ts.EnsureOrgs(ctx); err != nil {
		t.Fatalf("EnsureOrgs() is not idempotent: %v", err)
	}
}

// TestLoadPrincipalReadsRolesLive: the session carries only a user id, so a
// demotion takes effect on the user's very next request with no session
// invalidation machinery at all.
func TestLoadPrincipalReadsRolesLive(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")

	p, err := ts.loadPrincipal(ctx, userID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal() error = %v", err)
	}

	if !p.CanOwnOrg(acme) {
		t.Fatal("the owner cannot own their org")
	}

	// The IdP demotes them. No session is touched.
	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-devs")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	p, err = ts.loadPrincipal(ctx, userID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal() error = %v", err)
	}

	if p.CanOwnOrg(acme) || p.CanAdminOrg(acme) {
		t.Error("the demoted user still owns or administers the org; roles are not being read live")
	}

	if !p.CanViewOrg(acme) {
		t.Error("the demoted user lost even member access")
	}
}

// TestReconcileRefusesWithoutAGroupMap: no policy means no logins. Failing open
// here would admit everyone.
func TestReconcileRefusesWithoutAGroupMap(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, "", false)

	_, err := ts.Reconcile(t.Context(), identity("s1", "dev@acme.example", "acme-devs"))
	if !errors.Is(err, ErrLoginNotAllowed) {
		t.Fatalf("Reconcile() with no group map = %v, want ErrLoginNotAllowed", err)
	}
}

// TestUserIsKeyedOnIssuerAndSubject: two different IdPs can hand out the same
// `sub`, and an email is mutable and reassignable. Keying on email is an
// account-takeover vector.
func TestUserIsKeyedOnIssuerAndSubject(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	first, err := ts.Reconcile(ctx, identity("shared-sub", "a@acme.example", "acme-devs"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// The same subject at the SAME issuer is the same human, even after they change
	// their email at the IdP.
	again, err := ts.Reconcile(ctx, identity("shared-sub", "renamed@acme.example", "acme-devs"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if first != again {
		t.Error("the same (issuer, subject) produced two users after an email change")
	}

	user, err := ts.store.GetUser(ctx, again)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	if user.Email != "renamed@acme.example" {
		t.Errorf("Email = %q, want the updated address", user.Email)
	}

	// A different issuer with the same subject is a DIFFERENT human.
	other := identity("shared-sub", "b@globex.example", "globex-devs")
	other.Issuer = "https://other-idp.example.com"

	otherID, err := ts.Reconcile(ctx, other)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if otherID == again {
		t.Error("the same subject at a DIFFERENT issuer collapsed into one user -- that is account takeover")
	}
}

// nullTimestamp is "never expires": an absent bound.
func nullTimestamp() pgtype.Timestamptz {
	return pgtype.Timestamptz{}
}
