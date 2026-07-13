// These tests live in package db_test, not package db.
//
// They have to: they need a real Postgres, the harness that provides one is
// internal/db/dbtest, and dbtest imports internal/db to run the migrations. An
// in-package test file importing dbtest would be an import cycle. This is the one
// place the "same-package tests" convention cannot hold, and it is a language
// constraint rather than a preference.
package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/slug"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// digest builds a 32-byte digest -- blobs.digest CHECKs octet_length = 32.
func digest(b byte) []byte {
	d := make([]byte, 32)
	for i := range d {
		d[i] = b
	}

	return d
}

// --- migrations --------------------------------------------------------------

// TestMigrateUpDownUp is the migration gate: every migration applies, every
// migration rolls back, and the schema comes back clean afterwards.
//
// The down leg is not ceremony. A down migration nobody ever runs is a down
// migration that does not work, and you discover that at the exact moment you need
// to roll production back. Asserting that DOWN leaves ZERO residual tables, types
// and functions is what makes it real -- a DROP TABLE that forgets its trigger
// function, or an enum nobody dropped, would sail through a shallow "did it error"
// check and then blow up on the next UP.
func TestMigrateUpDownUp(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t) // dbtest hands us an already-migrated database
	ctx := t.Context()

	assertSchemaPresent(ctx, t, pool)

	if err := db.MigrateDown(pool); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	for _, c := range []struct {
		what  string
		query string
	}{
		{
			what: "tables",
			query: `SELECT count(*) FROM information_schema.tables
			         WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`,
		},
		{
			what: "enum types",
			query: `SELECT count(*) FROM pg_type t
			          JOIN pg_namespace n ON n.oid = t.typnamespace
			         WHERE n.nspname = 'public' AND t.typtype = 'e'`,
		},
		{
			what: "functions",
			query: `SELECT count(*) FROM pg_proc p
			          JOIN pg_namespace n ON n.oid = p.pronamespace
			         WHERE n.nspname = 'public'`,
		},
	} {
		var n int
		if err := pool.QueryRow(ctx, c.query).Scan(&n); err != nil {
			t.Fatalf("count residual %s: %v", c.what, err)
		}

		if n != 0 {
			t.Errorf("after down, %d %s survive, want 0", n, c.what)
		}
	}

	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}

	assertSchemaPresent(ctx, t, pool)
}

// assertSchemaPresent spot-checks one relation per migration, so a migration that
// silently stopped applying cannot pass.
func assertSchemaPresent(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	for _, rel := range []string{
		"users", "organizations", "projects", "org_memberships", "project_memberships",
		"cache_backends", "api_keys", "sessions", "blobs", "cache_objects", "gc_runs",
	} {
		var got pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, rel).Scan(&got); err != nil {
			t.Fatalf("to_regclass(%s): %v", rel, err)
		}

		if !got.Valid || got.String != rel {
			t.Errorf("relation %q is missing after migrate up", rel)
		}
	}

	// The helper functions and the refcount trigger are as load-bearing as the
	// tables, and a DROP FUNCTION in a down file would take them out silently.
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT bakery_slug_ok('acme')`).Scan(&ok); err != nil {
		t.Fatalf("bakery_slug_ok is missing: %v", err)
	}

	var lockKey int64
	if err := pool.QueryRow(ctx, `SELECT bakery_blob_lock_key($1)`, digest(0x01)).Scan(&lockKey); err != nil {
		t.Fatalf("bakery_blob_lock_key is missing: %v", err)
	}

	var triggers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger WHERE tgname = 'cache_objects_refcount_aiud'`,
	).Scan(&triggers); err != nil {
		t.Fatalf("count refcount trigger: %v", err)
	}

	if triggers != 1 {
		t.Errorf("cache_objects_refcount_aiud trigger count = %d, want 1", triggers)
	}
}

func TestMigrationVersion(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)

	version, dirty, applied, err := db.MigrationVersion(pool)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}

	if !applied {
		t.Fatal("applied = false on a migrated database")
	}

	if dirty {
		t.Error("dirty = true on a cleanly migrated database")
	}

	// 9 up/down pairs ship in internal/db/migrations. If this number changes, the
	// change was deliberate and this line moves with it.
	if version != 9 {
		t.Errorf("version = %d, want 9", version)
	}
}

// --- boot advisory lock ------------------------------------------------------

// TestBootLockRefusesASecondInstance is the invariant: boot takes
// pg_try_advisory_lock and REFUSES to start a second instance unless
// --allow-multi-instance.
//
// The whole in-process route cache is sound only because of this. A second writer
// would invalidate its own LRU on a control-plane write and leave the other
// instance serving a stale route indefinitely.
func TestBootLockRefusesASecondInstance(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	first, err := db.AcquireBootLock(ctx, pool)
	if err != nil {
		t.Fatalf("first instance could not take the boot lock: %v", err)
	}

	// A SECOND POOL, i.e. a second set of sessions -- what a second process
	// actually looks like. Taking the second attempt on the same pool would prove
	// nothing about a second bakery.
	second, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}

	defer second.Close()

	_, err = db.AcquireBootLock(ctx, second)
	if !errors.Is(err, db.ErrLocked) {
		t.Fatalf("second instance got err = %v, want db.ErrLocked -- it booted anyway", err)
	}

	// After the first instance shuts down, the lock is free again. Without this
	// leg, a Release that silently did nothing would still pass the test above.
	first.Release()

	third, err := db.AcquireBootLock(ctx, second)
	if err != nil {
		t.Fatalf("after the first instance released, a new one still could not boot: %v", err)
	}

	third.Release()
}

// TestBootLockSurvivesPoolChurn is the reason the lock pins a DEDICATED
// connection.
//
// Advisory locks taken by pg_try_advisory_lock are SESSION-scoped. Take one via
// pool.QueryRow and the connection goes straight back to the pool; when pgxpool
// later recycles it, the lock releases SILENTLY and a second instance boots while
// every log line still says the lock is held. Here the pool is driven hard enough
// to hand out and return every other connection, and the lock must survive.
func TestBootLockSurvivesPoolChurn(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	lock, err := db.AcquireBootLock(ctx, pool)
	if err != nil {
		t.Fatalf("AcquireBootLock: %v", err)
	}

	defer lock.Release()

	for range 50 {
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("churn query: %v", err)
		}
	}

	second, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}

	defer second.Close()

	if _, err := db.AcquireBootLock(ctx, second); !errors.Is(err, db.ErrLocked) {
		t.Fatalf("after pool churn the boot lock was no longer held: err = %v, want db.ErrLocked", err)
	}
}

// TestBootLockDoesNotCollideWithBlobLocks proves the two advisory key spaces are
// disjoint.
//
// bakery_blob_lock_key(digest) is a SINGLE bigint. The boot lock is a TWO-int4
// key. PostgreSQL keeps those spaces strictly separate -- and it has to, because
// the boot lock is held for the entire process lifetime: a digest that collided
// with it would wedge that one blob's PUT forever, and only that one.
func TestBootLockDoesNotCollideWithBlobLocks(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	lock, err := db.AcquireBootLock(ctx, pool)
	if err != nil {
		t.Fatalf("AcquireBootLock: %v", err)
	}

	defer lock.Release()

	// The single-bigint key that has the same bit pattern as the boot lock's two
	// int4s concatenated: 0x42414B45_00005259. If the spaces overlapped, this is
	// exactly the digest lock that would deadlock against boot.
	const collidingKey int64 = 0x42414B4500005259

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, collidingKey).Scan(&got); err != nil {
		t.Fatalf("pg_try_advisory_lock(bigint): %v", err)
	}

	if !got {
		t.Error("a single-bigint advisory lock collided with the two-int4 boot lock -- " +
			"the key spaces are not separate and a blob PUT can wedge forever")
	}
}

// --- the Go/SQL slug mirror --------------------------------------------------

// TestSlugMirrorsDatabase proves internal/slug and bakery_slug_ok agree.
//
// The database is the AUTHORITY: the CHECK on organizations.slug and projects.slug
// holds for every writer -- the API, the dev seeder, a migration, a psql session.
// internal/slug exists only so the API can render a friendly 400 instead of
// surfacing a 23514. A drift between the two turns that friendly 400 into a lie,
// so it has to be a failing test rather than a production surprise.
//
// The denylist leg drives slug.Reserved() itself through the SQL function rather
// than a copied literal, so a word added to one and not the other fails here.
func TestSlugMirrorsDatabase(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	sqlSaysValid := func(s string) bool {
		t.Helper()

		var ok pgtype.Bool
		if err := pool.QueryRow(ctx, `SELECT bakery_slug_ok($1)`, s).Scan(&ok); err != nil {
			t.Fatalf("bakery_slug_ok(%q): %v", s, err)
		}

		// STRICT, so a NULL in means a NULL out. Nothing in the schema can hold a
		// NULL slug (both columns are NOT NULL), so treat it as invalid.
		return ok.Valid && ok.Bool
	}

	grammar := []struct {
		name string
		slug string
	}{
		{name: "simple", slug: "acme"},
		{name: "hyphenated", slug: "acme-widgets"},
		{name: "leading digit", slug: "2acme"},
		{name: "single character", slug: "a"},
		{name: "63 characters", slug: "a12345678901234567890123456789012345678901234567890123456789012"},
		{name: "64 characters", slug: "a123456789012345678901234567890123456789012345678901234567890123"},
		{name: "empty", slug: ""},
		{name: "uppercase", slug: "Acme"},
		{name: "camelCase actionResults", slug: "actionResults"},
		{name: "leading hyphen", slug: "-acme"},
		{name: "trailing hyphen", slug: "acme-"},
		{name: "underscore", slug: "acme_widgets"},
		{name: "slash", slug: "acme/widgets"},
		{name: "dot", slug: "acme.widgets"},
		{name: "space", slug: "acme widgets"},
		{name: "reserved word as a prefix", slug: "cache-server"},
	}

	for _, tt := range grammar {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := slug.Valid(tt.slug), sqlSaysValid(tt.slug); got != want {
				t.Errorf("slug.Valid(%q) = %v, but bakery_slug_ok(%q) = %v", tt.slug, got, tt.slug, want)
			}
		})
	}

	for _, word := range slug.Reserved() {
		t.Run("reserved/"+word, func(t *testing.T) {
			if sqlSaysValid(word) {
				t.Errorf("slug.Reserved() carries %q but bakery_slug_ok(%q) accepts it -- "+
					"the Go denylist and the migration have drifted", word, word)
			}
		})
	}

	// And the reverse direction: the database must not reserve a word Go has
	// forgotten. A slug that is well-formed but refused by SQL and accepted by Go
	// is the drift that produces a 500 instead of a 400.
	t.Run("no word is reserved in SQL only", func(t *testing.T) {
		for _, word := range []string{
			"blobs", "uploads", "actions", "actionresults", "operations",
			"capabilities", "compressed-blobs", "ac", "cas", "v2", "api", "cache",
		} {
			if !slug.IsReserved(word) {
				t.Errorf("bakery_slug_ok reserves %q but slug.IsReserved(%q) is false", word, word)
			}
		}
	})
}

// TestSlugCheckIsEnforcedByTheDatabase proves the CHECK is really on the columns,
// not just on a function nobody wired up.
func TestSlugCheckIsEnforcedByTheDatabase(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	for _, bad := range []string{"cache", "Acme", "acme_widgets", ""} {
		_, err := pool.Exec(ctx,
			`INSERT INTO organizations (slug, name) VALUES ($1, 'x')`, bad)
		if err == nil {
			t.Errorf("organizations accepted the slug %q", bad)
		}
	}
}

// --- the GC write barrier ----------------------------------------------------

// TestGCWriteBarrierSparesAConcurrentBuild is the reproduction of the bug the
// barrier exists to prevent, run as a test.
//
// CLAUDE.md states the barrier as `created_at < gc_run.started_at`, and AS WRITTEN
// THAT IS NOT SUFFICIENT. now() is TRANSACTION-START time, so a build that BEGINs
// before a GC run starts and COMMITs after it produces a row whose created_at
// predates gc_runs.started_at while being invisible to the GC's snapshot. The
// timestamp barrier says "sweep it" and the GC deletes the bytes of a build that
// is still running.
//
// pg_visible_in_snapshot(live_xid, gc_runs.snapshot) is the form that actually
// holds, and the sweep predicate ANDs both. This test sets up exactly that
// interleaving and asserts the blob is spared -- and then asserts a LATER run,
// with a fresh snapshot, does sweep it, so "spares everything forever" cannot pass
// by accident.
func TestGCWriteBarrierSparesAConcurrentBuild(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	d := digest(0xD1)

	// The "build": a transaction that starts BEFORE the GC run and commits AFTER
	// it. Its now() -- and therefore its created_at and unreferenced_since -- is
	// stamped here, at BEGIN.
	build, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin build: %v", err)
	}

	defer func() { _ = build.Rollback(ctx) }()

	if _, err := build.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(7),
	); err != nil {
		t.Fatalf("build insert: %v", err)
	}

	// Make sure the GC run's started_at is strictly after the build's now().
	time.Sleep(10 * time.Millisecond)

	// The GC run: started_at and snapshot are frozen here, and the build's xid is
	// in flight and therefore NOT visible in that snapshot.
	run, err := q.StartGCRun(ctx, pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true})
	if err != nil {
		t.Fatalf("StartGCRun: %v", err)
	}

	if err := build.Commit(ctx); err != nil {
		t.Fatalf("build commit: %v", err)
	}

	// The timestamp barrier alone would sweep this row. Prove that, so the test
	// below is a real result and not a vacuous one.
	var tsBarrierWouldSweep bool

	err = pool.QueryRow(ctx, `
		SELECT b.created_at < g.started_at
		  FROM blobs b, gc_runs g
		 WHERE b.digest = $1 AND g.id = $2`, d, run.ID,
	).Scan(&tsBarrierWouldSweep)
	if err != nil {
		t.Fatalf("evaluate timestamp barrier: %v", err)
	}

	if !tsBarrierWouldSweep {
		t.Fatal("the timestamp barrier did not even select this row -- " +
			"the interleaving this test needs did not happen, so it proves nothing")
	}

	marked, err := q.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{
		ID: run.ID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("MarkBlobsPendingDelete: %v", err)
	}

	if len(marked) != 0 {
		t.Fatalf("the GC marked %d blobs of a still-running build for deletion -- "+
			"the snapshot write barrier is not holding", len(marked))
	}

	// Finish the first run. gc_runs_single_active_idx allows exactly one 'running'
	// row, so this is not bookkeeping -- the next StartGCRun is a unique violation
	// without it.
	if err := q.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID:             run.ID,
		Status:         repository.GcRunStatusSucceeded,
		Error:          pgtype.Text{String: "", Valid: false},
		ObjectsDeleted: 0,
		BlobsMarked:    0,
		BlobsDeleted:   0,
		BytesReclaimed: 0,
	}); err != nil {
		t.Fatalf("FinishGCRun: %v", err)
	}

	// A LATER run takes a fresh snapshot in which the build's xid IS visible, so
	// the same blob is now legitimately sweepable. Without this leg, a barrier that
	// spares everything forever would pass.
	next, err := q.StartGCRun(ctx, pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true})
	if err != nil {
		t.Fatalf("StartGCRun (second): %v", err)
	}

	marked, err = q.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{
		ID: next.ID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("MarkBlobsPendingDelete (second): %v", err)
	}

	if len(marked) != 1 {
		t.Fatalf("the second GC run marked %d blobs, want 1 -- the barrier never converges "+
			"and unreferenced bytes would leak forever", len(marked))
	}
}

// TestOnlyOneGCRunAtATime: two concurrent sweeps would each hold a snapshot and
// each treat the other's in-flight writes as sweepable -- mark-sweep with a live
// mutator, where the mutator is the other GC. gc_runs_single_active_idx makes that
// a unique violation rather than a race.
func TestOnlyOneGCRunAtATime(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	grace := pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true}

	if _, err := q.StartGCRun(ctx, grace); err != nil {
		t.Fatalf("first StartGCRun: %v", err)
	}

	if _, err := q.StartGCRun(ctx, grace); err == nil {
		t.Fatal("a second concurrent GC run was allowed to start")
	}
}

// --- the refcount trigger ----------------------------------------------------

// TestRefcountTriggerOwnsTheArithmetic. There is no increment query and no
// decrement query anywhere in internal/db/query, and that is deliberate: a refcount
// is a materialized aggregate over cache_objects, and the only way to keep one
// honest under a mutating population is to derive it FROM the mutation.
//
// The /ac/ overwrite is the decider -- the one MUTABLE namespace, where a PUT must
// decrement the old blob and increment the new one atomically. That is the easiest
// refcount bug to write in Go, it leaks bytes silently, and it only shows up on the
// ccache path.
func TestRefcountTriggerOwnsTheArithmetic(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	backendID := seedBackend(t, pool)

	blobA, blobB := digest(0xA1), digest(0xB2)
	for _, d := range [][]byte{blobA, blobB} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(10),
		); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
	}

	refcount := func(d []byte) int64 {
		t.Helper()

		var n int64
		if err := pool.QueryRow(ctx, `SELECT refcount FROM blobs WHERE digest = $1`, d).Scan(&n); err != nil {
			t.Fatalf("read refcount: %v", err)
		}

		return n
	}

	unreferenced := func(d []byte) bool {
		t.Helper()

		var ts pgtype.Timestamptz
		if err := pool.QueryRow(ctx,
			`SELECT unreferenced_since FROM blobs WHERE digest = $1`, d,
		).Scan(&ts); err != nil {
			t.Fatalf("read unreferenced_since: %v", err)
		}

		return ts.Valid
	}

	q := repository.New(pool)

	// INSERT -> +1 on A, and unreferenced_since cleared.
	if _, err := q.PutObjectImmutable(ctx, repository.PutObjectImmutableParams{
		BackendID: backendID, Namespace: "ac", Key: "k", Digest: blobA, SizeBytes: 10,
	}); err != nil {
		t.Fatalf("PutObjectImmutable: %v", err)
	}

	if got := refcount(blobA); got != 1 {
		t.Errorf("after insert, A refcount = %d, want 1", got)
	}

	if unreferenced(blobA) {
		t.Error("after insert, A is still stamped unreferenced_since -- the GC would sweep a live blob")
	}

	// The /ac/ OVERWRITE: repoint the same key at B. A must go to 0 AND be stamped
	// unreferenced; B must go to 1. A blob whose refcount hits 0 without the stamp
	// is invisible to the GC forever and its bytes leak silently.
	if _, err := q.PutObjectOverwritable(ctx, repository.PutObjectOverwritableParams{
		BackendID: backendID, Namespace: "ac", Key: "k", Digest: blobB, SizeBytes: 10,
	}); err != nil {
		t.Fatalf("PutObjectOverwritable: %v", err)
	}

	if got := refcount(blobA); got != 0 {
		t.Errorf("after /ac/ overwrite, A refcount = %d, want 0 -- the old blob's bytes leak", got)
	}

	if !unreferenced(blobA) {
		t.Error("after /ac/ overwrite, A is not stamped unreferenced_since -- the GC will never see it")
	}

	if got := refcount(blobB); got != 1 {
		t.Errorf("after /ac/ overwrite, B refcount = %d, want 1", got)
	}

	// DELETE -> -1. [METADATA FIRST.] The bytes are never touched here.
	if _, err := q.DeleteObject(ctx, repository.DeleteObjectParams{
		BackendID: backendID, Namespace: "ac", Key: "k",
	}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if got := refcount(blobB); got != 0 {
		t.Errorf("after delete, B refcount = %d, want 0", got)
	}

	if !unreferenced(blobB) {
		t.Error("after delete, B is not stamped unreferenced_since")
	}
}

// TestDanglingMetadataIsImpossible. The invariant says dangling metadata is a
// permanent 500 while orphaned bytes are merely wasteful. cache_objects_blob_fk is
// ON DELETE RESTRICT, so the database REFUSES to delete a blob any object still
// names -- no matter what the refcount says, and no matter which code path asks.
// The refcount is the fast path; this FK is the truth, and refcount drift becomes a
// loud foreign-key violation rather than a silent corruption.
func TestDanglingMetadataIsImpossible(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	backendID := seedBackend(t, pool)
	d := digest(0xE5)

	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(3),
	); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
		 VALUES ($1, '', 'sstate:thing.tar.zst', $2, 3)`, backendID, d,
	); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM blobs WHERE digest = $1`, d); err == nil {
		t.Fatal("the database deleted a blob that a cache_object still names -- " +
			"dangling metadata is representable, and it is a permanent 500")
	}
}

// TestEmptyBlobIsStorable. The REAPI empty blob (sha256 of nothing, size 0) MUST
// always report as present. A CHECK (size_bytes > 0) is the reflex, and it breaks
// every Bazel client at once.
func TestEmptyBlobIsStorable(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, 0)`, digest(0xE3),
	); err != nil {
		t.Fatalf("the zero-length blob is not storable: %v", err)
	}
}

// TestACAndCASKeysCoexist. A bazel backend serves TWO key spaces, /ac/ and /cas/,
// and BOTH are 64 hex characters. Without `namespace` in the primary key they
// collide -- and /ac/ is overwritable and UNVERIFIED while /cas/ is immutable and
// digest-VERIFIED, so a ccache write to /ac/<h> would silently repoint the CAS blob
// at <h> at unrelated content.
func TestACAndCASKeysCoexist(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()

	backendID := seedBackend(t, pool)

	const sameKey = "b1946ac92492d2347c6235b4d2611184b1946ac92492d2347c6235b4d2611184"

	for i, ns := range []string{"ac", "cas"} {
		d := digest(byte(0x40 + i))

		if _, err := pool.Exec(ctx,
			`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(5),
		); err != nil {
			t.Fatalf("seed blob: %v", err)
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
			 VALUES ($1, $2, $3, $4, 5)`, backendID, ns, sameKey, d,
		); err != nil {
			t.Fatalf("insert %s/%s: %v", ns, sameKey, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM cache_objects WHERE key = $1`, sameKey,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	if n != 2 {
		t.Errorf("count = %d, want 2 -- /ac/ and /cas/ collided on one key", n)
	}
}

// --- Store.Tx ----------------------------------------------------------------

// TestStoreTxRebindsQueriesOntoTheTransaction. The failure this guards against is
// silent: bind Queries to the POOL, open a pgx.Tx alongside it, and every write in
// the "transaction" executes outside it. The transaction opens, does nothing, and
// commits -- and the FOR UPDATE the refcount protocol depends on locks nothing.
func TestStoreTxRebindsQueriesOntoTheTransaction(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	store := db.NewStore(pool)

	sentinel := errors.New("roll it back")

	err := store.Tx(ctx, func(q *repository.Queries) error {
		if _, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
			Slug: "rolled-back", Name: "Rolled Back",
		}); err != nil {
			return err
		}

		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx returned %v, want the closure's error", err)
	}

	if _, err := store.GetOrganizationBySlug(ctx, "rolled-back"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the row survived a rolled-back transaction (err = %v) -- "+
			"the queries ran on the pool, not the tx", err)
	}

	// And the happy path really does commit.
	err = store.Tx(ctx, func(q *repository.Queries) error {
		_, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
			Slug: "committed", Name: "Committed",
		})

		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if _, err := store.GetOrganizationBySlug(ctx, "committed"); err != nil {
		t.Fatalf("the row did not survive a committed transaction: %v", err)
	}
}

// --- the refcount FOR UPDATE / recheck interlock -----------------------------

// zeroGrace is the grace period the mark tests use: none, so a blob is sweepable
// the instant it is unreferenced and the GC is as adversarial as it can be.
func zeroGrace() pgtype.Interval {
	return pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true}
}

// TestGetBlobForWriteLockBlocksTheGCMark is the missing guard the M1 refcount-safety
// invariant names explicitly: FOR UPDATE in GetBlobForWrite.
//
// The interlock: a PUT that has run GetBlobForWrite holds `FOR UPDATE` on the blob
// row, and the GC's mark is `FOR UPDATE OF b SKIP LOCKED`, so the row a live PUT has
// locked is SKIPPED and never tombstoned under it. Delete FOR UPDATE from
// GetBlobForWrite and the PUT locks nothing, the mark no longer skips it, and the GC
// tombstones a blob a committed object is about to reference -- silently, with every
// other test still green. This is the test that goes red.
func TestGetBlobForWriteLockBlocksTheGCMark(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	d := digest(0xF0)

	// A committed, sweepable blob: no objects (refcount 0, therefore stamped
	// unreferenced), old enough that the grace and the write barrier both pass.
	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(9),
	); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	time.Sleep(10 * time.Millisecond) // created_at strictly before the GC run's started_at

	// tx A takes the PUT-side lock on the blob row and HOLDS the transaction open,
	// exactly as blob.Service.Put does between GetBlobForWrite and COMMIT.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx A: %v", err)
	}

	defer func() { _ = txA.Rollback(ctx) }()

	if _, err := repository.New(txA).GetBlobForWrite(ctx, d); err != nil {
		t.Fatalf("GetBlobForWrite: %v", err)
	}

	// The GC marks. FOR UPDATE OF b SKIP LOCKED must SKIP the row tx A holds.
	run, err := q.StartGCRun(ctx, zeroGrace())
	if err != nil {
		t.Fatalf("StartGCRun: %v", err)
	}

	marked, err := q.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{ID: run.ID, Limit: 100})
	if err != nil {
		t.Fatalf("MarkBlobsPendingDelete: %v", err)
	}

	if len(marked) != 0 {
		t.Fatalf("the GC marked %d blobs while a PUT held GetBlobForWrite's FOR UPDATE lock, want 0 -- "+
			"the SKIP LOCKED interlock is gone (FOR UPDATE removed from GetBlobForWrite?)", len(marked))
	}

	// NOT VACUOUS: once the PUT's transaction ends, the very same blob IS markable,
	// so the zero above was the lock and not an unsweepable row.
	if err := txA.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx A: %v", err)
	}

	if err := q.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID:             run.ID,
		Status:         repository.GcRunStatusSucceeded,
		Error:          pgtype.Text{String: "", Valid: false},
		ObjectsDeleted: 0,
		BlobsMarked:    0,
		BlobsDeleted:   0,
		BytesReclaimed: 0,
	}); err != nil {
		t.Fatalf("FinishGCRun: %v", err)
	}

	next, err := q.StartGCRun(ctx, zeroGrace())
	if err != nil {
		t.Fatalf("StartGCRun (second): %v", err)
	}

	marked, err = q.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{ID: next.ID, Limit: 100})
	if err != nil {
		t.Fatalf("MarkBlobsPendingDelete (second): %v", err)
	}

	if len(marked) != 1 {
		t.Fatalf("after the PUT released, the GC marked %d blobs, want 1 -- the row was never sweepable "+
			"and this test proves nothing", len(marked))
	}
}

// TestGetBlobForPhysicalDeleteRechecksRevival is the recheck side of the invariant:
// the physical-delete SELECT must return ZERO rows for a blob a concurrent PUT
// revived while the GC queued for the digest lock, so storage.Delete never runs.
//
// A real revive flips BOTH the state ('pending_delete' -> 'live') and the refcount
// (0 -> 1), so gutting the recheck guard entirely (SELECT ... WHERE digest = $1) is
// what turns this red. See TestPendingDeleteImpliesRefcountZero for the constraint
// that makes the refcount = 0 half of the guard load-bearing.
func TestGetBlobForPhysicalDeleteRechecksRevival(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)
	backendID := seedBackend(t, pool)

	// A blob genuinely queued for deletion: pending_delete, refcount 0. Reapable.
	reapable := digest(0xF1)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes, state, delete_started_at)
		 VALUES ($1, $2, 'pending_delete', now())`, reapable, int64(4),
	); err != nil {
		t.Fatalf("seed pending_delete blob: %v", err)
	}

	if _, err := q.GetBlobForPhysicalDelete(ctx, reapable); err != nil {
		t.Fatalf("GetBlobForPhysicalDelete on a pending_delete refcount=0 blob: err = %v, want it reapable", err)
	}

	// A revived blob: a PUT flips state back to 'live' (CreateOrReviveBlob) BEFORE the
	// object insert bumps refcount to 1 -- that order is mandatory, or
	// blobs_pending_delete_is_unreferenced rejects the bump.
	revived := digest(0xF2)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes, state, delete_started_at)
		 VALUES ($1, $2, 'pending_delete', now())`, revived, int64(4),
	); err != nil {
		t.Fatalf("seed pending_delete blob: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET state = 'live', delete_started_at = NULL WHERE digest = $1`, revived,
	); err != nil {
		t.Fatalf("revive blob (state -> live): %v", err)
	}

	if _, err := q.PutObjectImmutable(ctx, repository.PutObjectImmutableParams{
		BackendID: backendID, Namespace: "", Key: "revived-key", Digest: revived, SizeBytes: 4,
	}); err != nil {
		t.Fatalf("PutObjectImmutable (revive): %v", err)
	}

	if _, err := q.GetBlobForPhysicalDelete(ctx, revived); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetBlobForPhysicalDelete on a revived blob: err = %v, want pgx.ErrNoRows -- "+
			"the recheck would let the GC unlink bytes a live object names", err)
	}
}

// TestPendingDeleteImpliesRefcountZero pins the constraint that makes the
// `refcount = 0` clause in GetBlobForPhysicalDelete's recheck sound: a blob can never
// be simultaneously pending_delete AND referenced. The two clauses of the recheck
// (state = 'pending_delete' AND refcount = 0) are equivalent TODAY only because of
// this CHECK; drop the constraint and refcount = 0 stops being redundant and becomes
// the only thing standing between the GC and an unlink of referenced bytes.
func TestPendingDeleteImpliesRefcountZero(t *testing.T) {
	t.Parallel()

	pool := dbtest.New(t)
	ctx := t.Context()
	backendID := seedBackend(t, pool)
	d := digest(0xF3)

	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(6),
	); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	// One reference: the trigger raises refcount to 1 and clears unreferenced_since.
	if _, err := pool.Exec(ctx,
		`INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
		 VALUES ($1, '', 'k', $2, 6)`, backendID, d,
	); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	// Tombstoning a still-referenced blob must be refused by
	// blobs_pending_delete_is_unreferenced.
	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET state = 'pending_delete', delete_started_at = now() WHERE digest = $1`, d,
	); err == nil {
		t.Fatal("the database let a referenced blob (refcount 1) enter pending_delete -- " +
			"GetBlobForPhysicalDelete's refcount = 0 recheck rests on this being impossible")
	}
}

// --- boot advisory lock: surviving a lost session ----------------------------

// boot-lock advisory key, as the two int4s appear in pg_locks (classid, objid) for a
// pg_advisory_lock(int4, int4): objsubid is 2 for the two-int4 form. These mirror the
// unexported bootLockClassID / bootLockObjID.
const (
	bootLockTestClassID = 0x42414B45 // "BAKE"
	bootLockTestObjID   = 0x5259     // "RY"
)

// bootLockHolderPID returns the pid of the backend that currently holds the granted
// boot lock, if any.
func bootLockHolderPID(t *testing.T, pool *pgxpool.Pool) (int32, bool) {
	t.Helper()

	var pid int32

	// THE DATABASE FILTER IS LOAD-BEARING. Advisory locks are scoped to a database
	// (the lock tag carries the database OID), but pg_locks is CLUSTER-WIDE and shows
	// every database's locks. dbtest hands each test its own database on a SHARED
	// server -- which is precisely what CI does with TEST_DB_URL -- so every boot-lock
	// test holds this same (classid, objid) concurrently. Without the filter this
	// LIMIT 1 returns an arbitrary one of them, and the caller then
	// pg_terminate_backend()s ANOTHER TEST'S lock session: that test's boot lock is
	// released early, its queued competitor is granted the lock instantly instead of
	// waiting, and it fails with "condition not satisfied" nowhere near the real cause.
	err := pool.QueryRow(t.Context(),
		`SELECT pid FROM pg_locks
		  WHERE locktype = 'advisory' AND classid = $1::oid AND objid = $2::oid
		    AND objsubid = 2 AND granted
		    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  LIMIT 1`, bootLockTestClassID, bootLockTestObjID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}

	if err != nil {
		t.Fatalf("query boot lock holder pid: %v", err)
	}

	return pid, true
}

// bootLockHasWaiter reports whether a session is queued (blocked) on the boot lock.
func bootLockHasWaiter(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()

	// Scoped to THIS test's database, for the reason spelled out on bootLockHolderPID:
	// pg_locks is cluster-wide, and an unfiltered count here reports a waiter queued
	// on a DIFFERENT test's boot lock as if it were our own.
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND classid = $1::oid AND objid = $2::oid
		    AND objsubid = 2 AND NOT granted
		    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		bootLockTestClassID, bootLockTestObjID).Scan(&n); err != nil {
		t.Fatalf("query boot lock waiters: %v", err)
	}

	return n > 0
}

// terminateBackend kills one Postgres backend -- the sharp edge of a restart or
// failover, and the exact event that silently drops a session-scoped advisory lock.
func terminateBackend(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()

	if _, err := pool.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}
}

// waitUntil polls cond until it holds or within elapses.
func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("condition not satisfied within %v", within)
}

// TestBootLockReacquiresAfterSessionDeath is the reproduction of the reported bug,
// run as a test: a routine Postgres restart / failover (here, terminating the exact
// backend that holds the lock) drops the session-scoped advisory lock, and the
// running instance keeps serving unlocked -- so a SECOND instance boots alongside it.
//
// The watcher must notice the dead session and RE-ACQUIRE the lock on a fresh
// connection, so the single-instance invariant survives the restart. Without the
// watcher the lock is simply gone and the second AcquireBootLock below succeeds.
func TestBootLockReacquiresAfterSessionDeath(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	lock, err := db.AcquireBootLock(ctx, pool, db.WithWatchInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("AcquireBootLock: %v", err)
	}

	defer lock.Release()

	oldPID, ok := bootLockHolderPID(t, pool)
	if !ok {
		t.Fatal("no backend holds the boot lock right after acquiring it")
	}

	terminateBackend(t, pool, oldPID)

	// The lock must reappear, held by a DIFFERENT backend -- our watcher's fresh
	// session, not the dead one.
	waitUntil(t, 5*time.Second, func() bool {
		pid, ok := bootLockHolderPID(t, pool)

		return ok && pid != oldPID
	})

	// The invariant: a second instance still cannot boot, because we reclaimed it.
	second, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}

	defer second.Close()

	if _, err := db.AcquireBootLock(ctx, second); !errors.Is(err, db.ErrLocked) {
		t.Fatalf("after the boot lock's session died a second instance booted: err = %v, want db.ErrLocked", err)
	}

	// The watcher recovered; it did not surrender.
	select {
	case <-lock.Lost():
		t.Fatal("boot lock signalled irrecoverable loss when it should have re-acquired")
	default:
	}
}

// TestBootLockSignalsLossWhenStolen covers the narrower failure the recovery path
// cannot fix: our session dies AND another instance takes the lock during the outage.
// The watcher cannot re-acquire, and it must SIGNAL LOSS on Lost() so the server can
// shut down instead of serving on as a phantom second writer.
func TestBootLockSignalsLossWhenStolen(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	ctx := t.Context()

	// A DELIBERATELY SLOW watch interval. The competitor below queues for the lock
	// with a BLOCKING acquire, so PostgreSQL grants it the lock the instant our
	// session dies -- within a millisecond. A short interval would let our watcher's
	// own non-blocking re-acquire race that grant for the momentarily-free lock (and
	// barge ahead of the queue); a slow interval guarantees the queued competitor is
	// already the holder by the time the watcher next looks, making the steal
	// deterministic rather than a coin flip.
	lock, err := db.AcquireBootLock(ctx, pool, db.WithWatchInterval(2*time.Second))
	if err != nil {
		t.Fatalf("AcquireBootLock: %v", err)
	}

	defer lock.Release()

	// A competitor -- a second instance -- QUEUES for the boot lock with a blocking
	// acquire. It cannot get it yet (we hold it), so it waits in the lock's queue.
	competitor, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open competitor pool: %v", err)
	}

	cconn, err := competitor.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire competitor conn: %v", err)
	}

	// The competitor's acquire blocks until it is granted the stolen lock (or the
	// test tears down). It gets its OWN context so cleanup can unblock it, and we
	// wait for the goroutine to finish touching cconn BEFORE releasing it -- otherwise
	// a failing assertion would race the deferred Release against the live Exec.
	compCtx, compCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = cconn.Exec(compCtx, `SELECT pg_advisory_lock($1, $2)`,
			int32(bootLockTestClassID), int32(bootLockTestObjID))
	}()

	t.Cleanup(func() {
		compCancel()
		<-done
		cconn.Release()
		competitor.Close()
	})

	// Wait for the competitor to be queued behind our lock.
	waitUntil(t, 20*time.Second, func() bool { return bootLockHasWaiter(t, pool) })

	holderPID, ok := bootLockHolderPID(t, pool)
	if !ok {
		t.Fatal("no backend holds the boot lock")
	}

	// Kill our session. The queued competitor is granted the lock, and our watcher's
	// next non-blocking re-acquire therefore finds it held by someone else.
	terminateBackend(t, pool, holderPID)

	select {
	case <-lock.Lost():
	case <-time.After(30 * time.Second):
		t.Fatal("boot lock did not signal loss after another instance stole it")
	}
}

// seedBackend creates the org -> project -> backend chain a cache_object needs.
func seedBackend(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()

	ctx := t.Context()
	q := repository.New(pool)

	org, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: "acme", Name: "Acme",
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
		Kind:             repository.BackendKindBazel,
		Enabled:          true,
		ReadAuthRequired: true,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}

	return backend.ID
}
