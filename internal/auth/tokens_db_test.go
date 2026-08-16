package auth

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// ---------------------------------------------------------------------------
// User tokens: live roles, epoch invalidation, instant revocation.
// ---------------------------------------------------------------------------

// TestRoleChangeInvalidatesViaEpoch is the F4 gate, and the property it asserts
// is that THERE IS NO INVALIDATION CODE.
//
// Nothing in this test calls Evict, and nothing in internal/auth has an Evict to
// call. Each case mutates authority through the ordinary query -- the same
// statement the API handler issues -- and then re-authenticates the SAME token.
// The new authority must be visible immediately, because the database bumped
// users.authz_epoch by trigger and the cache key therefore changed.
//
// The cases deliberately include the two an Evict-based design gets wrong in
// practice: a change made by a CASCADE (no Go statement runs at all) and a change
// to the site role, which lives on a different table from the memberships.
func TestRoleChangeInvalidatesViaEpoch(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	// Prime the cache: this is the entry every case below must be unable to reach.
	if _, err := fix.svc.AuthenticateToken(ctx, fix.userToken); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A second project in the same org, with no membership, so a grant can be
	// observed appearing rather than only disappearing.
	other, err := fix.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: fix.orgID, Slug: "bootloader", Name: "Bootloader",
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}

	tests := []struct {
		name string
		// mutate changes the user's authority through the production query.
		mutate func(t *testing.T)
		// check asserts what the token may do AFTER the change.
		check func(t *testing.T, p Principal)
	}{
		{
			name: "granting a project role",
			mutate: func(t *testing.T) {
				grantProjectRole(t, fix.testService, fix.userID, other.ID, ProjectRoleWriter)
			},
			check: func(t *testing.T, p Principal) {
				if !p.CanWriteProject(fix.orgID, other.ID) {
					t.Error("a freshly granted project role is not visible to a live token")
				}
			},
		},
		{
			name: "revoking a project role",
			mutate: func(t *testing.T) {
				if _, err := fix.store.DeleteProjectMembership(ctx, repository.DeleteProjectMembershipParams{
					UserID: fix.userID, ProjectID: other.ID,
				}); err != nil {
					t.Fatalf("delete project membership: %v", err)
				}
			},
			check: func(t *testing.T, p Principal) {
				if p.CanWriteProject(fix.orgID, other.ID) {
					t.Error("a revoked project role is still writable by a live token")
				}
			},
		},
		{
			name: "granting the site role",
			mutate: func(t *testing.T) {
				if _, err := fix.store.GrantSiteAdminLocal(ctx, repository.GrantSiteAdminLocalParams{
					ID: fix.userID, GrantedBy: fix.userID,
				}); err != nil {
					t.Fatalf("grant site admin: %v", err)
				}
			},
			check: func(t *testing.T, p Principal) {
				// The SITE ROLE changed, so the epoch must have moved -- but a personal
				// access token still must not BE a site admin. Both halves matter: the
				// first says the cache noticed, the second says noticing changed nothing
				// it should not.
				if p.SiteRole() != SiteRoleAdmin {
					t.Error("a site-role grant is not visible to a live token; the epoch did not move")
				}

				if p.IsSiteAdmin() {
					t.Error("a personal access token became a site admin")
				}
			},
		},
		{
			name: "revoking the site role",
			mutate: func(t *testing.T) {
				if _, err := fix.store.RevokeSiteAdminLocal(ctx, fix.userID); err != nil {
					t.Fatalf("revoke site admin: %v", err)
				}
			},
			check: func(t *testing.T, p Principal) {
				if p.SiteRole() != SiteRoleUser {
					t.Error("a site-role revocation is not visible to a live token")
				}
			},
		},
		{
			name: "revoking the ORG membership, which CASCADES the project role",
			mutate: func(t *testing.T) {
				grantProjectRole(t, fix.testService, fix.userID, other.ID, ProjectRoleWriter)

				if _, err := fix.store.DeleteLocalOrgMembership(ctx,
					repository.DeleteLocalOrgMembershipParams{UserID: fix.userID, OrgID: fix.orgID},
				); err != nil {
					t.Fatalf("delete org membership: %v", err)
				}
			},
			check: func(t *testing.T, p Principal) {
				// NO GO STATEMENT RAN for the project_memberships rows -- the database
				// cascaded them. An Evict()-based cache has nothing to hook here, which is
				// precisely why the epoch lives in the database.
				if p.CanReadProject(fix.orgID, fix.projectID) {
					t.Error("a token still reads a project after its owner left the org")
				}

				if p.CanWriteProject(fix.orgID, other.ID) {
					t.Error("a token still writes a project whose role was CASCADE-deleted")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(t)

			p, err := fix.svc.AuthenticateToken(ctx, fix.userToken)
			if err != nil {
				t.Fatalf("re-authenticate: %v", err)
			}

			tt.check(t, p)
		})
	}

	// The strandedness is the mechanism, so assert it directly: every mutation left
	// its predecessor's entry behind under an epoch nothing will ever ask for again.
	// That is what the TTL is for, and it is the only thing the TTL is for.
	if n := fix.svc.principals.len(); n < 2 {
		t.Errorf("the principal cache holds %d entries after %d authority changes; "+
			"entries are being UPDATED in place, which means the key is not epoch-scoped",
			n, len(tests))
	}
}

// TestUserTokenRevocationIsInstant. The role set is cached; the TOKEN never is.
// Revocation is a predicate on the probe, so it takes effect on the very next
// request with no TTL to wait out and no cache to invalidate.
func TestUserTokenRevocationIsInstant(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	if _, err := fix.svc.AuthenticateToken(ctx, fix.userToken); err != nil {
		t.Fatalf("before revocation: %v", err)
	}

	tokens, err := fix.store.ListUserTokensForUser(ctx, fix.userID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("list user tokens: %v (%d rows)", err, len(tokens))
	}

	n, err := fix.store.RevokeUserToken(ctx, repository.RevokeUserTokenParams{
		ID: tokens[0].ID, UserID: fix.userID,
	})
	if err != nil || n != 1 {
		t.Fatalf("revoke: %v (%d rows)", err, n)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.userToken); err == nil {
		t.Fatal("a revoked personal access token still authenticates")
	}

	// Idempotent, and scoped to the owner: a second revoke reports no rows rather
	// than succeeding, and another user's id cannot revoke this token at all.
	if n, err := fix.store.RevokeUserToken(ctx, repository.RevokeUserTokenParams{
		ID: tokens[0].ID, UserID: fix.userID,
	}); err != nil || n != 0 {
		t.Errorf("re-revoking reported %d rows (err %v), want 0", n, err)
	}
}

// TestExpiredUserTokenIsRefused. The predicate is on the probe, like revocation.
func TestExpiredUserTokenIsRefused(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	token, err := GenerateToken(UserTokenPrefix)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// created_at defaults to now(), and the CHECK requires expires_at > created_at,
	// so the only way to have an EXPIRED row is to insert a future expiry and then
	// move it back. That the schema refuses to accept an already-expired token at
	// INSERT is itself worth knowing.
	if _, err := fix.store.CreateUserToken(ctx, repository.CreateUserTokenParams{
		UserID: fix.userID, Name: "expiring",
		TokenSha256: token.Hash, TokenPrefix: token.Prefix, MaxScope: ScopeRead,
		ExpiresAt: pgtype.Timestamptz{
			Time: time.Now().Add(time.Hour), InfinityModifier: pgtype.Finite, Valid: true,
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, token.Token); err != nil {
		t.Fatalf("an unexpired token was refused: %v", err)
	}

	// created_at moves back too: user_tokens_expires_after_created refuses a row
	// whose expiry precedes its creation, so the schema will not let a test (or a
	// handler) manufacture an already-expired token by writing the expiry alone.
	if _, err := fix.pool.Exec(ctx, `
		UPDATE user_tokens
		   SET created_at = now() - interval '2 hours',
		       expires_at = now() - interval '1 second'
		 WHERE token_sha256 = $1`, token.Hash,
	); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, token.Token); err == nil {
		t.Fatal("an expired personal access token still authenticates")
	}
}

// TestUserTokenProbeIsOneIndexOnlyScanAndAPKJoin is the plan gate.
//
// The probe runs on every request of a BB_NUMBER_THREADS-parallel sstate HEAD
// storm. "One round trip" is not the claim -- one round trip is obvious from the
// Go code. The claim is that the round trip is an INDEX ONLY SCAN with no heap
// fetches, joined to users on its PRIMARY KEY, and that neither table is ever
// sequentially scanned. A covering index that stops covering (someone drops a
// column from the INCLUDE list) is invisible to every other test in this package
// and shows up here.
func TestUserTokenProbeIsOneIndexOnlyScanAndAPKJoin(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	// Enough rows that the planner has a real choice to make. Against a three-row
	// table it would pick a sequential scan and this test would assert nothing.
	if _, err := fix.pool.Exec(ctx, `
		INSERT INTO users (issuer, subject, email)
		SELECT 'https://idp.example.com', 'bulk-' || i, 'bulk-' || i || '@example.com'
		  FROM generate_series(1, 5000) i`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	if _, err := fix.pool.Exec(ctx, `
		INSERT INTO user_tokens (user_id, name, token_sha256, token_prefix)
		SELECT u.id, 'bulk', sha256(u.subject::bytea), 'bkru_bulkbulk'
		  FROM users u WHERE u.subject LIKE 'bulk-%'`); err != nil {
		t.Fatalf("seed user tokens: %v", err)
	}

	// VACUUM sets the visibility map, which is what makes Heap Fetches: 0 reachable
	// at all -- an index-only scan on a freshly written table still visits the heap.
	// ANALYZE gives the planner the statistics it needs to prefer the index.
	if _, err := fix.pool.Exec(ctx, `VACUUM ANALYZE users, user_tokens`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}

	var raw []byte
	if err := fix.pool.QueryRow(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) `+validateUserTokenSQL, HashToken(fix.userToken),
	).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}

	plan := string(raw)

	if strings.Contains(plan, `"Seq Scan"`) {
		t.Errorf("the user-token probe sequentially scans a table:\n%s", plan)
	}

	if !strings.Contains(plan, `"Index Only Scan"`) ||
		!strings.Contains(plan, `"Index Name": "user_tokens_token_sha256_key"`) {
		t.Errorf("the user-token probe does not ride user_tokens_token_sha256_key "+
			"as an Index Only Scan:\n%s", plan)
	}

	if !strings.Contains(plan, `"Index Name": "users_pkey"`) {
		t.Errorf("the users join is not a primary-key lookup:\n%s", plan)
	}

	// The COVERING half, asserted on the index definition rather than on a heap-fetch
	// count.
	//
	// "Heap Fetches: 0" is the property we actually want, and it is not reliably
	// observable here: an index-only scan visits the heap unless the page is marked
	// all-visible, and VACUUM can only mark it once no snapshot anywhere in the
	// CLUSTER predates the insert. dbtest runs every package's tests against one
	// server, so a sibling test's open transaction holds the horizon back and the
	// count flickers between 0 and 1 for reasons that have nothing to do with this
	// index. Asserting the index DEFINITION instead is deterministic and strictly
	// stronger: it fails the moment a column the probe reads leaves the INCLUDE
	// list, which is the change that would make the heap fetch permanent.
	var indexdef string

	if err := fix.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'user_tokens_token_sha256_key'`,
	).Scan(&indexdef); err != nil {
		t.Fatalf("read index definition: %v", err)
	}

	covered := []string{"token_sha256", "id", "user_id", "max_scope", "expires_at", "revoked_at"}
	for _, column := range covered {
		if !strings.Contains(indexdef, column) {
			t.Errorf("user_tokens_token_sha256_key does not cover %q, so the probe must "+
				"fetch from the heap on every sstate HEAD:\n%s", column, indexdef)
		}
	}
}

// ---------------------------------------------------------------------------
// The principal cache.
// ---------------------------------------------------------------------------

// TestPrincipalCacheIsSharded. A single-mutex cache gets SLOWER as parallelism
// rises -- the blob LRU measured 170ns/op at -cpu 8 degrading to 246ns/op at
// -cpu 64 -- which is exactly backwards under the HEAD storm this cache exists
// for. Sharding is not an optimization here, it is the difference between a cache
// and a global lock.
//
// This asserts the two properties a benchmark cannot: that the keys really are
// spread across shards, and that concurrent readers and writers do not race.
// BenchmarkPrincipalCache below measures the scaling.
func TestPrincipalCacheIsSharded(t *testing.T) {
	t.Parallel()

	cache := newPrincipalCache()
	fill := func(_ context.Context, id pgtype.UUID) (*userAuthz, error) {
		return &userAuthz{userID: id}, nil
	}

	const users = 512

	var wg sync.WaitGroup

	for i := range users {
		wg.Add(1)

		go func() {
			defer wg.Done()

			id := uuid(byte(i))
			id.Bytes[15] = byte(i >> 8)

			for epoch := range int64(4) {
				if _, err := cache.load(t.Context(), id, epoch, fill); err != nil {
					t.Errorf("load: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	occupied := 0

	for i := range cache.shards {
		cache.shards[i].mu.Lock()
		if len(cache.shards[i].m) > 0 {
			occupied++
		}
		cache.shards[i].mu.Unlock()
	}

	if occupied != principalCacheShards {
		t.Errorf("%d of %d shards hold entries; the hash is not spreading keys",
			occupied, principalCacheShards)
	}
}

// TestPrincipalCacheTTLIsGarbageCollectionOnly. Expiry must never be load-bearing
// for correctness -- the epoch is -- but an entry nobody will ever ask for again
// must not live forever.
func TestPrincipalCacheTTLIsGarbageCollectionOnly(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newPrincipalCache()
	cache.now = func() time.Time { return now }

	fills := 0
	fill := func(_ context.Context, id pgtype.UUID) (*userAuthz, error) {
		fills++

		return &userAuthz{userID: id}, nil
	}

	id := uuid(0x07)

	for range 3 {
		if _, err := cache.load(t.Context(), id, 1, fill); err != nil {
			t.Fatalf("load: %v", err)
		}
	}

	if fills != 1 {
		t.Errorf("fills = %d, want 1: the cache is not caching", fills)
	}

	now = now.Add(principalCacheTTL + time.Second)

	if _, err := cache.load(t.Context(), id, 1, fill); err != nil {
		t.Fatalf("load after expiry: %v", err)
	}

	if fills != 2 {
		t.Errorf("fills = %d, want 2: an expired entry was served", fills)
	}
}

// BenchmarkPrincipalCache is the scaling measurement. Run it at -cpu 1,8,64: a
// sharded cache holds roughly flat, a single-mutex one degrades.
func BenchmarkPrincipalCache(b *testing.B) {
	cache := newPrincipalCache()
	fill := func(_ context.Context, id pgtype.UUID) (*userAuthz, error) {
		return &userAuthz{userID: id}, nil
	}

	ctx := b.Context()

	for i := range 256 {
		id := uuid(byte(i))
		if _, err := cache.load(ctx, id, 1, fill); err != nil {
			b.Fatalf("prime: %v", err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0

		for pb.Next() {
			id := uuid(byte(i % 256))
			if _, err := cache.load(ctx, id, 1, fill); err != nil {
				b.Fatalf("load: %v", err)
			}

			i++
		}
	})
}

// ---------------------------------------------------------------------------
// Robots.
// ---------------------------------------------------------------------------

// TestRobotCannotAdminAnything, table-driven over every capability method.
//
// The principal under test is a REAL one, produced by the production validator
// from a real org_tokens row with WRITE scope -- the most privileged robot the
// schema can express. It reads and writes its org's projects and does nothing
// else, ever.
func TestRobotCannotAdminAnything(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)

	p, err := fix.svc.AuthenticateToken(t.Context(), fix.orgToken)
	if err != nil {
		t.Fatalf("authenticate robot: %v", err)
	}

	denied := map[string]bool{
		"IsSiteAdmin":     p.IsSiteAdmin(),
		"CanViewOrg":      p.CanViewOrg(fix.orgID),
		"CanAdminOrg":     p.CanAdminOrg(fix.orgID),
		"CanOwnOrg":       p.CanOwnOrg(fix.orgID),
		"CanAdminProject": p.CanAdminProject(fix.orgID, fix.projectID),
	}

	for name, granted := range denied {
		if granted {
			t.Errorf("%s = true for a write-scoped robot; a robot administers nothing", name)
		}
	}

	if !p.CanReadProject(fix.orgID, fix.projectID) || !p.CanWriteProject(fix.orgID, fix.projectID) {
		t.Error("a write-scoped robot cannot read or write its own org's project")
	}

	// Another org's project: the org id is the whole decision, so a different one is
	// a flat refusal with no lookup and no membership to fall through to.
	if p.CanReadProject(uuid(0xbb), fix.projectID) || p.CanWriteProject(uuid(0xbb), fix.projectID) {
		t.Error("a robot reached a project outside its own organization")
	}

	// It carries no identity at all -- there is no user row behind it.
	if p.UserID().Valid || p.Email() != "" || p.SiteRole() != SiteRoleUser {
		t.Error("a robot principal carries user identity it has no source for")
	}

	if _, ok := p.APIKey(); ok {
		t.Error("a robot principal reports an API key grant")
	}
}

// TestReadScopedRobotCannotWrite. The scope is on the row, and the probe returns
// it, so this is decided from the index tuple with no second lookup.
func TestReadScopedRobotCannotWrite(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)

	_, token := seedRobotToken(t, fix.testService, "reader", fix.orgID, fix.userID, ScopeRead)

	p, err := fix.svc.AuthenticateToken(t.Context(), token)
	if err != nil {
		t.Fatalf("authenticate read-scoped robot: %v", err)
	}

	if !p.CanReadProject(fix.orgID, fix.projectID) {
		t.Error("a read-scoped robot cannot read")
	}

	if p.CanWriteProject(fix.orgID, fix.projectID) {
		t.Error("a read-scoped robot wrote")
	}
}

// TestRobotHasNoOrgMembershipRow is the structural half of "no touchy".
//
// CanReadProject has a branch that grants read on every project in an org the
// principal is a MEMBER of. That branch is one refactor away from being reachable
// by anything with an org_memberships row. A robot must never have one -- then the
// branch is unreachable for it no matter what anyone writes later, and the schema
// is what says so rather than a guard.
func TestRobotHasNoOrgMembershipRow(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	members, err := fix.store.ListOrgMembers(ctx, fix.orgID)
	if err != nil {
		t.Fatalf("list org members: %v", err)
	}

	// The one human, and nobody else. A robot cannot appear here because
	// org_memberships.user_id references users, and a robot has no users row.
	if len(members) != 1 || members[0].UserID != fix.userID {
		t.Errorf("org members = %d rows (%+v), want exactly the one human", len(members), members)
	}

	var memberships int
	if err := fix.pool.QueryRow(ctx,
		`SELECT count(*) FROM org_memberships WHERE user_id = (SELECT id FROM robots LIMIT 1)`,
	).Scan(&memberships); err != nil {
		t.Fatalf("count: %v", err)
	}

	if memberships != 0 {
		t.Errorf("a robot id appears in org_memberships %d time(s)", memberships)
	}
}

// TestRobotWritesProjectCreatedAfterMint. "Current and future projects" is not a
// feature that had to be built: the decision never names a project, so a project
// created after the token was minted is covered with zero provisioning.
func TestRobotWritesProjectCreatedAfterMint(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	p, err := fix.svc.AuthenticateToken(ctx, fix.orgToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	later, err := fix.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: fix.orgID, Slug: "created-later", Name: "Created later",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Note the principal was resolved BEFORE the project existed and is not
	// re-fetched: there is nothing to re-fetch, which is the point.
	if !p.CanWriteProject(fix.orgID, later.ID) {
		t.Error("a robot cannot write a project created after its token was minted")
	}
}

// TestOrgTokenOutlivesItsCreator is a DOCUMENTING test: it pins a deliberate
// product decision so that it is greppable and cannot be changed by accident.
//
// A robot is an ORG-OWNED identity, not a delegation. Deleting the human who
// created it does NOT revoke its tokens -- otherwise a CI pipeline breaks the day
// the engineer who set it up leaves, which is the failure this design exists to
// avoid. The countervailing controls are the NOT NULL expiry and the
// created_by_email snapshot that keeps the audit trail attributable after
// created_by goes NULL.
//
// Contrast api_keys, whose whole revocation story is the cascade
// org_memberships -> project_memberships -> api_keys. That cascade deliberately
// does not reach here.
func TestOrgTokenOutlivesItsCreator(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	// Delete the human. api_keys they held go with them; the robot does not.
	if _, err := fix.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, fix.userID); err != nil {
		t.Fatalf("delete creator: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.apiKey); err == nil {
		t.Error("an API key survived the deletion of its owner")
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.userToken); err == nil {
		t.Error("a personal access token survived the deletion of its owner")
	}

	p, err := fix.svc.AuthenticateToken(ctx, fix.orgToken)
	if err != nil {
		t.Fatalf("the robot token did NOT outlive its creator: %v.\n"+
			"This is a deliberate product decision -- see the test doc -- so if it is "+
			"being changed, change the doc and the expiry policy with it.", err)
	}

	if !p.CanWriteProject(fix.orgID, fix.projectID) {
		t.Error("the surviving robot token lost its authority")
	}

	// The audit trail survives the human, which is the price the decision pays.
	var email string

	var createdBy pgtype.UUID

	if err := fix.pool.QueryRow(ctx,
		`SELECT created_by, created_by_email FROM org_tokens LIMIT 1`,
	).Scan(&createdBy, &email); err != nil {
		t.Fatalf("read audit columns: %v", err)
	}

	if createdBy.Valid {
		t.Error("created_by was not SET NULL when its referent was deleted")
	}

	if email != "anna@example.com" {
		t.Errorf("created_by_email = %q; the audit snapshot did not survive the human", email)
	}
}

// TestDeletingRobotRevokesEveryToken: the first structural revocation leg.
func TestDeletingRobotRevokesEveryToken(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	// A second token on the SAME robot, so "every token" is a real claim.
	second, err := GenerateToken(OrgTokenPrefix)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if _, err := fix.store.CreateOrgToken(ctx, repository.CreateOrgTokenParams{
		RobotID: fix.robotID, OrgID: fix.orgID, Name: "secondary",
		TokenSha256: second.Hash, TokenPrefix: second.Prefix, Scope: ScopeRead,
		ExpiresAt: pgtype.Timestamptz{
			Time: time.Now().Add(time.Hour), InfinityModifier: pgtype.Finite, Valid: true,
		},
		CreatedBy: fix.userID, CreatedByEmail: "anna@example.com",
	}); err != nil {
		t.Fatalf("create second token: %v", err)
	}

	n, err := fix.store.DeleteRobotForOrg(ctx, repository.DeleteRobotForOrgParams{
		ID: fix.robotID, OrgID: fix.orgID,
	})
	if err != nil || n != 1 {
		t.Fatalf("delete robot: %v (%d rows)", err, n)
	}

	for name, tok := range map[string]string{"first": fix.orgToken, "second": second.Token} {
		if _, err := fix.svc.AuthenticateToken(ctx, tok); err == nil {
			t.Errorf("the %s token survived the deletion of its robot", name)
		}
	}
}

// TestDeletingOrgRevokesEveryRobotToken: the second structural revocation leg.
//
// Both FKs on org_tokens (robot_id and the denormalized org_id) cascade, so the
// tokens die whichever path the delete takes. Note the org delete only succeeds
// once its projects are gone -- organizations are ON DELETE RESTRICT down to
// projects on purpose, so this is the real sequence an operator performs.
func TestDeletingOrgRevokesEveryRobotToken(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	if _, err := fix.store.DeleteProject(ctx, fix.projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	if _, err := fix.store.DeleteOrganization(ctx, fix.orgID); err != nil {
		t.Fatalf("delete org: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.orgToken); err == nil {
		t.Error("a robot token survived the deletion of its organization")
	}

	var robots int
	if err := fix.pool.QueryRow(ctx, `SELECT count(*) FROM robots`).Scan(&robots); err != nil {
		t.Fatalf("count robots: %v", err)
	}

	if robots != 0 {
		t.Errorf("%d robot(s) survived their organization", robots)
	}
}

// ---------------------------------------------------------------------------
// The reconciler cannot address a robot.
// ---------------------------------------------------------------------------

// queryNamePattern matches sqlc's `-- name: X :kind` markers.
var queryNamePattern = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)\s`)

// TestReconcileNeverNamesRobotTables is the load-bearing test of the robot
// design, and it is deliberately a SOURCE test rather than a behavioural one.
//
// The claim the separate-tables decision buys is not "the reconciler happens not
// to touch robots today". It is "there is no SQL on the login path that can
// ADDRESS a robot". A behavioural test can only sample the inputs someone thought
// of; this reads every statement the login path can reach and asserts that none
// of them names either table.
//
// It fails loudly the day someone adds a convenience join -- "list the org's
// robots while we are reconciling anyway" -- which is exactly the refactor that
// would quietly reconnect a login to a machine identity.
func TestReconcileNeverNamesRobotTables(t *testing.T) {
	t.Parallel()

	queries := loadQueryBodies(t)
	reachable := loginPathQueries(t, queries)

	if len(reachable) < 5 {
		t.Fatalf("only found %d login-path queries (%v); the source scan has lost the "+
			"call sites it is supposed to be reading", len(reachable), reachable)
	}

	forbidden := regexp.MustCompile(`\b(robots|org_tokens)\b`)

	for _, name := range reachable {
		if forbidden.MatchString(queries[name]) {
			t.Errorf("the login-path query %s names a robot table.\n"+
				"Robots live in separate tables precisely so that a login CANNOT address "+
				"one; naming them here re-opens the reconciler onto machine identities.\n%s",
				name, queries[name])
		}
	}

	// And the file the reconciler's statements live in, as a whole.
	identity, err := os.ReadFile(filepath.Join(queryDir(t), "identity.sql"))
	if err != nil {
		t.Fatalf("read identity.sql: %v", err)
	}

	if forbidden.Match(identity) {
		t.Error("internal/db/query/identity.sql names a robot table")
	}
}

func queryDir(t *testing.T) string {
	t.Helper()

	return filepath.Join("..", "db", "query")
}

// loadQueryBodies reads every sqlc query in internal/db/query into name -> SQL.
func loadQueryBodies(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(queryDir(t))
	if err != nil {
		t.Fatalf("read query dir: %v", err)
	}

	out := map[string]string{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(queryDir(t), entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}

		text := string(raw)
		matches := queryNamePattern.FindAllStringSubmatchIndex(text, -1)

		for i, m := range matches {
			end := len(text)
			if i+1 < len(matches) {
				end = matches[i+1][0]
			}

			out[text[m[2]:m[3]]] = text[m[1]:end]
		}
	}

	if len(out) == 0 {
		t.Fatal("found no sqlc queries; the marker pattern has drifted from the files")
	}

	return out
}

// robotPlaneFile is the ONE file in this package that is allowed to name the
// robot tables, because it IS the robot plane: orgtoken.go holds
// validateOrgToken and CreateOrgToken and nothing else does.
//
// Excluding it by NAME, from a scan that otherwise reads the whole package, is
// the shape that keeps the claim structural. The alternative -- enumerating the
// files the login path is believed to occupy -- cannot notice a login-path helper
// that moves to a fifth file, which is the one thing a source scan defending a
// structural property must never miss.
const robotPlaneFile = "orgtoken.go"

// loginPathQueries reads the SOURCE of every non-test file in this package
// EXCEPT the robot plane's own, and returns every sqlc query they call.
//
// It used to take a hand-written four-file list (reconcile.go, oidc.go,
// service.go, devlogin.go). That list was complete on the day it was written and
// could not stay so: a login-path helper extracted to a new file would simply
// stop being scanned, silently, while the remaining queries kept the `len < 5`
// guard below satisfied. The property being defended -- "no SQL a login can reach
// can ADDRESS a robot" -- is structural, so the scan has to be too.
//
// A method call whose name matches a declared query is counted, whatever the
// receiver -- s.store.X, q.X or a transaction-bound Queries -- so rebinding onto
// a transaction does not hide a call from this scan.
func loginPathQueries(t *testing.T, queries map[string]string) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	var files []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		if name == robotPlaneFile {
			continue
		}

		files = append(files, name)
	}

	if len(files) == 0 {
		t.Fatal("found no non-test Go files to scan; the source scan has lost its inputs")
	}

	var found []string

	seen := map[string]bool{}

	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if _, isQuery := queries[sel.Sel.Name]; isQuery && !seen[sel.Sel.Name] {
				seen[sel.Sel.Name] = true
				found = append(found, sel.Sel.Name)
			}

			return true
		})
	}

	return found
}
