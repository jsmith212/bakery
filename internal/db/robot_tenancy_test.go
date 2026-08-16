// The robot plane's cross-tenant gate (migration 000016).
//
// package db_test for the same reason as the rest of this suite: the property is
// the SCHEMA's, and a test that went through a Go helper would be testing the
// helper. Nothing here calls sqlc.
package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// sqlstateForeignKeyViolation is 23503, asserted by code rather than by message.
const sqlstateForeignKeyViolation = "23503"

// TestOrgTokenCannotNameARobotFromAnotherOrg is the gate on the composite
// foreign key, and the reason `robots` carries an otherwise-redundant
// UNIQUE (id, org_id).
//
// org_tokens.org_id is THE ENTIRE AUTHORIZATION DECISION for a robot: the hot
// probe returns it from the index tuple, and principal.robotGrants compares it to
// the routed org and reads nothing else -- it never looks through `robots`. It is
// denormalized deliberately, for that probe.
//
// With two INDEPENDENT foreign keys (robot_id -> robots, org_id -> organizations)
// nothing made the denormalized value agree with the robot's real org except the
// shape of one INSERT. Any future writer that supplies org_id as a parameter -- a
// bulk import, a robot-move feature, a repair script, a second CreateOrgToken
// variant -- would mint a LIVE token granting org-wide write on org B, backed by
// a robot in org A. Not dangling (deleting the robot still cascades), simply
// cross-tenant, and no constraint and no test would notice.
//
// The composite FK makes the divergent row unrepresentable, which is the standard
// api_keys' composite FK onto project_memberships already sets.
func TestOrgTokenCannotNameARobotFromAnotherOrg(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	orgA := seedOrg(t, ctx, pool, "robot-tenancy-a")
	orgB := seedOrg(t, ctx, pool, "robot-tenancy-b")

	var robotID string

	if err := pool.QueryRow(ctx,
		`INSERT INTO robots (org_id, name, created_by_email)
		 VALUES ($1, 'ci', 'admin@example.com') RETURNING id`, orgA,
	).Scan(&robotID); err != nil {
		t.Fatalf("seed the robot in org A: %v", err)
	}

	// The name and the hash vary per call: org_tokens carries a UNIQUE on
	// token_sha256 (and one on (robot_id, name)), and a 23505 from either would
	// mask the 23503 this test is looking for.
	insert := func(label string, orgID any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO org_tokens
			     (robot_id, org_id, name, token_sha256, token_prefix, scope,
			      expires_at, created_by_email)
			 VALUES ($1, $2, $3, sha256($3::text::bytea), 'bkro_abcdef', 'write',
			         now() + interval '30 days', 'admin@example.com')`,
			robotID, orgID, label)

		return err
	}

	// The honest row: the token's org IS its robot's org.
	if err := insert("honest", orgA); err != nil {
		t.Fatalf("a token whose org matches its robot's must be insertable: %v", err)
	}

	// The cross-tenant row: a live write-scoped credential for org B, backed by a
	// robot that belongs to org A.
	err := insert("cross-tenant", orgB)
	if err == nil {
		t.Fatal("inserted an org_token whose org_id disagrees with its robot's org.\n" +
			"org_tokens.org_id IS the authorization decision -- the probe reads it alone -- " +
			"so this row is a cross-tenant grant. The composite FK " +
			"(robot_id, org_id) REFERENCES robots (id, org_id) is what must make it " +
			"unrepresentable; a plain robot_id FK does not.")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateForeignKeyViolation {
		t.Fatalf("insert error = %v, want a 23503 foreign-key violation from the composite FK", err)
	}

	// And the cascade the composite FK now carries is still the one that matters:
	// deleting the robot revokes every token it holds, with no application code in
	// the loop.
	if _, err := pool.Exec(ctx, `DELETE FROM robots WHERE id = $1`, robotID); err != nil {
		t.Fatalf("delete the robot: %v", err)
	}

	var left int

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM org_tokens WHERE robot_id = $1`, robotID).Scan(&left); err != nil {
		t.Fatalf("count the surviving tokens: %v", err)
	}

	if left != 0 {
		t.Errorf("%d org_tokens survived their robot's deletion; the composite FK must keep "+
			"ON DELETE CASCADE", left)
	}
}
