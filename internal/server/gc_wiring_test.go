package server

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// orphanedRun forges the gc_runs row a process that died mid-sweep leaves behind: it
// is still 'running', so it holds the partial unique index's active slot and every
// later real run collides with it until something clears it.
func orphanedRun(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64

	if err := pool.QueryRow(t.Context(),
		`INSERT INTO gc_runs (grace_period) VALUES (interval '24 hours') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("seed an orphaned gc run: %v", err)
	}

	return id
}

func runStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()

	var status string

	if err := pool.QueryRow(t.Context(), `SELECT status::text FROM gc_runs WHERE id = $1`, id).
		Scan(&status); err != nil {
		t.Fatalf("read gc run %d: %v", id, err)
	}

	return status
}

// bootUntilReady boots a server, waits for its listeners, then shuts it down.
func bootUntilReady(t *testing.T, cmd config.ServeCmd) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan struct{}, 1)
	done := make(chan error, 1)

	go func() {
		done <- Boot(ctx, BootParams{
			Cmd: cmd, Version: "test", Dist: testDist(),
			Ready: func(_, _, _ net.Addr) { ready <- struct{}{} },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Boot returned before binding: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("server never became ready")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("server never shut down")
	}
}

func serveCmd(dsn, storageDir string) config.ServeCmd {
	return config.ServeCmd{
		DBFlags:     config.DBFlags{DBURL: dsn},
		Host:        "127.0.0.1",
		Port:        0,
		MetricsAddr: "127.0.0.1:0",
		GRPCAddr:    "",
		StorageDir:  storageDir,
		GC: config.GCFlags{
			GCEnabled: true, GCInterval: time.Hour, GCUsageInterval: time.Hour,
			GCGracePeriod: 24 * time.Hour, GCBatchSize: 1000, GCBatchPause: time.Millisecond,
			GCDisableRetention: false, GCTouchInterval: time.Minute, GCTouchStaleness: time.Hour,
		},
	}
}

// THE BOOT REAPER RUNS IFF THIS PROCESS HOLDS THE BOOT ADVISORY LOCK (spec §9.3,
// finding 4).
//
// Holding the lock is the PROOF that anything still 'running' belongs to this
// process's own dead predecessor. Gate the reaper on the --allow-multi-instance flag
// instead and a booting instance marks a HEALTHY instance's live sweep failed: the
// audit trail then says a run failed that did not, and the active slot is handed to
// a second concurrent sweep -- two mark-and-sweeps with a live mutator, where the
// mutator is the other GC.
func TestBootMarksOrphanedRunsFailed(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	id := orphanedRun(t, pool)

	bootUntilReady(t, serveCmd(dsn, t.TempDir()))

	if got := runStatus(t, pool, id); got != "failed" {
		t.Errorf("orphaned run status = %q, want failed: it holds the active slot until it is cleared", got)
	}
}

// The twin, and the one that matters: a boot that does NOT hold the lock must leave
// another instance's run alone.
func TestBootWithoutTheLockLeavesRunningRunsAlone(t *testing.T) {
	t.Parallel()

	pool, dsn := dbtest.NewWithDSN(t)
	id := orphanedRun(t, pool)

	// Somebody else holds the lock -- exactly the state --allow-multi-instance exists
	// to let an operator boot into, and exactly the state in which the run above may
	// belong to a live instance rather than a dead one.
	holder, err := db.NewBootstrapPool(t.Context(), db.Config{URL: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open a second pool: %v", err)
	}

	defer holder.Close()

	lock, err := db.AcquireBootLock(t.Context(), holder)
	if err != nil {
		t.Fatalf("AcquireBootLock() error = %v", err)
	}

	defer lock.Release()

	cmd := serveCmd(dsn, t.TempDir())
	cmd.AllowMultiInstance = true

	bootUntilReady(t, cmd)

	if got := runStatus(t, pool, id); got != "running" {
		t.Errorf("run status = %q, want running: a boot that holds no lock cannot know whose sweep "+
			"that is, and marking it failed is a lie plus a second concurrent sweep", got)
	}
}

// A LOST BOOT LOCK STOPS THE SERVER (M: R7#2).
//
// BootLock's own contract says so in as many words: "The server MUST select on this
// and shut down: continuing to serve past it is the two-writers state the lock
// exists to forbid." Nothing consumed Lost() before, so a process that lost its
// lock to a second instance kept serving with a stale route cache, a stale object
// LRU and a GC that believes its pending-read set is complete -- with one ERROR
// line from the watcher as the only sign.
//
// The seam is unit-tested rather than driven through a real Boot: reproducing a
// genuine loss needs the pinned session killed AND another process holding the lock
// before the watcher's next tick, which is a slow, racy way to assert a select
// statement. internal/db's own bootlock tests cover the DETECTION; this covers what
// the server does with it.
func TestWatchBootLockCancelsTheServerOnLoss(t *testing.T) {
	t.Parallel()

	t.Run("a loss cancels the lifetime context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		lost := make(chan struct{})

		go watchBootLock(t.Context(), lost, cancel, testLogger())

		select {
		case <-ctx.Done():
			t.Fatal("the server context was cancelled before the lock was lost")
		default:
		}

		close(lost)

		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the server kept serving after losing the boot lock: it is no longer the " +
				"sole writer and every process-local cache it owns is now stale")
		}
	})

	t.Run("an ordinary shutdown does not leak the watcher", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			watchBootLock(ctx, make(chan struct{}), func() {
				t.Error("watchBootLock cancelled the server on a clean shutdown")
			}, testLogger())
		}()

		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("watchBootLock outlived its context")
		}
	})

	t.Run("a nil channel is the --allow-multi-instance boot", func(t *testing.T) {
		t.Parallel()

		done := make(chan struct{})

		go func() {
			defer close(done)

			watchBootLock(t.Context(), nil, func() {
				t.Error("watchBootLock cancelled the server for a boot that holds no lock")
			}, testLogger())
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("watchBootLock blocked forever on a nil Lost() channel")
		}
	})
}

// testLogger discards: these tests assert behaviour, not log output, and a real
// handler would interleave a loud ERROR line into every run.
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
