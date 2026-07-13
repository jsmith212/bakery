// The hybrid role model (migration 000008). See
// docs/design/specs/2026-07-12-hybrid-role-model.md.
//
// package db_test for the same reason as db_test.go: these need a real Postgres,
// and the harness that provides one imports internal/db.
//
// Everything here is asserted against the DATABASE, not against Go code, because
// the whole point of the design is that the database computes the effective role
// and enforces the invariants. No application code recomputes greatest(), so no
// application code can get it wrong -- and a test that went through a Go helper
// would be testing the helper rather than the guarantee.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// SQLSTATEs we assert on by code, not by message.
const (
	sqlstateNotNullViolation = "23502"
	sqlstateCheckViolation   = "23514"
)

// TestEffectiveOrgRoleIsGreatestOfBothSources walks every (oidc, local) pair.
//
// The enums are declared in privilege order in 000001 ('member' < 'admin' <
// 'owner'), so enum comparison IS privilege comparison and greatest() IS the
// max-wins rule. greatest() also ignores NULLs, which is exactly what "this
// source does not justify anything" has to mean. Both of those are load-bearing
// and neither is obvious, so both are pinned here.
func TestEffectiveOrgRoleIsGreatestOfBothSources(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgID := seedOrg(t, ctx, pool, "greatest-org")

	// (nil, nil) is deliberately absent: it is not a pair with a weaker answer,
	// it is a row that must not exist. TestOrgMembershipWithNoSourceIsRejected
	// covers it.
	for _, tc := range []struct {
		name  string
		oidc  string // "" == NULL
		local string // "" == NULL
		want  string
	}{
		{"oidc only, member", "member", "", "member"},
		{"oidc only, admin", "admin", "", "admin"},
		{"oidc only, owner", "owner", "", "owner"},
		{"local only, member", "", "member", "member"},
		{"local only, admin", "", "admin", "admin"},
		{"local only, owner", "", "owner", "owner"},
		{"equal, member", "member", "member", "member"},
		{"equal, admin", "admin", "admin", "admin"},
		{"equal, owner", "owner", "owner", "owner"},
		{"local wins over a weaker claim", "member", "admin", "admin"},
		{"local wins big", "member", "owner", "owner"},
		{"local wins, admin over member", "admin", "owner", "owner"},
		{"the CLAIM wins when it is stronger", "admin", "member", "admin"},
		{"the claim wins big", "owner", "member", "owner"},
		{"the claim wins, owner over admin", "owner", "admin", "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			userID := seedUser(t, ctx, pool, tc.name)
			insertMembership(t, ctx, pool, userID, orgID, tc.oidc, tc.local)

			var got string
			if err := pool.QueryRow(ctx,
				`SELECT role FROM org_memberships WHERE user_id = $1 AND org_id = $2`,
				userID, orgID,
			).Scan(&got); err != nil {
				t.Fatalf("read effective role: %v", err)
			}

			if got != tc.want {
				t.Errorf("effective role = %q, want %q (oidc=%q local=%q)",
					got, tc.want, tc.oidc, tc.local)
			}
		})
	}
}

// TestOrgMembershipRoleIsGeneratedAndRefusesADirectWrite.
//
// The effective role is the DATABASE's to compute. If application code could
// write it, application code could disagree with greatest() -- and the first
// place it would disagree is the one that matters: a local grant that outranks a
// claim. The database refuses the write outright, so that class of bug cannot be
// written.
func TestOrgMembershipRoleIsGeneratedAndRefusesADirectWrite(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgID := seedOrg(t, ctx, pool, "generated-org")
	userID := seedUser(t, ctx, pool, "generated")

	_, err := pool.Exec(ctx,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
		userID, orgID,
	)
	if err == nil {
		t.Fatal("writing org_memberships.role directly succeeded; it must be a generated column")
	}

	// 428C9: cannot insert a non-DEFAULT value into a generated column.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "428C9" {
		t.Fatalf("wrong error for a direct write to the generated column: %v", err)
	}
}

// TestOrgMembershipWithNoSourceIsRejected.
//
// A row justified by neither source is garbage, and it is not harmless garbage:
// while it exists it holds up the project-membership cascade, so it leaves alive
// exactly the API keys that leaving the org is supposed to revoke.
//
// It is refused as a NOT-NULL violation on the generated `role`, NOT as the
// named CHECK -- greatest(a, b) IS NULL iff both are NULL, so the two constraints
// are logically identical and Postgres reports NOT NULL first. The CHECK is kept
// as the declarative statement of the invariant. Asserting on its NAME here would
// be asserting on Postgres's constraint-evaluation order, which is not a contract.
func TestOrgMembershipWithNoSourceIsRejected(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgID := seedOrg(t, ctx, pool, "sourceless-org")
	userID := seedUser(t, ctx, pool, "sourceless")

	t.Run("insert with neither source", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO org_memberships (user_id, org_id, oidc_role, local_role)
			 VALUES ($1, $2, NULL, NULL)`,
			userID, orgID,
		)
		assertPgCode(t, err, sqlstateNotNullViolation, "a membership with no source")
	})

	// The reconciler's discipline, enforced structurally: when the claim goes away
	// and there is no local grant, the row must be DELETED, not blanked. An UPDATE
	// that blanks it is refused, so a reconciler that tried would fail loudly
	// rather than leave a cascade-suppressing husk behind.
	t.Run("update that removes the last source", func(t *testing.T) {
		other := seedUser(t, ctx, pool, "last-source")
		insertMembership(t, ctx, pool, other, orgID, "member", "")

		_, err := pool.Exec(ctx,
			`UPDATE org_memberships SET oidc_role = NULL, oidc_group = NULL
			  WHERE user_id = $1 AND org_id = $2`,
			other, orgID,
		)
		assertPgCode(t, err, sqlstateNotNullViolation, "nulling the only source")
	})
}

// TestOrgMembershipLocalProvenance: granted_at is present iff local_role is.
//
// Provenance is only meaningful for a local grant, and a local grant with no
// provenance is unauditable -- the site-admin and org-member screens have to be
// able to say "local: granted by X on Y", and a grant that cannot answer that is
// indistinguishable from a backdoor.
//
// granted_by is deliberately NOT in the constraint: it is ON DELETE SET NULL, so
// deleting the granting user must not retroactively invalidate every grant they
// ever made. The final subtest proves that really happens.
func TestOrgMembershipLocalProvenance(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgID := seedOrg(t, ctx, pool, "provenance-org")

	t.Run("local_role without granted_at", func(t *testing.T) {
		userID := seedUser(t, ctx, pool, "no-granted-at")

		_, err := pool.Exec(ctx,
			`INSERT INTO org_memberships (user_id, org_id, local_role, granted_at)
			 VALUES ($1, $2, 'admin', NULL)`,
			userID, orgID,
		)
		assertPgCode(t, err, sqlstateCheckViolation, "a local grant with no granted_at")
	})

	t.Run("granted_at without local_role", func(t *testing.T) {
		userID := seedUser(t, ctx, pool, "no-local-role")

		_, err := pool.Exec(ctx,
			`INSERT INTO org_memberships (user_id, org_id, oidc_role, granted_at)
			 VALUES ($1, $2, 'member', now())`,
			userID, orgID,
		)
		assertPgCode(t, err, sqlstateCheckViolation, "granted_at with no local grant")
	})

	t.Run("deleting the granting user does not invalidate the grant", func(t *testing.T) {
		granter := seedUser(t, ctx, pool, "granter")
		grantee := seedUser(t, ctx, pool, "grantee")

		if _, err := pool.Exec(ctx,
			`INSERT INTO org_memberships (user_id, org_id, local_role, granted_by, granted_at)
			 VALUES ($1, $2, 'admin', $3, now())`,
			grantee, orgID, granter,
		); err != nil {
			t.Fatalf("insert local grant: %v", err)
		}

		// If the provenance CHECK were written on granted_by rather than granted_at,
		// this DELETE would violate it via the ON DELETE SET NULL and fail.
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, granter); err != nil {
			t.Fatalf("delete the granting user: %v", err)
		}

		var role string

		var grantedBy pgtype.UUID

		if err := pool.QueryRow(ctx,
			`SELECT role, granted_by FROM org_memberships WHERE user_id = $1 AND org_id = $2`,
			grantee, orgID,
		).Scan(&role, &grantedBy); err != nil {
			t.Fatalf("the grant did not survive its granter: %v", err)
		}

		if role != "admin" {
			t.Errorf("effective role = %q after the granter was deleted, want admin", role)
		}

		if grantedBy.Valid {
			t.Errorf("granted_by = %v, want NULL after the granter was deleted", grantedBy)
		}
	})
}

// TestLocalGrantSurvivesTheOIDCGroupDisappearing is the headline of the whole
// milestone, asserted at the layer that actually guarantees it.
//
// The reconciler writes only the oidc_* half. When the claim goes away it nulls
// oidc_role and oidc_group -- and because local_role is a different column, the
// row survives, the effective role falls back to the local grant, and the user's
// project roles and API keys are NOT cascaded away. That is structural: there is
// no code path in the reconciler that could clobber local_role, because it never
// names it.
func TestLocalGrantSurvivesTheOIDCGroupDisappearing(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgID := seedOrg(t, ctx, pool, "survivor-org")
	projectID := seedProject(t, ctx, pool, orgID, "survivor-proj")
	granter := seedUser(t, ctx, pool, "survivor-granter")
	userID := seedUser(t, ctx, pool, "survivor")

	// Both sources: an LDAP group says 'member', an admin locally granted 'admin'.
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_memberships
		     (user_id, org_id, oidc_role, oidc_group, local_role, granted_by, granted_at)
		 VALUES ($1, $2, 'member', 'acme-engineering', 'admin', $3, now())`,
		userID, orgID, granter,
	); err != nil {
		t.Fatalf("insert hybrid membership: %v", err)
	}

	seedProjectMembership(t, ctx, pool, userID, orgID, projectID)
	seedKey(t, ctx, pool, userID, projectID, "ci", "write", 0x11)

	// THE RECONCILER'S MOVE: the group is gone from the claims, so the OIDC half
	// is nulled. It never names local_role.
	if _, err := pool.Exec(ctx,
		`UPDATE org_memberships SET oidc_role = NULL, oidc_group = NULL
		  WHERE user_id = $1 AND org_id = $2`,
		userID, orgID,
	); err != nil {
		t.Fatalf("reconcile the OIDC half away: %v", err)
	}

	var (
		role      string
		oidcRole  pgtype.Text
		localRole string
	)

	if err := pool.QueryRow(ctx,
		`SELECT role, oidc_role::text, local_role::text
		   FROM org_memberships WHERE user_id = $1 AND org_id = $2`,
		userID, orgID,
	).Scan(&role, &oidcRole, &localRole); err != nil {
		t.Fatalf("the membership did not survive losing its OIDC group: %v", err)
	}

	if oidcRole.Valid {
		t.Errorf("oidc_role = %q, want NULL -- the claim is gone", oidcRole.String)
	}

	if localRole != "admin" {
		t.Errorf("local_role = %q, want admin -- the reconciler must not touch it", localRole)
	}

	if role != "admin" {
		t.Errorf("effective role = %q, want admin -- it must fall back to the local grant", role)
	}

	// And the access the local grant entitles them to is all still there.
	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM project_memberships WHERE user_id = $1`, userID); n != 1 {
		t.Errorf("project memberships = %d, want 1 -- the local grant must keep them", n)
	}

	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM api_keys WHERE user_id = $1`, userID); n != 1 {
		t.Errorf("api keys = %d, want 1 -- the local grant must keep them", n)
	}
}

// TestOrgMembershipDeleteStillCascadesToProjectRolesAndKeys.
//
// THE regression test for the composite FK. 000008 drops and re-adds
// org_memberships.role, and dropping a column is exactly the operation that could
// silently take a constraint with it. If the FK
//
//	project_memberships (user_id, org_id) -> org_memberships (user_id, org_id) CASCADE
//
// is lost, nothing fails, nothing complains, and every API key a user holds in an
// org quietly outlives their removal from it -- which is the one thing the
// join-free key grant is not allowed to permit. Nothing else in the suite would
// notice, so this asserts on a REAL DELETE rather than on the catalog.
func TestOrgMembershipDeleteStillCascadesToProjectRolesAndKeys(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	assertOrgMembershipCascade(t, ctx, pool)
}

// TestHybridRolesMigrationRoundTrips: up -> down -> up.
//
// A down migration nobody runs is a down migration that does not work. This one
// is LOSSY BY NATURE -- it collapses two sources into one column, so a local
// grant that outlived its OIDC group comes back indistinguishable from a
// claim-derived one; 000002's schema cannot represent it. It is written to
// preserve the EFFECTIVE role, so nobody loses access on a rollback.
//
// What must hold after the round trip is the shape: role is generated again, and
// the cascade -- the thing a botched re-add of a dropped column would quietly
// destroy -- still fires.
func TestHybridRolesMigrationRoundTrips(t *testing.T) {
	t.Parallel()

	_, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	// A bootstrap pool: MigrateDown takes the enum types with it, so a pool that
	// insisted on registering them could not open afterwards.
	pool, err := db.NewBootstrapPool(ctx, db.Config{URL: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open bootstrap pool: %v", err)
	}

	defer pool.Close()

	if err := db.MigrateDown(pool); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate back up: %v", err)
	}

	var generated string
	if err := pool.QueryRow(ctx,
		`SELECT is_generated FROM information_schema.columns
		  WHERE table_name = 'org_memberships' AND column_name = 'role'`,
	).Scan(&generated); err != nil {
		t.Fatalf("read is_generated after the round trip: %v", err)
	}

	if generated != "ALWAYS" {
		t.Errorf("org_memberships.role is_generated = %q after down+up, want ALWAYS", generated)
	}

	// The one that matters. Re-run the whole cascade proof against the rebuilt
	// schema, on a pool that registers the enum types again.
	serving, err := db.NewPool(ctx, db.Config{URL: dsn, MaxConns: 4})
	if err != nil {
		t.Fatalf("open serving pool after the round trip: %v", err)
	}

	defer serving.Close()

	assertOrgMembershipCascade(t, ctx, serving)
}

// TestEffectiveSiteRoleCoalescesToUser.
//
// users.site_role is ASYMMETRIC with org_memberships.role, and the asymmetry is
// the subtle part of 000008. An org membership with no source is garbage and must
// not exist. A user with no site grant is an ORDINARY USER and must exist -- so
// site_role coalesces to 'user' and stays NOT NULL, preserving 000002's contract
// that it is never NULL. A straight greatest() would compute NULL for every
// ordinary user and the migration itself would fail.
func TestEffectiveSiteRoleCoalescesToUser(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		oidc  string // "" == NULL
		local string // "" == NULL
		want  string
	}{
		{"neither source: an ordinary user", "", "", "user"},
		{"claimed admin", "admin", "", "admin"},
		{"locally granted admin", "", "admin", "admin"},
		{"both", "admin", "admin", "admin"},
		// The IdP affirmatively claiming the ordinary role is not a grant of
		// anything, and must not read as one.
		{"claimed 'user' only", "user", "", "user"},
		{"claimed 'user', locally admin", "user", "admin", "admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				id    pgtype.UUID
				grant = nullTimeIf(tc.local != "")
			)

			if err := pool.QueryRow(ctx,
				`INSERT INTO users (issuer, subject, email, display_name,
				                    site_role_oidc, site_role_local, site_granted_at)
				 VALUES ('dev', $1, $1 || '@example.test', $1,
				         nullif($2, '')::site_role, nullif($3, '')::site_role, $4)
				 RETURNING id`,
				"site-"+tc.name, tc.oidc, tc.local, grant,
			).Scan(&id); err != nil {
				t.Fatalf("insert user: %v", err)
			}

			var got string
			if err := pool.QueryRow(ctx,
				`SELECT site_role FROM users WHERE id = $1`, id,
			).Scan(&got); err != nil {
				t.Fatalf("read site_role: %v", err)
			}

			if got != tc.want {
				t.Errorf("site_role = %q, want %q (oidc=%q local=%q)",
					got, tc.want, tc.oidc, tc.local)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assertOrgMembershipCascade seeds a user with an org membership, a project role
// under it and an API key under THAT, deletes the org membership, and proves all
// three are gone -- and that a bystander's key is not.
//
// The bystander is not decoration: a cascade that deleted everything would pass a
// count-based assertion just as happily as the correct one.
func assertOrgMembershipCascade(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	orgID := seedOrg(t, ctx, pool, "cascade-org")
	projectID := seedProject(t, ctx, pool, orgID, "cascade-proj")

	leaver := seedUser(t, ctx, pool, "cascade-leaver")
	bystander := seedUser(t, ctx, pool, "cascade-bystander")

	for _, u := range []pgtype.UUID{leaver, bystander} {
		insertMembership(t, ctx, pool, u, orgID, "member", "")
		seedProjectMembership(t, ctx, pool, u, orgID, projectID)
	}

	seedKey(t, ctx, pool, leaver, projectID, "leaver-key", "write", 0x21)
	seedKey(t, ctx, pool, bystander, projectID, "bystander-key", "write", 0x22)

	// ONE DELETE. Everything the leaver holds in this org must go with it.
	tag, err := pool.Exec(ctx,
		`DELETE FROM org_memberships WHERE user_id = $1 AND org_id = $2`, leaver, orgID)
	if err != nil {
		t.Fatalf("delete org membership: %v", err)
	}

	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted %d org memberships, want 1", tag.RowsAffected())
	}

	for _, tc := range []struct {
		what  string
		query string
	}{
		{"project memberships", `SELECT count(*) FROM project_memberships WHERE user_id = $1`},
		{"api keys", `SELECT count(*) FROM api_keys WHERE user_id = $1`},
	} {
		if n := countRows(t, ctx, pool, tc.query, leaver); n != 0 {
			t.Errorf("the leaver still has %d %s after their org membership was deleted.\n"+
				"The composite FK project_memberships (user_id, org_id) -> org_memberships "+
				"ON DELETE CASCADE is gone, and with it the ONLY thing that revokes a "+
				"join-free API key when a user leaves an org.", n, tc.what)
		}

		if n := countRows(t, ctx, pool, tc.query, bystander); n != 1 {
			t.Errorf("the bystander has %d %s, want 1 -- the cascade was not scoped to the leaver",
				n, tc.what)
		}
	}
}

// insertMembership inserts one org membership from its SOURCE columns. `role` is
// generated and cannot be written, which is the point.
func insertMembership(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	userID, orgID pgtype.UUID, oidc, local string,
) {
	t.Helper()

	if _, err := pool.Exec(ctx,
		`INSERT INTO org_memberships (user_id, org_id, oidc_role, local_role, granted_at)
		 VALUES ($1, $2, nullif($3, '')::org_role, nullif($4, '')::org_role, $5)`,
		userID, orgID, oidc, local, nullTimeIf(local != ""),
	); err != nil {
		t.Fatalf("insert membership (oidc=%q local=%q): %v", oidc, local, err)
	}
}

// nullTimeIf returns now() when a local grant is present and NULL otherwise --
// the org_memberships_local_provenance CHECK demands exactly that correspondence.
func nullTimeIf(present bool) pgtype.Timestamptz {
	if !present {
		return pgtype.Timestamptz{}
	}

	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

func seedOrg(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s string) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ($1, $1) RETURNING id`, s,
	).Scan(&id); err != nil {
		t.Fatalf("seed org %q: %v", s, err)
	}

	return id
}

func seedProject(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, s string,
) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id`, orgID, s,
	).Scan(&id); err != nil {
		t.Fatalf("seed project %q: %v", s, err)
	}

	return id
}

// seedUser takes a label rather than an email so a table-driven subtest can hand
// it its own name and get a distinct user for free.
func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO users (issuer, subject, email, display_name)
		 VALUES ('dev', $1, $1 || '@example.test', $1) RETURNING id`,
		label,
	).Scan(&id); err != nil {
		t.Fatalf("seed user %q: %v", label, err)
	}

	return id
}

func seedProjectMembership(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, orgID, projectID pgtype.UUID,
) {
	t.Helper()

	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (user_id, org_id, project_id, role)
		 VALUES ($1, $2, $3, 'writer')`,
		userID, orgID, projectID,
	); err != nil {
		t.Fatalf("seed project membership: %v", err)
	}
}

func seedKey(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	userID, projectID pgtype.UUID, name, scope string, seed byte,
) {
	t.Helper()

	// token_prefix is CHECKed against '^bkry_[A-Za-z0-9_-]{6,12}$' (000004), so it
	// is derived from the seed rather than the name.
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (user_id, project_id, name, token_sha256, token_prefix, scope)
		 VALUES ($1, $2, $3, $4, $5, $6::api_key_scope)`,
		userID, projectID, name, bytes32(seed), fmt.Sprintf("bkry_seed%02x", seed), scope,
	); err != nil {
		t.Fatalf("seed key %q: %v", name, err)
	}
}

func countRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any,
) int64 {
	t.Helper()

	var n int64
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

// assertPgCode asserts on the SQLSTATE, never on the message or the constraint
// name -- the message is not a contract and the constraint that fires first is
// Postgres's business, not ours.
func assertPgCode(t *testing.T, err error, code, what string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s was accepted; it must be refused with SQLSTATE %s", what, code)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: error is not a PgError: %v", what, err)
	}

	if pgErr.Code != code {
		t.Fatalf("%s: SQLSTATE = %s (%s), want %s", what, pgErr.Code, pgErr.Message, code)
	}
}
