package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// TestAnUnreadableGroupsClaimRefusesTheLoginAndMutatesNOTHING is the test this
// whole milestone turns on.
//
// "The IdP says this user is in zero groups" and "we could not read this user's
// groups" are CATEGORICALLY DIFFERENT, and only the first is safe to act on. Azure
// AD replaces a >200-group claim with a `_claim_names` overage pointing at Graph,
// so the `groups` claim is simply ABSENT for real, correctly-configured users.
// Reconciling that as "zero groups" would NULL every oidc_role, delete every row
// with no local grant, and cascade away the user's project roles and API keys. Re-
// adding the group does not bring any of it back.
//
// So the refusal must come BEFORE THE FIRST WRITE -- not be rolled back after one.
// This test therefore asserts on the DATABASE, not on the error: row counts, and
// the actual role values, before and after. A test that only checked
// errors.Is(err, ErrLoginNotAllowed) would pass against a reconciler that wiped the
// table and then returned an error, which is precisely the bug it is here to catch.
func TestAnUnreadableGroupsClaimRefusesTheLoginAndMutatesNothing(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	// A user with real state to lose: an OIDC membership, a LOCAL grant in a second
	// org, a project role, and a live key.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")
	globex := orgIDOf(t, ts, "globex")

	grantLocalRole(t, ts, userID, globex, OrgRoleAdmin)

	keyHash := seedProjectAndKey(t, ts, userID, acme, "yocto")

	before := snapshot(t, ts, userID, acme, globex)

	unreadable := []struct {
		name string
		id   Identity
	}{
		{
			// The Azure AD overage: the token carried no readable `groups` claim at all.
			name: "the claim is absent",
			id:   unreadableIdentity("s1", "dev@acme.example"),
		},
		{
			// Defence in depth. GroupsPresent is the ONLY thing that carries the
			// distinction, so a stale slice beside a false Present must still refuse:
			// the slice is not evidence, the bit is.
			name: "a stale slice beside an unset Present bit",
			id: Identity{
				Issuer: "https://idp.example.com", Subject: "s1", Email: "dev@acme.example",
				DisplayName: "dev@acme.example", Groups: []string{"acme-devs"}, GroupsPresent: false,
				IssuedAt: time.Now(), RefreshToken: "",
			},
		},
	}

	for _, tt := range unreadable {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ts.Reconcile(ctx, tt.id)
			if !errors.Is(err, ErrLoginNotAllowed) {
				t.Fatalf("Reconcile() = %v, want ErrLoginNotAllowed -- the login MUST be refused", err)
			}

			// The error is necessary and nowhere near sufficient. THIS is the assertion.
			after := snapshot(t, ts, userID, acme, globex)
			if after != before {
				t.Fatalf("a REFUSED login mutated the database.\n before = %+v\n after  = %+v", before, after)
			}

			// The counts are the structural half; this is the one the user feels.
			if _, err := ts.keys.validateKey(ctx, keyHash); err != nil {
				t.Fatalf("the refused login revoked the user's API key: %v", err)
			}
		})
	}

	// And it must not JIT-provision a user it is about to refuse either: a login we
	// cannot authorize creates nothing at all.
	usersBefore := countRows(t, ts, "users")

	if _, err := ts.Reconcile(ctx, unreadableIdentity("never-seen", "ghost@acme.example")); !errors.Is(err, ErrLoginNotAllowed) {
		t.Fatalf("Reconcile() = %v, want ErrLoginNotAllowed", err)
	}

	if got := countRows(t, ts, "users"); got != usersBefore {
		t.Errorf("users = %d, want %d: a refused login provisioned a user row", got, usersBefore)
	}
}

// TestALocalGrantSurvivesTheLDAPGroupGoingAway is the other one that matters.
//
// The reconciler owns exactly ONE of the two sources. When the claims stop
// justifying a membership it must clear the OIDC half and then delete the row IFF
// `local_role IS NULL` -- never blindly. A blind DELETE of "every org the claims no
// longer justify" destroys a deliberate local grant and cascades away the user's
// project roles and API keys in that org, which is exactly the state the hybrid
// model exists to make possible.
func TestALocalGrantSurvivesTheLDAPGroupGoingAway(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	// They are in acme by CLAIM (owner) and by LOCAL GRANT (member) at once.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")

	grantLocalRole(t, ts, userID, acme, OrgRoleMember)

	keyHash := seedProjectAndKey(t, ts, userID, acme, "yocto")

	// The effective role is greatest(owner, member) -- the database's arithmetic, not ours.
	if m := membership(t, ts, userID, acme); m.Role != OrgRoleOwner {
		t.Fatalf("effective role = %q, want owner (greatest of the claim and the grant)", m.Role)
	}

	// The IdP drops them from every acme group.
	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "globex-devs")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	m := membership(t, ts, userID, acme)

	// The row SURVIVES. The local grant is what justifies it now.
	if m.OidcRole.Valid {
		t.Errorf("oidc_role = %q, want NULL: the claims no longer justify this membership", m.OidcRole.OrgRole)
	}

	if !m.LocalRole.Valid || m.LocalRole.OrgRole != OrgRoleMember {
		t.Fatalf("local_role = %+v, want member: the reconciler destroyed a LOCAL grant", m.LocalRole)
	}

	if m.Role != OrgRoleMember {
		t.Errorf("effective role = %q, want member: greatest(NULL, member)", m.Role)
	}

	// And therefore the cascade did NOT fire: the project role and the key it backs
	// are still alive, because the user is still legitimately in the org.
	if _, err := ts.store.GetProjectMembership(ctx, repository.GetProjectMembershipParams{
		UserID: userID, ProjectID: projectIDOf(t, ts, acme, "yocto"),
	}); err != nil {
		t.Errorf("the locally-granted user lost their project membership: %v", err)
	}

	if _, err := ts.keys.validateKey(ctx, keyHash); err != nil {
		t.Errorf("the locally-granted user lost their API key: %v", err)
	}
}

// TestTheRowIsDeletedOnlyWhenBothSourcesAreGone is the same coin's other face. A
// row nothing justifies MUST go -- while it exists it suppresses the
// project-membership cascade and leaves alive exactly the keys that leaving the org
// is supposed to revoke.
func TestTheRowIsDeletedOnlyWhenBothSourcesAreGone(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")
	keyHash := seedProjectAndKey(t, ts, userID, acme, "yocto")
	projectID := projectIDOf(t, ts, acme, "yocto")

	// Step one: a local grant is added, then the LDAP group goes away. ONE source is
	// gone. The row stays, and so does everything hanging off it.
	grantLocalRole(t, ts, userID, acme, OrgRoleMember)

	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, ok := orgRoleOf(t, ts, userID, "acme"); !ok {
		t.Fatal("the row was deleted while a local grant still justified it")
	}

	// Step two: the LDAP group comes back and the LOCAL grant is the one withdrawn.
	// Again only one source is gone, from the other side. The row still stays.
	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	revokeLocalRole(t, ts, userID, acme)

	if role, ok := orgRoleOf(t, ts, userID, "acme"); !ok || role != OrgRoleOwner {
		t.Fatalf("acme role = (%q, %v), want (owner, true): the claim still justifies the row", role, ok)
	}

	// Step three: the claim goes too. NOW both sources are gone, and only now may the
	// row -- and the cascade behind it -- fire.
	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if role, ok := orgRoleOf(t, ts, userID, "acme"); ok {
		t.Fatalf("acme role = %q: the row survived with NEITHER source justifying it, which "+
			"suppresses the cascade and leaves the user's API keys alive", role)
	}

	if _, err := ts.store.GetProjectMembership(ctx, repository.GetProjectMembershipParams{
		UserID: userID, ProjectID: projectID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetProjectMembership after the last grant went = %v, want no rows: the cascade did not fire", err)
	}

	if _, err := ts.keys.validateKey(ctx, keyHash); !errors.Is(err, ErrKeyInvalid) {
		t.Errorf("the key still validates after the user's last grant on the org went away: %v", err)
	}
}

// TestAnEmptyGroupsClaimIsAdmitted: `groups: []` is the IdP's ANSWER, and it is an
// ordinary one. Under the hybrid model it means "this user has only local
// memberships" -- so the login is ADMITTED and the OIDC half is reconciled away.
//
// M1 refused this login, and that refusal is now the bug: it locks out every
// local-only user. What replaced it is the unreadable-claim refusal above, which
// fires on "we do not know your groups" rather than on "you have none".
func TestAnEmptyGroupsClaimIsAdmittedAndReconcilesTheOIDCHalfAway(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	// A brand-new user whose only groups are ones Bakery does not map: zero orgs, and
	// a perfectly good login.
	local, err := ts.Reconcile(ctx, identity("local-only", "local@acme.example"))
	if err != nil {
		t.Fatalf("Reconcile() with an empty groups claim = %v, want admission: a local-only user "+
			"legitimately resolves to zero claim-derived orgs", err)
	}

	if _, err := ts.store.GetUser(ctx, local); err != nil {
		t.Fatalf("the admitted user was not provisioned: %v", err)
	}

	if _, ok := orgRoleOf(t, ts, local, "acme"); ok {
		t.Error("a user with no groups got an org membership")
	}

	// Same story for groups that map to nothing: users are routinely in dozens of
	// unrelated IdP groups, and being in only those is not an error either.
	if _, err := ts.Reconcile(ctx, identity("unmapped", "sales@acme.example", "sales", "everyone")); err != nil {
		t.Fatalf("Reconcile() with only unmapped groups = %v, want admission", err)
	}

	// And the OIDC half really is reconciled AWAY, not merely left alone: an existing
	// claim-derived membership is removed when the claim empties out.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, ok := orgRoleOf(t, ts, userID, "acme"); !ok {
		t.Fatal("the claim-derived membership was not created")
	}

	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if role, ok := orgRoleOf(t, ts, userID, "acme"); ok {
		t.Errorf("acme role = %q, want no membership: `groups: []` must reconcile the OIDC half away", role)
	}
}

// TestReconcileNeverWritesTheLocalHalf pins the structural claim the spec makes:
// the reconciler names oidc_role and oidc_group and NOTHING else. Not local_role,
// not granted_by, not granted_at, and never project_memberships.
func TestReconcileNeverWritesTheLocalHalf(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-leads"))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acme := orgIDOf(t, ts, "acme")

	m := membership(t, ts, userID, acme)

	if !m.OidcRole.Valid || m.OidcRole.OrgRole != OrgRoleAdmin {
		t.Errorf("oidc_role = %+v, want admin", m.OidcRole)
	}

	// The audit trail: WHICH group justified the membership.
	if m.OidcGroup.String != "acme-leads" {
		t.Errorf("oidc_group = %q, want acme-leads", m.OidcGroup.String)
	}

	if m.LocalRole.Valid || m.GrantedBy.Valid || m.GrantedAt.Valid {
		t.Errorf("the reconciler wrote the LOCAL half: local_role=%+v granted_by=%+v granted_at=%+v",
			m.LocalRole, m.GrantedBy, m.GrantedAt)
	}

	// Project roles are in-app and single-source. A login must not create, change or
	// remove one -- only the org-membership cascade may ever touch them.
	before := countRows(t, ts, "project_memberships")

	if _, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got := countRows(t, ts, "project_memberships"); got != before {
		t.Errorf("project_memberships = %d, want %d: the reconciler wrote project roles", got, before)
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

// TestNoGroupMapAdmitsAnySuccessfulOIDCAuth pins the deployment the hybrid model
// exists to enable: an IdP that answers "who are you", and Bakery answering "what
// may you do" entirely from in-app grants. No mapping file at all.
//
// This is the shape README, config.go, boot.go and groupmap.go all promise, and it
// is unrecoverable when it is broken: with dev login off, a refused login means no
// user row, and the CLI break-glass (`bakery user site-admin <email>`) can only
// promote a user who has already logged in once. Every login refused, forever, with
// no way out -- so the assertion is that a login SUCCEEDS.
//
// The absent map is NOT the fail-closed guard, and the subtests below prove the
// real one is untouched: an unreadable claim is still refused, and it is refused
// exactly as hard with no map as with one.
func TestNoGroupMapAdmitsAnySuccessfulOIDCAuth(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, "", false) // GROUP_MAP_FILE unset: Deps.Groups is nil.
	ctx := t.Context()

	// The empty map is an empty LOGIN GATE, so a readable claim -- any readable
	// claim -- is admitted.
	userID, err := ts.Reconcile(ctx, identity("s1", "boss@acme.example", "engineering"))
	if err != nil {
		t.Fatalf("Reconcile() with no group map = %v, want admission: an absent mapping file "+
			"means every role is an in-app grant, not that nobody may log in", err)
	}

	user, err := ts.store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("the admitted user was not provisioned: %v", err)
	}

	if user.Email != "boss@acme.example" {
		t.Errorf("Email = %q, want boss@acme.example", user.Email)
	}

	// An empty map grants nothing, which is the other half of the promise: no
	// site_admin_groups means no claim-derived site admin, and no orgs means no
	// claim-derived membership. The first login is an ordinary user.
	p, err := ts.loadPrincipal(ctx, userID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal() error = %v", err)
	}

	if p.IsSiteAdmin() {
		t.Error("an empty group map minted a site admin; it must grant nothing")
	}

	if got := countRows(t, ts, "org_memberships"); got != 0 {
		t.Errorf("org_memberships = %d, want 0: an empty group map grants no org membership", got)
	}

	// EnsureOrgs over no map ensures nothing, rather than exploding at boot.
	if err := ts.EnsureOrgs(ctx); err != nil {
		t.Fatalf("EnsureOrgs() with no group map = %v, want nil", err)
	}

	if got := countRows(t, ts, "organizations"); got != 0 {
		t.Errorf("organizations = %d, want 0: an absent mapping file names no orgs", got)
	}

	// And this is the point of the whole deployment: a site admin grants the org
	// membership locally, and it SURVIVES every subsequent login -- the reconciler
	// resolves zero claim-derived orgs and must not read that as "drop everything".
	org, err := ts.store.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: "acme", Name: "acme",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	grantLocalRole(t, ts, userID, org.ID, OrgRoleAdmin)

	if _, err := ts.Reconcile(ctx, identity("s1", "boss@acme.example", "engineering")); err != nil {
		t.Fatalf("Reconcile() on a second login = %v", err)
	}

	role, ok := orgRoleOf(t, ts, userID, "acme")
	if !ok {
		t.Fatal("the local grant was deleted by a login against no group map: " +
			"zero CLAIM-derived orgs is not zero orgs")
	}

	if role != OrgRoleAdmin {
		t.Errorf("acme role = %q, want %q: the local half must survive the reconcile", role, OrgRoleAdmin)
	}

	// The fail-closed rule is about the CLAIM, never about the file. An unreadable
	// groups claim is still refused, and still writes nothing -- the local grant, and
	// the user's whole authorization, must be exactly where they were.
	before := countRows(t, ts, "org_memberships")

	if _, err := ts.Reconcile(ctx, unreadableIdentity("s1", "boss@acme.example")); !errors.Is(err, ErrLoginNotAllowed) {
		t.Fatalf("Reconcile() with an unreadable claim and no group map = %v, want ErrLoginNotAllowed: "+
			"the trap detection does not depend on a mapping file existing", err)
	}

	if got := countRows(t, ts, "org_memberships"); got != before {
		t.Errorf("org_memberships = %d, want %d: a REFUSED login mutated the database", got, before)
	}

	if role, ok := orgRoleOf(t, ts, userID, "acme"); !ok || role != OrgRoleAdmin {
		t.Errorf("acme role = (%q, %v), want (admin, true): a refused login destroyed a local grant", role, ok)
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

// unreadableIdentity is the identity the OIDC provider builds when it could not
// read the groups claim AT ALL: absent, or replaced by an Azure AD `_claim_names`
// overage pointing at Graph. It is NOT "in zero groups", and the reconciler must
// refuse it rather than reconcile against it.
func unreadableIdentity(subject, email string) Identity {
	return Identity{
		Issuer: "https://idp.example.com", Subject: subject, Email: email,
		DisplayName: email, Groups: nil, GroupsPresent: false,
		IssuedAt: time.Now(), RefreshToken: "",
	}
}

// grantLocalRole writes the LOCAL half of a membership directly, because the API
// that will write it is the NEXT task. The reconciler must already be safe against
// it -- the whole point is that it cannot see this column.
func grantLocalRole(t *testing.T, ts *testService, userID, orgID pgtype.UUID, role OrgRole) {
	t.Helper()

	_, err := ts.pool.Exec(t.Context(), `
		INSERT INTO org_memberships (user_id, org_id, local_role, granted_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, org_id)
		DO UPDATE SET local_role = EXCLUDED.local_role, granted_at = now()`,
		userID, orgID, role)
	if err != nil {
		t.Fatalf("grant a local role: %v", err)
	}
}

// revokeLocalRole withdraws the local grant, leaving whatever the claims justify.
//
// granted_at must be cleared with it: org_memberships_local_provenance is
// `(local_role IS NULL) = (granted_at IS NULL)`. And this is only ever called while
// oidc_role is still set, because `role` is GENERATED NOT NULL -- clearing the last
// remaining source is a 23502, not an orphan row. That is the schema refusing to
// represent the garbage state, and it is why the reconciler deletes rather than
// nulls.
func revokeLocalRole(t *testing.T, ts *testService, userID, orgID pgtype.UUID) {
	t.Helper()

	_, err := ts.pool.Exec(t.Context(), `
		UPDATE org_memberships
		   SET local_role = NULL, granted_at = NULL, granted_by = NULL
		 WHERE user_id = $1 AND org_id = $2`,
		userID, orgID)
	if err != nil {
		t.Fatalf("revoke a local role: %v", err)
	}
}

// membership reads the WHOLE row -- both sources and the provenance -- not just the
// effective role.
func membership(t *testing.T, ts *testService, userID, orgID pgtype.UUID) repository.OrgMembership {
	t.Helper()

	m, err := ts.store.GetOrgMembership(t.Context(), repository.GetOrgMembershipParams{
		UserID: userID, OrgID: orgID,
	})
	if err != nil {
		t.Fatalf("get org membership: %v", err)
	}

	return m
}

func projectIDOf(t *testing.T, ts *testService, orgID pgtype.UUID, slug string) pgtype.UUID {
	t.Helper()

	projects, err := ts.store.ListProjectsForOrg(t.Context(), orgID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}

	for _, p := range projects {
		if p.Slug == slug {
			return p.ID
		}
	}

	t.Fatalf("project %q does not exist", slug)

	return pgtype.UUID{}
}

func countRows(t *testing.T, ts *testService, table string) int {
	t.Helper()

	var n int

	// table is a compile-time literal in every caller; there is no user input here.
	if err := ts.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}

	return n
}

// dbState is a COMPARABLE snapshot of everything a bad reconciliation would
// destroy. Comparable on purpose: the assertion is `after != before`, so a column
// added here is a column the refusal test starts protecting for free.
type dbState struct {
	users, orgMemberships, projectMemberships, apiKeys int

	siteRole SiteRole

	acmeOIDC, acmeLocal, acmeRole       string
	globexOIDC, globexLocal, globexRole string
}

func snapshot(t *testing.T, ts *testService, userID, acme, globex pgtype.UUID) dbState {
	t.Helper()

	user, err := ts.store.GetUser(t.Context(), userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	aOIDC, aLocal, aRole := membershipState(t, ts, userID, acme)
	gOIDC, gLocal, gRole := membershipState(t, ts, userID, globex)

	return dbState{
		users:              countRows(t, ts, "users"),
		orgMemberships:     countRows(t, ts, "org_memberships"),
		projectMemberships: countRows(t, ts, "project_memberships"),
		apiKeys:            countRows(t, ts, "api_keys"),
		siteRole:           user.SiteRole,
		acmeOIDC:           aOIDC, acmeLocal: aLocal, acmeRole: aRole,
		globexOIDC: gOIDC, globexLocal: gLocal, globexRole: gRole,
	}
}

// membershipState reports a membership's two sources and its effective role.
//
// A MISSING ROW IS A VALUE, not a fatal: "the refused login cascade-deleted this
// membership" is precisely the outcome under test, and a helper that called
// t.Fatalf on it would report the symptom (`no rows`) instead of the finding.
func membershipState(t *testing.T, ts *testService, userID, orgID pgtype.UUID) (string, string, string) {
	t.Helper()

	m, err := ts.store.GetOrgMembership(t.Context(), repository.GetOrgMembershipParams{
		UserID: userID, OrgID: orgID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "<gone>", "<gone>", "<gone>"
	}

	if err != nil {
		t.Fatalf("get org membership: %v", err)
	}

	return nullRole(m.OidcRole), nullRole(m.LocalRole), string(m.Role)
}

func nullRole(r repository.NullOrgRole) string {
	if !r.Valid {
		return "<null>"
	}

	return string(r.OrgRole)
}
