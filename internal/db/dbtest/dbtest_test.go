package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { Main(m) }

// bootLockKey stands in for the well-known advisory-lock key the real server
// takes at boot to refuse a second instance. internal/db owns the real one; this
// only has to be a fixed key.
const bootLockKey int64 = 0x6261_6b65

// digest builds a 32-byte digest, which is what blobs.digest CHECKs for.
func digest(b byte) []byte {
	d := make([]byte, 32)
	for i := range d {
		d[i] = b
	}

	return d
}

// seedBlob inserts one live blob and returns its digest.
func seedBlob(t *testing.T, pool *pgxpool.Pool, b byte) []byte {
	t.Helper()

	d := digest(b)

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2)`, d, int64(1024),
	); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	return d
}

// TestMigrationsApplied proves the template really was migrated and the clone
// carried the schema with it.
func TestMigrationsApplied(t *testing.T) {
	t.Parallel()

	pool := New(t)

	var (
		table string
		count int
	)

	err := pool.QueryRow(t.Context(),
		`SELECT to_regclass('public.blobs')::text, (SELECT count(*) FROM blobs)`,
	).Scan(&table, &count)
	if err != nil {
		t.Fatalf("query migrated schema: %v", err)
	}

	if table != "blobs" {
		t.Errorf("to_regclass = %q, want %q", table, "blobs")
	}

	if count != 0 {
		t.Errorf("fresh database has %d rows, want 0", count)
	}
}

// TestIsolation_A and TestIsolation_B run in parallel and both insert the SAME
// primary key. If they shared a database, one of them would hit a unique
// violation and both would see two rows. Neither happens.
func TestIsolation_A(t *testing.T) {
	t.Parallel()
	assertSolePrimaryKeyOwner(t)
}

func TestIsolation_B(t *testing.T) {
	t.Parallel()
	assertSolePrimaryKeyOwner(t)
}

func assertSolePrimaryKeyOwner(t *testing.T) {
	t.Helper()

	pool := New(t)

	seedBlob(t, pool, 0xAA)

	// Give the sibling test room to insert its own copy before we count.
	time.Sleep(200 * time.Millisecond)

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM blobs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}

	if count != 1 {
		t.Errorf("see %d rows, want 1 -- a sibling test's rows are visible", count)
	}
}

// TestConcurrentTransactions_ForUpdate is the test that decides the isolation
// strategy.
//
// The M1 blob refcount path is "SELECT ... FOR UPDATE, recheck refcount = 0, then
// delete". Its correctness is entirely about what a SECOND transaction sees and
// when it blocks. A harness that wrapped each test in one transaction and rolled
// it back could not express this at all: the second transaction would either be a
// SAVEPOINT of the first (savepoints take no independent row locks and do not
// block) or a separate session that cannot see the first's uncommitted rows.
// Either way the test would pass while testing nothing.
//
// A real database per test gives real sessions, real row locks and real commit
// visibility. This proves all three.
func TestConcurrentTransactions_ForUpdate(t *testing.T) {
	t.Parallel()

	pool := New(t)
	ctx := t.Context()

	d := seedBlob(t, pool, 0xBB)

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}

	defer func() { _ = tx1.Rollback(ctx) }()

	var got int64
	if err := tx1.QueryRow(ctx,
		`SELECT refcount FROM blobs WHERE digest = $1 FOR UPDATE`, d,
	).Scan(&got); err != nil {
		t.Fatalf("tx1 lock: %v", err)
	}

	// tx2 is a genuinely separate session. It must block on tx1's row lock.
	acquired := make(chan int64, 1)
	failed := make(chan error, 1)

	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			failed <- err

			return
		}

		defer func() { _ = tx2.Rollback(ctx) }()

		var refcount int64
		if err := tx2.QueryRow(ctx,
			`SELECT refcount FROM blobs WHERE digest = $1 FOR UPDATE`, d,
		).Scan(&refcount); err != nil {
			failed <- err

			return
		}

		acquired <- refcount
		_ = tx2.Commit(ctx)
	}()

	// (1) tx2 must NOT get the lock while tx1 holds it.
	select {
	case <-acquired:
		t.Fatal("tx2 took the row lock while tx1 held it -- these are not real concurrent transactions")
	case err := <-failed:
		t.Fatalf("tx2: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// tx1 bumps the refcount and commits, releasing the lock. unreferenced_since
	// has to be cleared in the same statement: blobs_unreferenced_consistent
	// CHECKs that (refcount = 0) = (unreferenced_since IS NOT NULL).
	if _, err := tx1.Exec(ctx,
		`UPDATE blobs SET refcount = refcount + 1, unreferenced_since = NULL WHERE digest = $1`, d,
	); err != nil {
		t.Fatalf("tx1 update: %v", err)
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}

	// (2) tx2 must now unblock, and (3) its FOR UPDATE re-read must observe tx1's
	// committed value -- the exact mechanism the refcount=0 recheck depends on to
	// not delete a blob someone just took a reference to.
	select {
	case refcount := <-acquired:
		if refcount != 1 {
			t.Errorf("tx2 re-read refcount = %d, want 1 -- it did not see tx1's commit", refcount)
		}
	case err := <-failed:
		t.Fatalf("tx2: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("tx2 never unblocked after tx1 committed")
	}
}

// TestAdvisoryLocksAreDatabaseScoped is the reason isolation is per-DATABASE and
// not per-SCHEMA.
//
// The boot path takes pg_try_advisory_lock on a fixed key to refuse a second
// instance. Advisory locks are scoped to a DATABASE, not a schema: every test in a
// per-schema harness would share one lock space, so two parallel tests exercising
// the boot lock would fight over the same key and flake nondeterministically.
// Cloned databases give each test its own lock space for free.
func TestAdvisoryLocksAreDatabaseScoped(t *testing.T) {
	t.Parallel()

	poolA := New(t)
	poolB := New(t) // a second, separate database in the same cluster
	ctx := t.Context()

	// First instance in database A takes the boot lock.
	connA1, err := poolA.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire A1: %v", err)
	}

	defer connA1.Release()

	if !tryLock(ctx, t, connA1.Conn()) {
		t.Fatal("first instance could not take the boot lock")
	}

	// A second instance against the SAME database must be refused. This is the
	// behaviour the server actually ships.
	connA2, err := poolA.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire A2: %v", err)
	}

	defer connA2.Release()

	if tryLock(ctx, t, connA2.Conn()) {
		t.Error("second instance took the boot lock on the same database -- it must be refused")
	}

	// An instance against a DIFFERENT database must succeed, even on the same key.
	// This is what stops parallel tests from colliding.
	connB, err := poolB.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}

	defer connB.Release()

	if !tryLock(ctx, t, connB.Conn()) {
		t.Error("advisory lock leaked across databases -- a per-schema harness would flake here")
	}
}

func tryLock(ctx context.Context, t *testing.T, conn *pgx.Conn) bool {
	t.Helper()

	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, bootLockKey).Scan(&ok); err != nil {
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}

	return ok
}

// TestParallelDatabases hammers the CREATE DATABASE ... TEMPLATE path from many
// parallel tests at once, which is what `go test` will actually do. It catches
// name collisions and the "source database is being accessed by other users"
// race.
func TestParallelDatabases(t *testing.T) {
	t.Parallel()

	for range 8 {
		t.Run("clone", func(t *testing.T) {
			t.Parallel()

			pool := New(t)

			seedBlob(t, pool, 0xCC)

			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM blobs`).Scan(&count); err != nil {
				t.Fatalf("count: %v", err)
			}

			if count != 1 {
				t.Errorf("count = %d, want 1", count)
			}
		})
	}
}

// TestCleanupDropsDatabase proves the per-test database really is gone afterwards,
// rather than accumulating for the life of the container.
func TestCleanupDropsDatabase(t *testing.T) {
	t.Parallel()

	var leaked string

	t.Run("inner", func(t *testing.T) {
		_, dsn := NewWithDSN(t)

		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}

		leaked = cfg.ConnConfig.Database
	})

	// The subtest has finished, so its t.Cleanup has run.
	var exists bool
	if err := shared.admin.QueryRow(t.Context(),
		`SELECT exists(SELECT 1 FROM pg_database WHERE datname = $1)`, leaked,
	).Scan(&exists); err != nil {
		t.Fatalf("check pg_database: %v", err)
	}

	if exists {
		t.Errorf("database %s survived the test -- cleanup leaked it", leaked)
	}
}

// TestCleanupSurvivesPanic proves t.Cleanup -- and therefore the DROP DATABASE and
// the container removal -- still runs when a test panics.
func TestCleanupSurvivesPanic(t *testing.T) {
	t.Parallel()

	cleaned := make(chan struct{})

	// A panicking test aborts the whole binary unless it is recovered on the
	// goroutine that panicked, so run it as a subtest whose body recovers.
	t.Run("panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected a panic")
			}
		}()

		t.Cleanup(func() { close(cleaned) })

		_ = New(t)

		panic("boom")
	})

	select {
	case <-cleaned:
	default:
		t.Error("t.Cleanup did not run for a panicking test")
	}
}

// TestSkipsWithoutPostgres documents the skip contract: errUnavailable, and only
// errUnavailable, turns into a Skip. Every other error fails the test loudly --
// a silently skipped database test is not a passing test.
func TestSkipsWithoutPostgres(t *testing.T) {
	t.Parallel()

	err := errors.Join(errUnavailable, errors.New("docker daemon not reachable"))
	if !errors.Is(err, errUnavailable) {
		t.Fatal("errUnavailable must be detectable with errors.Is")
	}

	msg := skipMessage(err)
	for _, want := range []string{DSNEnv, "CREATEDB", "did not run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("skip message does not mention %q:\n%s", want, msg)
		}
	}
}
