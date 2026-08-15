// M6: GC retention + quotas (migration 000012). package db_test for the same
// reason as db_test.go: these need a real Postgres, and the harness that
// provides one imports internal/db.
//
// docs/design/specs/2026-08-14-m6-gc-retention-quotas.md is the contract.
package db_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// seedOrgCounter makes every seedBackendKind call's org slug unique WITHIN one
// test's database, unlike db_test.go's seedBackend (hard-coded "acme"/"widget",
// fine for callers that seed exactly once). Several tests below need two
// differently-kinded backends -- e.g. a hashserv backend to prove a CHECK fires
// and a non-hashserv one to prove it does not fire everywhere -- and
// organizations_slug_key would collide on a second hard-coded "acme".
var seedOrgCounter atomic.Int64

// seedBackendKind is seedBackend (db_test.go), parameterised over kind and given
// its own org+project per call so multiple calls in one test's database never
// collide on organizations_slug_key.
func seedBackendKind(t *testing.T, pool *pgxpool.Pool, kind repository.BackendKind) int64 {
	t.Helper()

	ctx := t.Context()
	q := repository.New(pool)
	n := seedOrgCounter.Add(1)

	org, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: fmt.Sprintf("acme-%d", n), Name: "Acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	project, err := q.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	backend, err := q.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        project.ID,
		Kind:             kind,
		Enabled:          true,
		ReadAuthRequired: true,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}

	return backend.ID
}

// startRealGCRun is StartGCRun with an interval-triggered, non-dry real run --
// the ordinary case every write-barrier test below needs, spelled once so the
// params literal is not repeated at every call site.
func startRealGCRun(t *testing.T, ctx context.Context, q *repository.Queries, grace pgtype.Interval) repository.StartGCRunRow {
	t.Helper()

	run, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "interval", DryRun: false,
	})
	if err != nil {
		t.Fatalf("StartGCRun: %v", err)
	}

	return run
}

// finishGCRun finishes a run and asserts the update actually landed (rows == 1),
// so a silently no-op FinishGCRun -- exactly the failure
// TestFinishGCRunCannotResurrectATerminalRun pins on purpose in the OTHER
// direction -- cannot pass this helper's callers by accident.
func finishGCRun(t *testing.T, ctx context.Context, q *repository.Queries, runID int64) {
	t.Helper()

	rows, err := q.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID:     runID,
		Status: repository.GcRunStatusSucceeded,
		Error:  pgtype.Text{},
	})
	if err != nil {
		t.Fatalf("FinishGCRun: %v", err)
	}

	if rows != 1 {
		t.Fatalf("FinishGCRun updated %d rows, want 1", rows)
	}
}

// --- the hashserv write barrier (SweepUnihashes) -----------------------------

// TestSweepUnihashesSparesAConcurrentBuild is
// TestGCWriteBarrierSparesAConcurrentBuild's shape (db_test.go), replicated
// against SweepUnihashes: hashserv_unihashes is the GC ROOT (CLAUDE.md, spec
// §3 stage 1), so its own write barrier gets its own reproduction of the bug it
// exists to prevent, not just a description that MarkBlobsPendingDelete's does.
//
// retention_window is zero throughout: the AGE predicate must never be what
// gates this test, only the write barrier.
func TestSweepUnihashesSparesAConcurrentBuild(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackendKind(t, pool, repository.BackendKindHashserv)

	// The "build": a transaction that starts BEFORE the GC run and commits AFTER
	// it, so its created_at predates gc_runs.started_at while its xid is not yet
	// visible in the run's snapshot.
	build, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin build: %v", err)
	}

	defer func() { _ = build.Rollback(ctx) }()

	if _, err := build.Exec(ctx,
		`INSERT INTO hashserv_unihashes (backend_id, method, taskhash, unihash)
		 VALUES ($1, 'stamp:do_compile', 'deadbeef', 'cafebabe')`, backendID,
	); err != nil {
		t.Fatalf("build insert: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	run := startRealGCRun(t, ctx, q, zeroGrace())

	if err := build.Commit(ctx); err != nil {
		t.Fatalf("build commit: %v", err)
	}

	// Vacuity guard: prove the timestamp half alone WOULD select this row, so a
	// zero result below is the barrier working and not an empty table.
	var tsBarrierWouldSweep bool
	if err := pool.QueryRow(ctx, `
		SELECT u.created_at < g.started_at
		  FROM hashserv_unihashes u, gc_runs g
		 WHERE u.backend_id = $1 AND g.id = $2`, backendID, run.ID,
	).Scan(&tsBarrierWouldSweep); err != nil {
		t.Fatalf("evaluate timestamp barrier: %v", err)
	}

	if !tsBarrierWouldSweep {
		t.Fatal("the timestamp barrier did not even select this row -- " +
			"the interleaving this test needs did not happen, so it proves nothing")
	}

	deleted, err := q.SweepUnihashes(ctx, repository.SweepUnihashesParams{
		RunID: run.ID, BackendID: backendID, RetentionWindow: zeroGrace(),
	})
	if err != nil {
		t.Fatalf("SweepUnihashes: %v", err)
	}

	if deleted != 0 {
		t.Fatalf("SweepUnihashes deleted %d rows of a still-running build's unihash, want 0 -- "+
			"the snapshot write barrier is not holding", deleted)
	}

	finishGCRun(t, ctx, q, run.ID)

	// A LATER run's fresh snapshot DOES see the build's xid, so the same row is
	// now legitimately sweepable -- without this leg, a barrier that spares
	// everything forever would pass.
	next := startRealGCRun(t, ctx, q, zeroGrace())

	deleted, err = q.SweepUnihashes(ctx, repository.SweepUnihashesParams{
		RunID: next.ID, BackendID: backendID, RetentionWindow: zeroGrace(),
	})
	if err != nil {
		t.Fatalf("SweepUnihashes (second): %v", err)
	}

	if deleted != 1 {
		t.Fatalf("the second sweep deleted %d rows, want 1 -- the barrier never converges "+
			"and unreferenced unihashes would leak forever", deleted)
	}
}

// --- the cache_objects write barrier (ScanObjectsForGC) ----------------------

// TestSweepObjectsSparesAConcurrentBuild is the same reproduction against
// ScanObjectsForGC, the generic per-backend keyset scan every Layer-A cache_objects
// stage is built from (query/gc.sql). ScanObjectsForGC carries ONLY the universal
// write barrier (spec §3's first two predicates) -- proving it here is what lets
// every stage built on top (sstate, ac/ac-grpc/sccache, OCI blobs/tags) inherit the
// guarantee without re-deriving it.
func TestSweepObjectsSparesAConcurrentBuild(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackend(t, pool) // bazel-kind; namespace is irrelevant here

	d := digest(0xC1)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(5),
	); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	// The "build": starts before the run, commits after it.
	build, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin build: %v", err)
	}

	defer func() { _ = build.Rollback(ctx) }()

	if _, err := build.Exec(ctx,
		`INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
		 VALUES ($1, 'cas', 'concurrent-object', $2, 5)`, backendID, d,
	); err != nil {
		t.Fatalf("build insert: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	run := startRealGCRun(t, ctx, q, zeroGrace())

	if err := build.Commit(ctx); err != nil {
		t.Fatalf("build commit: %v", err)
	}

	var tsBarrierWouldSweep bool
	if err := pool.QueryRow(ctx, `
		SELECT o.created_at < g.started_at
		  FROM cache_objects o, gc_runs g
		 WHERE o.backend_id = $1 AND o.namespace = 'cas' AND o.key = 'concurrent-object'
		   AND g.id = $2`, backendID, run.ID,
	).Scan(&tsBarrierWouldSweep); err != nil {
		t.Fatalf("evaluate timestamp barrier: %v", err)
	}

	if !tsBarrierWouldSweep {
		t.Fatal("the timestamp barrier did not even select this row -- " +
			"the interleaving this test needs did not happen, so it proves nothing")
	}

	rows, err := q.ScanObjectsForGC(ctx, repository.ScanObjectsForGCParams{
		BackendID: backendID, AfterNamespace: "", AfterKey: "", ScanLimit: 100, RunID: run.ID,
	})
	if err != nil {
		t.Fatalf("ScanObjectsForGC: %v", err)
	}

	if len(rows) != 0 {
		t.Fatalf("ScanObjectsForGC returned %d rows of a still-running build's object, want 0 -- "+
			"the snapshot write barrier is not holding", len(rows))
	}

	finishGCRun(t, ctx, q, run.ID)

	next := startRealGCRun(t, ctx, q, zeroGrace())

	rows, err = q.ScanObjectsForGC(ctx, repository.ScanObjectsForGCParams{
		BackendID: backendID, AfterNamespace: "", AfterKey: "", ScanLimit: 100, RunID: next.ID,
	})
	if err != nil {
		t.Fatalf("ScanObjectsForGC (second): %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("the second scan returned %d rows, want 1 -- the barrier never converges", len(rows))
	}
}

// --- finding 1's regression: NULL accessed_at must not be immortal -----------

// TestNullAccessedAtRowsAreSweepable is the coalesce(accessed_at, created_at)
// regression (spec §3, finding 1): every row that exists before this migration has
// accessed_at = NULL. A bare `accessed_at < now() - window` would make every one of
// them immortal (NULL compares to nothing as true) and make quota eviction
// non-terminating. SweepUnihashes's `coalesce(u.accessed_at, u.created_at)` must
// fall back to created_at and sweep a sufficiently old, never-touched row.
func TestNullAccessedAtRowsAreSweepable(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackendKind(t, pool, repository.BackendKindHashserv)

	// A row that is old enough to be swept by created_at, and whose accessed_at
	// has NEVER been set -- exactly the state of every pre-upgrade row.
	if _, err := pool.Exec(ctx,
		`INSERT INTO hashserv_unihashes (backend_id, method, taskhash, unihash, created_at)
		 VALUES ($1, 'stamp:do_compile', 'oldtaskhash', 'oldunihash', now() - interval '100 days')`,
		backendID,
	); err != nil {
		t.Fatalf("seed old, never-touched unihash: %v", err)
	}

	var accessedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx,
		`SELECT accessed_at FROM hashserv_unihashes WHERE backend_id = $1 AND taskhash = 'oldtaskhash'`,
		backendID,
	).Scan(&accessedAt); err != nil {
		t.Fatalf("read back accessed_at: %v", err)
	}

	if accessedAt.Valid {
		t.Fatal("seeded row already has accessed_at set -- this test proves nothing about the NULL case")
	}

	run := startRealGCRun(t, ctx, q, zeroGrace())

	deleted, err := q.SweepUnihashes(ctx, repository.SweepUnihashesParams{
		RunID: run.ID, BackendID: backendID,
		RetentionWindow: pgtype.Interval{Days: 90, Valid: true},
	})
	if err != nil {
		t.Fatalf("SweepUnihashes: %v", err)
	}

	if deleted != 1 {
		t.Fatalf("SweepUnihashes deleted %d rows, want 1 -- a NULL accessed_at row past the "+
			"created_at window must be sweepable via coalesce(accessed_at, created_at), not immortal", deleted)
	}
}

// --- FinishGCRun cannot resurrect a terminal run ------------------------------

// TestFinishGCRunCannotResurrectATerminalRun pins the `AND status = 'running'`
// predicate FinishGCRun gained in 000012 (spec §7.3, finding 4): once a run has
// finished, a second, stale FinishGCRun call -- the boot reaper racing the
// process's own shutdown finisher is the concrete scenario -- must touch zero rows
// and leave the row's terminal status and counters exactly as they were.
func TestFinishGCRunCannotResurrectATerminalRun(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	run := startRealGCRun(t, ctx, q, zeroGrace())

	rows, err := q.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID: run.ID, Status: repository.GcRunStatusSucceeded, Error: pgtype.Text{},
		ObjectsDeleted: 7,
	})
	if err != nil {
		t.Fatalf("first FinishGCRun: %v", err)
	}

	if rows != 1 {
		t.Fatalf("first FinishGCRun updated %d rows, want 1", rows)
	}

	// The stale second call: a different, terminal status and different counters.
	// If the predicate is missing this SUCCEEDS and clobbers the row above.
	rows, err = q.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID: run.ID, Status: repository.GcRunStatusFailed,
		Error:          pgtype.Text{String: "orphaned: process restarted mid-run", Valid: true},
		ObjectsDeleted: 0,
	})
	if err != nil {
		t.Fatalf("second (stale) FinishGCRun: %v", err)
	}

	if rows != 0 {
		t.Fatalf("the stale FinishGCRun updated %d rows, want 0 -- it resurrected a terminal run", rows)
	}

	var status repository.GcRunStatus

	var objectsDeleted int64
	if err := pool.QueryRow(ctx,
		`SELECT status, objects_deleted FROM gc_runs WHERE id = $1`, run.ID,
	).Scan(&status, &objectsDeleted); err != nil {
		t.Fatalf("read back the run: %v", err)
	}

	if status != repository.GcRunStatusSucceeded {
		t.Errorf("status = %q after the stale call, want %q -- the terminal state was overwritten",
			status, repository.GcRunStatusSucceeded)
	}

	if objectsDeleted != 7 {
		t.Errorf("objects_deleted = %d after the stale call, want 7 -- the counters were overwritten", objectsDeleted)
	}
}

// --- dry runs don't hold the active slot --------------------------------------

// TestDryRunDoesNotHoldTheActiveSlot pins 000012's replacement single-active
// index (spec §7.2, finding 14): `WHERE status = 'running' AND NOT dry_run`. Two
// concurrent dry runs must NOT collide with each other, and a dry run must not
// block a concurrent REAL run from starting -- while two real runs must still
// collide exactly as TestOnlyOneGCRunAtATime already proves.
func TestDryRunDoesNotHoldTheActiveSlot(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	grace := zeroGrace()

	if _, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "api", DryRun: true,
	}); err != nil {
		t.Fatalf("first dry run: %v", err)
	}

	// A second, concurrent dry run: must NOT collide with the first.
	if _, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "api", DryRun: true,
	}); err != nil {
		t.Fatalf("a second concurrent dry run was refused: %v -- dry runs must not serialise on each other", err)
	}

	// A REAL run, concurrent with both dry runs above: must also succeed. If dry
	// runs held the slot, this would collide with the unique index.
	real, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "interval", DryRun: false,
	})
	if err != nil {
		t.Fatalf("a real run was refused while only dry runs were active: %v -- "+
			"dry runs must not hold the active slot", err)
	}

	// A third dry run, concurrent with the now-active REAL run: must also succeed,
	// proving the exemption holds in both directions.
	if _, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "api", DryRun: true,
	}); err != nil {
		t.Fatalf("a dry run was refused while a real run was active: %v", err)
	}

	// But a SECOND real run, concurrent with the first, must still collide --
	// unchanged from TestOnlyOneGCRunAtATime.
	if _, err := q.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: grace, Trigger: "interval", DryRun: false,
	}); err == nil {
		t.Fatal("a second concurrent REAL run was allowed to start -- the single-active " +
			"slot for real runs is gone")
	}

	finishGCRun(t, ctx, q, real.ID)
}

// --- hashserv gets no quota, structurally -------------------------------------

// TestHashservQuotaIsRefused pins cache_backends_hashserv_no_quota (spec §7.4,
// finding 15): hashserv has no cache_objects rows, so a quota on it would be
// unenforceable by construction and read 0 forever -- a silent lie rather than an
// honest "no cap". The CHECK makes the state unrepresentable rather than trusting
// every future writer to remember the rule.
func TestHashservQuotaIsRefused(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	backendID := seedBackendKind(t, pool, repository.BackendKindHashserv)

	if _, err := pool.Exec(ctx,
		`UPDATE cache_backends SET quota_bytes = 1073741824 WHERE id = $1`, backendID,
	); err == nil {
		t.Fatal("the database accepted a quota_bytes on a hashserv backend -- " +
			"cache_backends_hashserv_no_quota did not fire")
	}

	// A bazel backend (any non-hashserv kind) accepts the identical value --
	// proving the CHECK is scoped to `kind = 'hashserv'` and not rejecting every
	// quota outright.
	bazelBackendID := seedBackendKind(t, pool, repository.BackendKindBazel)
	if _, err := pool.Exec(ctx,
		`UPDATE cache_backends SET quota_bytes = 1073741824 WHERE id = $1`, bazelBackendID,
	); err != nil {
		t.Fatalf("a bazel backend refused a quota_bytes it should accept: %v", err)
	}
}

// --- opinionated defaults, seeded by CreateBackend ----------------------------

// TestDownloadsRetainsForeverByDefault pins product decision 2 (spec §1.2):
// downloads is an ARCHIVE, not a cache. Since CreateBackend (query/backends.sql)
// now computes retention_window at INSERT time rather than relying on 000012's
// one-time backfill UPDATE, this exercises CreateBackend's own hard-NULL for
// kind = 'downloads' -- a downloads backend's retention_window must come back
// NULL even though every other kind gets a real default, and even when (see
// TestCreateBackendSeedsRetentionAndQuota) the owning org has set a
// default_retention_window that every OTHER kind would inherit.
func TestDownloadsRetainsForeverByDefault(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	downloadsID := seedBackendKind(t, pool, repository.BackendKindDownloads)

	var window pgtype.Interval
	if err := pool.QueryRow(ctx,
		`SELECT retention_window FROM cache_backends WHERE id = $1`, downloadsID,
	).Scan(&window); err != nil {
		t.Fatalf("read retention_window: %v", err)
	}

	if window.Valid {
		t.Errorf("downloads backend retention_window is set (%+v), want NULL -- "+
			"downloads is an archive and must retain forever by default", window)
	}
}

// TestCreateBackendSeedsRetentionAndQuota pins CreateBackend's own opinionated
// seeding (R6#2/R7#4, query/backends.sql) -- the per-kind ladder, the org-default
// override, and downloads' immunity to that override -- independently of the
// migration's one-time backfill UPDATE, which this test never touches.
func TestCreateBackendSeedsRetentionAndQuota(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	// A fresh bazel backend, no org override: gets the per-kind default (30d).
	bazelID := seedBackendKind(t, pool, repository.BackendKindBazel)

	bazel, err := q.GetBackendByID(ctx, bazelID)
	if err != nil {
		t.Fatalf("GetBackendByID(bazel): %v", err)
	}

	if !bazel.RetentionWindow.Valid || bazel.RetentionWindow.Days != 30 {
		t.Errorf("fresh bazel backend retention_window = %+v, want 30 days", bazel.RetentionWindow)
	}

	// A fresh downloads backend, no org override: NULL, the archive default.
	downloadsID := seedBackendKind(t, pool, repository.BackendKindDownloads)

	downloads, err := q.GetBackendByID(ctx, downloadsID)
	if err != nil {
		t.Fatalf("GetBackendByID(downloads): %v", err)
	}

	if downloads.RetentionWindow.Valid {
		t.Errorf("fresh downloads backend retention_window = %+v, want NULL", downloads.RetentionWindow)
	}

	// An org with default_retention_window set: a NEW sstate backend in a
	// DIFFERENT project of the SAME org must inherit the org default (180d),
	// overriding the 90d per-kind default -- proving the COALESCE actually
	// prefers the org's column over the CASE ladder, not just that a NULL org
	// default falls through to it (every backend above already proves that half).
	org, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: "org-default-override", Name: "Org Default Override",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE organizations SET default_retention_window = interval '180 days' WHERE id = $1`, org.ID,
	); err != nil {
		t.Fatalf("set org default_retention_window: %v", err)
	}

	project, err := q.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	sstate, err := q.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID: project.ID, Kind: repository.BackendKindSstate,
		Enabled: true, ReadAuthRequired: true, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create sstate backend: %v", err)
	}

	if !sstate.RetentionWindow.Valid || sstate.RetentionWindow.Days != 180 {
		t.Errorf("sstate backend under an org default_retention_window of 180d got "+
			"retention_window = %+v, want 180 days -- the org default did not override "+
			"the 90d per-kind default", sstate.RetentionWindow)
	}

	// downloads IGNORES the org default too (spec §1.2: "stays NULL even when an
	// org default exists") -- the SAME org, a SECOND project, a downloads backend.
	project2, err := q.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget-2", Name: "Widget 2",
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}

	downloadsOverridden, err := q.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID: project2.ID, Kind: repository.BackendKindDownloads,
		Enabled: true, ReadAuthRequired: true, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create downloads backend: %v", err)
	}

	if downloadsOverridden.RetentionWindow.Valid {
		t.Errorf("downloads backend under an org default_retention_window of 180d got "+
			"retention_window = %+v, want NULL -- downloads must ignore the org default",
			downloadsOverridden.RetentionWindow)
	}
}

// --- the migration round trip -------------------------------------------------

// TestGCRetentionQuotasMigrationRoundTrips is 000012's own down+up proof, the
// same shape TestHybridRolesMigrationRoundTrips (hybrid_roles_test.go) uses for
// 000008: TestMigrateUpDownUp (db_test.go) already proves EVERY migration's round
// trip leaves zero residual tables/enums/functions, but a migration that ADDS
// COLUMNS to existing tables needs its own proof that those columns come back
// correctly on a second UP, not just that DOWN cleanly removes them.
//
// R6#6: the previous version of this test PRE-ARRANGED ITS OWN ANSWER -- it
// created one backend after the round trip and then hand-ran the exact UPDATE
// 000012's seeding is supposed to perform, so it could not have caught a broken
// migration (it would have "passed" even if the migration's seeding UPDATE were
// deleted entirely). Two changes fix that:
//
//  1. One backend of EVERY kind is seeded on the fully-migrated schema BEFORE the
//     round trip, so MigrateDown's DROP COLUMN / DROP CONSTRAINT statements on
//     cache_backends, and DROP TABLE on cache_backend_usage, run against a
//     POPULATED table of every kind rather than an empty one -- a down migration
//     that only ever gets exercised against zero rows is not proven to survive
//     real data.
//  2. After the round trip, a FRESH backend of every kind is created (via
//     CreateBackend, no hand-run UPDATE anywhere) and its retention_window is
//     asserted against the exact per-kind ladder 000012/CreateBackend both
//     implement: 90d/90d/30d/30d/NULL for sstate/hashserv/bazel/oci/downloads.
func TestGCRetentionQuotasMigrationRoundTrips(t *testing.T) {
	t.Parallel()

	preRoundTrip, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	// One backend of EVERY kind, seeded on the FULLY-MIGRATED schema `dbtest`
	// already handed back (enum types registered, ordinary CreateBackend path --
	// so these rows carry REAL, non-NULL retention_window/quota_bytes values,
	// not just populated columns) -- so MigrateDown's DROP COLUMN / DROP
	// CONSTRAINT statements on cache_backends, and DROP TABLE on
	// cache_backend_usage, run against a table that actually holds data of
	// every kind, exercising every CHECK constraint's removal for real, rather
	// than against an empty table that would let a broken down-migration pass
	// silently.
	for _, kind := range []repository.BackendKind{
		repository.BackendKindSstate, repository.BackendKindDownloads,
		repository.BackendKindHashserv, repository.BackendKindBazel, repository.BackendKindOci,
	} {
		seedBackendKind(t, preRoundTrip, kind)
	}

	// db.NewBootstrapPool, NOT the pool above: MigrateDown takes the enum types
	// with it, so a pool that insisted on registering them (preRoundTrip above)
	// could not open afterwards. preRoundTrip is closed FIRST so its idle
	// connections cannot contend with the ACCESS EXCLUSIVE locks MigrateDown's
	// DROP/ALTER statements need.
	preRoundTrip.Close()

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

	serving, err := db.NewPool(ctx, db.Config{URL: dsn, MaxConns: 4})
	if err != nil {
		t.Fatalf("open serving pool after the round trip: %v", err)
	}

	defer serving.Close()

	q := repository.New(serving)

	// The opinionated per-kind ladder, on FRESH rows created via CreateBackend
	// AFTER the round trip -- no hand-run UPDATE standing in for it anywhere.
	wantDays := map[repository.BackendKind]int32{
		repository.BackendKindSstate:   90,
		repository.BackendKindHashserv: 90,
		repository.BackendKindBazel:    30,
		repository.BackendKindOci:      30,
	}

	for _, kind := range []repository.BackendKind{
		repository.BackendKindSstate, repository.BackendKindHashserv,
		repository.BackendKindBazel, repository.BackendKindOci, repository.BackendKindDownloads,
	} {
		id := seedBackendKind(t, serving, kind)

		backend, err := q.GetBackendByID(ctx, id)
		if err != nil {
			t.Fatalf("GetBackendByID(%s): %v", kind, err)
		}

		want, ok := wantDays[kind]
		if !ok { // downloads
			if backend.RetentionWindow.Valid {
				t.Errorf("%s: retention_window = %+v after the round trip, want NULL -- "+
					"downloads is an archive", kind, backend.RetentionWindow)
			}

			continue
		}

		if !backend.RetentionWindow.Valid || backend.RetentionWindow.Days != want {
			t.Errorf("%s: retention_window = %+v after the round trip, want %d days",
				kind, backend.RetentionWindow, want)
		}
	}

	// The hashserv-no-quota CHECK must survive too.
	hashservID := seedBackendKind(t, serving, repository.BackendKindHashserv)
	if _, err := serving.Exec(ctx,
		`UPDATE cache_backends SET quota_bytes = 1 WHERE id = $1`, hashservID,
	); err == nil {
		t.Error("cache_backends_hashserv_no_quota did not survive the round trip")
	}

	if _, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: "post-roundtrip", Name: "Post Roundtrip",
	}); err != nil {
		t.Fatalf("ordinary write after the round trip failed: %v", err)
	}
}
