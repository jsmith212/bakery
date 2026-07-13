// Enum-type registration. See internal/db/enums.go.
//
// package db_test for the same reason as db_test.go: these need a real Postgres,
// and the harness that provides one imports internal/db.
package db_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestEnumArrayParameterEncodes is THE test that would have caught the bug.
//
// RevokeAPIKeysForMembership binds `scope = ANY($3::api_key_scope[])`, so its
// Scopes parameter is a SLICE of a custom enum. pgx cannot build an encode plan
// for that unless the enum's ARRAY type (_api_key_scope -- a distinct OID from
// the scalar) is registered on the connection. It never was, the query had no
// caller, and so nothing proved it: it failed 100% of the time, silently, from
// the day it was written.
//
// Every other enum query in the schema passes a SCALAR enum, which encodes as
// text and coerces server-side without any registration at all. That is why an
// enum-ARRAY parameter is the specific thing this test exercises.
func TestEnumArrayParameterEncodes(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	userID, projectID := seedKeyholder(t, ctx, pool)

	// Two keys: one read, one write. Revoking only 'write' must take exactly one.
	for _, tc := range []struct {
		name   string
		scope  repository.ApiKeyScope
		prefix string
		seed   byte
	}{
		{"reader", repository.ApiKeyScopeRead, "bkry_readkey", 0x01},
		{"writer", repository.ApiKeyScopeWrite, "bkry_writkey", 0x02},
	} {
		if _, err := q.CreateAPIKey(ctx, repository.CreateAPIKeyParams{
			UserID:      userID,
			ProjectID:   projectID,
			Name:        tc.name,
			TokenSha256: bytes32(tc.seed),
			TokenPrefix: tc.prefix,
			Scope:       tc.scope,
		}); err != nil {
			t.Fatalf("seed %s key: %v", tc.name, err)
		}
	}

	// The call that has never once succeeded.
	revoked, err := q.RevokeAPIKeysForMembership(ctx, repository.RevokeAPIKeysForMembershipParams{
		UserID:    userID,
		ProjectID: projectID,
		Scopes:    []repository.ApiKeyScope{repository.ApiKeyScopeWrite},
	})
	if err != nil {
		// Before the fix this is:
		//   failed to encode args[2]: unable to encode
		//   []repository.ApiKeyScope{"write"} into text format for unknown type
		//   (OID 16419): cannot find encode plan
		t.Fatalf("RevokeAPIKeysForMembership with an enum-array parameter: %v", err)
	}

	if revoked != 1 {
		t.Fatalf("revoked %d keys, want 1", revoked)
	}

	// Semantics, not just "it did not error": the write key is gone and the read
	// key survives. An encoding that dropped or mangled the array could still
	// return 1 by revoking the wrong row.
	keys, err := q.ListAPIKeysForUser(ctx, userID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}

	for _, k := range keys {
		revokedNow := k.RevokedAt.Valid
		want := k.Scope == repository.ApiKeyScopeWrite

		if revokedNow != want {
			t.Errorf("key %q (scope %s): revoked=%v, want %v", k.Name, k.Scope, revokedNow, want)
		}
	}
}

// TestEnumArrayEncodesOnEveryPooledConn proves the registration is a property of
// the POOL, not of one lucky connection.
//
// AfterConnect runs per connection, so a fix that only registered types on the
// first one would pass the test above and then fail in production the moment the
// pool grew under load or recycled a connection at MaxConnLifetime. Pool.Reset
// closes every existing connection, so the second wave is served entirely by
// connections built after the first.
func TestEnumArrayEncodesOnEveryPooledConn(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	userID, _ := seedKeyholder(t, ctx, pool)

	const conns = 8

	probe := func(t *testing.T, label string) {
		t.Helper()

		var wg sync.WaitGroup

		errs := make(chan error, conns)

		for range conns {
			wg.Add(1)

			go func() {
				defer wg.Done()

				var n int64
				// An enum-array parameter, on whatever connection the pool hands us.
				err := pool.QueryRow(ctx,
					`SELECT count(*) FROM api_keys WHERE user_id = $1 AND scope = ANY($2::api_key_scope[])`,
					userID, []repository.ApiKeyScope{repository.ApiKeyScopeRead, repository.ApiKeyScopeWrite},
				).Scan(&n)
				if err != nil {
					errs <- err
				}
			}()
		}

		wg.Wait()
		close(errs)

		for err := range errs {
			t.Errorf("%s: enum-array query failed: %v", label, err)
		}
	}

	probe(t, "initial connections")

	// Every connection opened above is now closed. The next wave is all-new --
	// exactly what MaxConnLifetime recycling does in a long-running server.
	pool.Reset()
	probe(t, "connections rebuilt after Reset")

	if opened := pool.Stat().NewConnsCount(); opened < 2 {
		t.Fatalf("pool only ever opened %d connections; the churn this test needs did not happen", opened)
	}
}

// TestNewPoolRefusesDatabaseMissingAnEnum pins the fail-loudly decision.
//
// conn.LoadTypes is a silent partial-success API: nil error, short slice. If
// registerEnumTypes trusted it, an enum added to a migration but not to the
// enums list -- or renamed out from under the binary -- would leave a pool that
// looks healthy and then fails deep inside a query with "cannot find encode
// plan". The whole point of the fix is that this is impossible, so the missing
// type has to take the connection, and therefore the boot, down.
func TestNewPoolRefusesDatabaseMissingAnEnum(t *testing.T) {
	t.Parallel()

	_, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	// Drop one enum. Everything else about the database is intact.
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, `DROP TABLE gc_runs`); err != nil {
		t.Fatalf("drop gc_runs: %v", err)
	}

	if _, err := admin.Exec(ctx, `DROP TYPE gc_run_status`); err != nil {
		t.Fatalf("drop gc_run_status: %v", err)
	}

	pool, err := db.NewPool(ctx, db.Config{URL: dsn})
	if err == nil {
		pool.Close()
		t.Fatal("NewPool succeeded against a database missing gc_run_status; it must refuse")
	}

	if !errors.Is(err, db.ErrEnumTypeMissing) {
		t.Fatalf("error is not ErrEnumTypeMissing: %v", err)
	}

	if !strings.Contains(err.Error(), "gc_run_status") {
		t.Errorf("error does not name the missing type: %v", err)
	}
}

// seedKeyholder creates the org -> project -> user -> memberships chain an
// api_keys row needs. The composite FK means a key for a non-member cannot exist.
func seedKeyholder(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (userID, projectID pgtype.UUID) {
	t.Helper()

	var orgID pgtype.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('enum-org', 'enum org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1, 'enum-proj', 'enum proj') RETURNING id`,
		orgID,
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO users (issuer, subject, email, display_name)
		 VALUES ('dev', 'enum-subject', 'enum@example.test', 'enum user') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		userID, orgID,
	); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (user_id, org_id, project_id, role) VALUES ($1, $2, $3, 'writer')`,
		userID, orgID, projectID,
	); err != nil {
		t.Fatalf("seed project membership: %v", err)
	}

	return userID, projectID
}

// bytes32 builds a distinct 32-byte token hash (api_keys CHECKs octet_length = 32).
func bytes32(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}

	return b
}
