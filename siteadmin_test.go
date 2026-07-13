package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestMain gives the break-glass tests a real, ephemeral Postgres. There is no way
// to test this command without one: the whole point of it is that it writes to the
// database and speaks no HTTP at all.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// siteRoleRow is the whole hybrid site role of one user: both sources, the effective
// role the database computes from them, and the provenance of the local half.
type siteRoleRow struct {
	effective repository.SiteRole
	oidc      repository.NullSiteRole
	oidcGroup pgtype.Text
	local     repository.NullSiteRole
	grantedBy pgtype.UUID
	grantedAt pgtype.Timestamptz
}

func readSiteRole(t *testing.T, pool *pgxpool.Pool, email string) siteRoleRow {
	t.Helper()

	var row siteRoleRow

	err := pool.QueryRow(t.Context(),
		`SELECT site_role, site_role_oidc, site_oidc_group, site_role_local,
		        site_granted_by, site_granted_at
		   FROM users WHERE email = $1`, email,
	).Scan(&row.effective, &row.oidc, &row.oidcGroup, &row.local, &row.grantedBy, &row.grantedAt)
	if err != nil {
		t.Fatalf("read the site role of %q: %v", email, err)
	}

	return row
}

// seedUser provisions a user exactly as a first login would -- with NO site role.
// That is the state a fresh deployment is in, and it is the deadlock the break-glass
// exists to break: nobody is a site admin, and every path to becoming one requires
// already being one.
func seedUser(t *testing.T, pool *pgxpool.Pool, email string, siteAdminGroup string) {
	t.Helper()

	oidc := pgtype.Text{String: "admin", Valid: siteAdminGroup != ""}
	group := pgtype.Text{String: siteAdminGroup, Valid: siteAdminGroup != ""}

	_, err := pool.Exec(t.Context(),
		`INSERT INTO users (issuer, subject, email, display_name, site_role_oidc, site_oidc_group)
		 VALUES ('https://idp.test', $1, $1, 'Test User', $2::text::site_role, $3)`,
		email, oidc, group)
	if err != nil {
		t.Fatalf("seed user %q: %v", email, err)
	}
}

// ---------------------------------------------------------------------------

// TestBreakGlassAppointsTheFirstSiteAdmin.
//
// THE BOOTSTRAP. With login_groups empty and no site_admin_groups, a fresh deployment
// has no site admin -- and no API path can make one, because every such path requires
// already being one. This command is the way out, and it is deliberately NOT on the
// network: it takes DB_URL, so reaching it means having infrastructure access rather
// than a session. That is the same shape as DEV_LOGIN_ENABLED, which no request can
// turn on.
func TestBreakGlassAppointsTheFirstSiteAdmin(t *testing.T) {
	pool, dsn := dbtest.NewWithDSN(t)

	seedUser(t, pool, "jsmith@acme.dev", "")

	// The precondition: NOBODY is a site admin. This is the deadlock.
	var admins int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE site_role = 'admin'`).Scan(&admins); err != nil {
		t.Fatalf("count site admins: %v", err)
	}

	if admins != 0 {
		t.Fatalf("the fixture already has %d site admins; the bootstrap case is not being tested", admins)
	}

	var out bytes.Buffer

	err := userSiteAdmin(t.Context(), &out, config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "jsmith@acme.dev",
		Revoke:  false,
	})
	if err != nil {
		t.Fatalf("bakery user site-admin: %v", err)
	}

	got := readSiteRole(t, pool, "jsmith@acme.dev")

	if got.effective != repository.SiteRoleAdmin {
		t.Errorf("effective site_role = %q, want admin", got.effective)
	}

	if !got.local.Valid || got.local.SiteRole != repository.SiteRoleAdmin {
		t.Errorf("site_role_local = %+v, want admin", got.local)
	}

	// It wrote the LOCAL half, never the claim half. Forging site_role_oidc would make
	// an admin who stops being one the moment they next sign in -- the reconciler would
	// see no group backing it and NULL the column.
	if got.oidc.Valid {
		t.Errorf("site_role_oidc = %+v: the break-glass forged an OIDC CLAIM. No group says "+
			"this, and the user's next login would reconcile it away.", got.oidc)
	}

	if !got.grantedAt.Valid {
		t.Error("site_granted_at is NULL: the grant has no provenance at all")
	}

	// granted_by is NULL, and that is deliberate and informative: there is no session
	// here, so there is nobody to name. A site admin with a local grant and no granter
	// is one that was made with database access -- and the site-admin listing shows
	// exactly that, which is what keeps this from being an invisible backdoor.
	if got.grantedBy.Valid {
		t.Errorf("site_granted_by = %v, want NULL: there is no session, so there is nobody "+
			"to name, and inventing one would put a lie in the audit trail", got.grantedBy)
	}

	if !strings.Contains(out.String(), "site administrator") {
		t.Errorf("the command said nothing useful: %q", out.String())
	}
}

// TestBreakGlassGrantSurvivesTheNextLogin is the reason it writes site_role_LOCAL.
//
// The reconciler runs on every login and owns the OIDC half. It NULLs site_role_oidc
// for a user no group makes an admin. If the break-glass had written that column, the
// first site admin would have been demoted by their own first sign-in -- which is a
// bootstrap that does not bootstrap.
func TestBreakGlassGrantSurvivesTheNextLogin(t *testing.T) {
	pool, dsn := dbtest.NewWithDSN(t)

	seedUser(t, pool, "jsmith@acme.dev", "")

	if err := userSiteAdmin(t.Context(), &bytes.Buffer{}, config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "jsmith@acme.dev",
		Revoke:  false,
	}); err != nil {
		t.Fatalf("bakery user site-admin: %v", err)
	}

	// THE NEXT LOGIN, as the reconciler performs it: UpsertUser with the claim half
	// resolved to nothing (no group makes them a site admin), and NOTHING ELSE.
	_, err := repository.New(pool).UpsertUser(t.Context(), repository.UpsertUserParams{
		Issuer: "https://idp.test", Subject: "jsmith@acme.dev",
		Email: "jsmith@acme.dev", DisplayName: "Test User",
		SiteRole:  repository.SiteRoleUser,
		SiteGroup: pgtype.Text{String: "", Valid: false},
	})
	if err != nil {
		t.Fatalf("reconcile a login: %v", err)
	}

	got := readSiteRole(t, pool, "jsmith@acme.dev")

	if got.effective != repository.SiteRoleAdmin {
		t.Fatalf("effective site_role = %q after a login, want admin. The break-glass grant "+
			"did not survive the user's first sign-in, so the bootstrap it exists for does "+
			"not work.", got.effective)
	}

	if !got.local.Valid {
		t.Error("site_role_local was cleared by a login: the reconciler must not name that column")
	}
}

// TestBreakGlassRevokeLeavesAClaimDerivedAdminStanding: --revoke clears only the
// LOCAL half. A site admin held up by an OIDC group claim is LDAP's to remove, and
// telling an operator otherwise -- reporting a success that demoted nobody -- is how
// someone ends up believing they removed a platform admin who still administers the
// platform.
func TestBreakGlassRevokeLeavesAClaimDerivedAdminStanding(t *testing.T) {
	pool, dsn := dbtest.NewWithDSN(t)

	seedUser(t, pool, "jsmith@acme.dev", "platform-admins")

	cmd := config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "jsmith@acme.dev",
		Revoke:  false,
	}

	// A local grant ON TOP of the claim: both halves now justify the role.
	if err := userSiteAdmin(t.Context(), &bytes.Buffer{}, cmd); err != nil {
		t.Fatalf("grant: %v", err)
	}

	var out bytes.Buffer

	cmd.Revoke = true
	if err := userSiteAdmin(t.Context(), &out, cmd); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got := readSiteRole(t, pool, "jsmith@acme.dev")

	if got.local.Valid {
		t.Errorf("site_role_local = %+v, want NULL after --revoke", got.local)
	}

	if !got.oidc.Valid || got.effective != repository.SiteRoleAdmin {
		t.Errorf("the revoke clobbered the CLAIM half: %+v", got)
	}

	// And the operator was TOLD they are still an admin, and by which group.
	if !strings.Contains(out.String(), "STILL A SITE ADMINISTRATOR") {
		t.Errorf("the revoke reported a success that demoted nobody, without saying so:\n%s",
			out.String())
	}

	if !strings.Contains(out.String(), "platform-admins") {
		t.Errorf("the output does not name the group still holding the role up:\n%s", out.String())
	}
}

// TestBreakGlassRevokeOfAPurelyClaimDerivedAdminIsRefused: there is nothing local to
// clear, and pretending there was would be the same lie in a louder register.
func TestBreakGlassRevokeOfAPurelyClaimDerivedAdminIsRefused(t *testing.T) {
	pool, dsn := dbtest.NewWithDSN(t)

	seedUser(t, pool, "jsmith@acme.dev", "platform-admins")

	err := userSiteAdmin(t.Context(), &bytes.Buffer{}, config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "jsmith@acme.dev",
		Revoke:  true,
	})
	if err == nil {
		t.Fatal("revoking a claim-derived site admin succeeded; it must refuse and say why")
	}

	if !strings.Contains(err.Error(), "identity provider") {
		t.Errorf("the error does not tell the operator where to actually remove them: %v", err)
	}

	if got := readSiteRole(t, pool, "jsmith@acme.dev"); got.effective != repository.SiteRoleAdmin {
		t.Errorf("the refused revoke demoted the user anyway: %+v", got)
	}
}

// TestBreakGlassRevokeDemotesALocalOnlyAdmin.
func TestBreakGlassRevokeDemotesALocalOnlyAdmin(t *testing.T) {
	pool, dsn := dbtest.NewWithDSN(t)

	seedUser(t, pool, "jsmith@acme.dev", "")

	cmd := config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "jsmith@acme.dev",
		Revoke:  false,
	}

	if err := userSiteAdmin(t.Context(), &bytes.Buffer{}, cmd); err != nil {
		t.Fatalf("grant: %v", err)
	}

	var out bytes.Buffer

	cmd.Revoke = true
	if err := userSiteAdmin(t.Context(), &out, cmd); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got := readSiteRole(t, pool, "jsmith@acme.dev")

	if got.effective != repository.SiteRoleUser {
		t.Errorf("effective site_role = %q, want user", got.effective)
	}

	if got.grantedAt.Valid {
		t.Error("the grant provenance survived the revocation")
	}

	if !strings.Contains(out.String(), "no longer a site administrator") {
		t.Errorf("the command did not confirm the demotion: %q", out.String())
	}
}

// TestBreakGlassRefusesAnUnknownEmail. Users are JIT-provisioned at their first
// login, so there is genuinely nothing to grant -- and nobody guesses that from a
// bare "not found".
func TestBreakGlassRefusesAnUnknownEmail(t *testing.T) {
	_, dsn := dbtest.NewWithDSN(t)

	err := userSiteAdmin(t.Context(), &bytes.Buffer{}, config.UserSiteAdminCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
		Email:   "nobody@acme.dev",
		Revoke:  false,
	})
	if err == nil {
		t.Fatal("granting a site role to a nonexistent user succeeded")
	}

	if !strings.Contains(err.Error(), "sign in once") {
		t.Errorf("the error does not tell the operator what to do about it: %v", err)
	}
}
