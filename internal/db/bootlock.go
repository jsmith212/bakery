package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLocked means another Bakery instance already holds the boot lock on this
// database. Boot must refuse unless --allow-multi-instance was passed.
var ErrLocked = errors.New("another bakery instance holds the boot lock on this database")

// The boot lock lives in PostgreSQL's TWO-int4 advisory key space, which the
// server keeps strictly separate from the single-bigint space.
//
// That separation is the whole point, and it is load-bearing twice:
//
//  1. blob.Service and the GC serialise on pg_advisory_xact_lock(
//     bakery_blob_lock_key(digest)), a SINGLE bigint derived from a sha256. If the
//     boot lock shared that space, a digest whose first 8 bytes happened to equal
//     the boot key would block forever on a lock this process holds for its entire
//     lifetime -- a PUT wedged permanently, reproducible on exactly one blob.
//  2. golang-migrate takes pg_advisory_lock(bigint) too (crc32(name) * salt,
//     truncated to uint32), so it is in the single-bigint space as well.
//
// The values spell "BAKE"/"RY" and carry no meaning beyond being fixed.
const (
	bootLockClassID int32 = 0x42414B45 // "BAKE"
	bootLockObjID   int32 = 0x5259     // "RY"
)

// defaultWatchInterval is how often the watcher re-checks that the pinned session
// -- and therefore the advisory lock -- is still alive. Coarse on purpose: a lost
// lock is a rare, catastrophic event, and a ping every few seconds detects a
// Postgres restart or a killed backend fast enough while costing nothing.
const defaultWatchInterval = 5 * time.Second

// BootLock is a session-scoped advisory lock held for the process lifetime.
//
// It pins a DEDICATED connection, and that is not an optimisation. Advisory
// locks taken with pg_try_advisory_lock are scoped to the SESSION, so the lock
// lives and dies with the connection that took it. Taking it through
// pool.QueryRow would hand that connection straight back to the pool, and when
// pgxpool later recycled it (MaxConnLifetime, or a failed health check) the lock
// would release SILENTLY and a second instance could boot -- defeating the
// invariant while every log line still says the lock was taken.
//
// # Why a watcher, and not just a pinned connection
//
// Pinning the connection is necessary but NOT sufficient. The session dies with
// its Postgres backend, and a backend dies whenever Postgres restarts, fails over,
// or the backend is terminated -- routine operational events, none of which the
// pooled client notices beyond a reconnect. When it happens the advisory lock
// vanishes server-side while THIS process keeps serving as though it still holds
// it, and a second instance can now boot: two writers, both believing they are the
// only one, which is exactly what the in-process route cache, the LRU and the
// single-writer GC assume can never happen.
//
// So a background watcher pings the pinned session. When the session is gone it
// tries to RE-ACQUIRE the lock on a fresh connection:
//
//   - re-acquired -> the common case (a single instance survived a Postgres
//     restart); keep serving, now on the new session.
//   - held by someone else -> another instance booted during our outage and took
//     the lock; we can no longer guarantee we are the sole writer, so we SIGNAL
//     LOSS on Lost() and stop. The server must treat a closed Lost() channel as
//     fatal and shut down.
//   - database unreachable -> nobody else can hold the lock while the server is
//     down either, so keep the belief and retry on the next tick.
type BootLock struct {
	pool     *pgxpool.Pool
	interval time.Duration
	log      *slog.Logger

	mu   sync.Mutex
	conn *pgxpool.Conn // the pinned session holding the lock; nil while re-acquiring or after loss

	lost     chan struct{} // closed exactly once when the lock is irrecoverably lost
	lostOnce sync.Once

	stop     chan struct{} // closed by Release to ask the watcher to exit
	stopOnce sync.Once
	done     chan struct{} // closed when the watcher goroutine has exited
}

// BootLockOption tunes AcquireBootLock. The zero set of options is the production
// configuration; the options exist mainly so tests can drive the watcher fast.
type BootLockOption func(*bootLockOptions)

type bootLockOptions struct {
	interval time.Duration
	log      *slog.Logger
}

// WithWatchInterval sets how often the watcher re-verifies the lock. A non-positive
// value is ignored and the default is kept.
func WithWatchInterval(d time.Duration) BootLockOption {
	return func(o *bootLockOptions) {
		if d > 0 {
			o.interval = d
		}
	}
}

// WithLogger sets the logger the watcher uses for its warnings. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) BootLockOption {
	return func(o *bootLockOptions) {
		if l != nil {
			o.log = l
		}
	}
}

// AcquireBootLock takes the boot lock, or returns ErrLocked if another instance
// holds it. The caller must hold the returned *BootLock for the life of the
// process and Release it on shutdown.
//
// ctx governs the lifetime of the background watcher: cancel it (or call Release)
// to stop watching. It should be the SERVER lifetime context, not a short boot
// context -- a watcher tied to a boot context stops watching the moment boot
// finishes, which is the moment the invariant starts mattering.
func AcquireBootLock(ctx context.Context, pool *pgxpool.Pool, opts ...BootLockOption) (*BootLock, error) {
	o := bootLockOptions{interval: defaultWatchInterval, log: slog.Default()}
	for _, fn := range opts {
		fn(&o)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire boot-lock connection: %w", err)
	}

	var ok bool

	err = conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1, $2)", bootLockClassID, bootLockObjID,
	).Scan(&ok)
	if err != nil {
		conn.Release()

		return nil, fmt.Errorf("take boot lock: %w", err)
	}

	if !ok {
		conn.Release()

		return nil, ErrLocked
	}

	l := &BootLock{
		pool:     pool,
		interval: o.interval,
		log:      o.log,
		conn:     conn,
		lost:     make(chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	go l.watch(ctx)

	return l, nil
}

// Lost is closed exactly once, when the watcher determines the lock has been
// irrecoverably lost -- the session died AND another instance has since taken the
// lock. The server MUST select on this and shut down: continuing to serve past it
// is the two-writers state the lock exists to forbid.
//
// It is never closed for a --allow-multi-instance boot (which holds no lock and so
// has no watcher) and never closed on a clean Release.
func (l *BootLock) Lost() <-chan struct{} {
	if l == nil {
		// A nil BootLock (the --allow-multi-instance path) can never lose a lock it
		// does not hold. Hand back a channel that never fires so callers can select
		// on it unconditionally.
		return nil
	}

	return l.lost
}

// watch pings the pinned session on an interval and drives recovery when it dies.
func (l *BootLock) watch(ctx context.Context) {
	defer close(l.done)

	t := time.NewTicker(l.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-t.C:
			if l.check(ctx) == checkLost {
				l.lostOnce.Do(func() { close(l.lost) })
				l.log.Error("boot lock was lost and re-acquired by another instance; " +
					"this process is no longer the sole writer and must shut down")

				return
			}
		}
	}
}

// checkResult is what one watch tick concluded.
type checkResult int

const (
	checkHeld      checkResult = iota // the pinned session answered; we still hold the lock
	checkRecovered                    // the session died but we re-took the lock on a fresh one
	checkRetry                        // could not verify (database unreachable); try again next tick
	checkLost                         // the session died and someone else now holds the lock
)

// check verifies the lock is still ours. When the pinned session is alive it is a
// single ping; when it is dead (or we are mid-recovery) it attempts to re-acquire.
func (l *BootLock) check(ctx context.Context) checkResult {
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()

	if conn != nil {
		// A generous timeout so a momentarily slow-but-alive session is never
		// mistaken for a dead one; a genuinely dead backend errors immediately
		// (connection reset), well inside it.
		pingCtx, cancel := context.WithTimeout(ctx, max(l.interval, 2*time.Second))
		err := conn.Ping(pingCtx)
		cancel()

		if err == nil {
			return checkHeld
		}

		l.log.Warn("boot lock: pinned database session is unhealthy; verifying the lock", "error", err)
	}

	return l.recover(ctx)
}

// recover runs after the pinned session has failed a ping. It tears the dead
// session down (which releases any lock it might still hold) and tries to re-take
// the lock on a fresh connection.
func (l *BootLock) recover(ctx context.Context) checkResult {
	l.mu.Lock()
	old := l.conn
	l.conn = nil
	l.mu.Unlock()

	if old != nil {
		// Destroy the broken connection rather than return it to the pool, so its
		// (possibly still-lingering) server-side session is torn down and cannot keep
		// holding the lock against our own re-acquire attempt below.
		old.Release()
	}

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		// The database is unreachable -- e.g. Postgres is still restarting. Nobody
		// else can hold the lock while the server is down, so keep believing we hold
		// it and try again on the next tick.
		l.log.Warn("boot lock: database unreachable while verifying the lock; will retry", "error", err)

		return checkRetry
	}

	var ok bool

	err = conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1, $2)", bootLockClassID, bootLockObjID,
	).Scan(&ok)
	if err != nil {
		conn.Release()
		l.log.Warn("boot lock: could not verify the lock; will retry", "error", err)

		return checkRetry
	}

	if !ok {
		// The lock is held -- and since our old session is gone, it is held by
		// SOMEONE ELSE. We are no longer the sole writer.
		conn.Release()

		return checkLost
	}

	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()

	l.log.Warn("boot lock: the database session was lost; the lock was re-acquired on a new connection")

	return checkRecovered
}

// Release drops the lock and returns the connection to the pool. It is safe to
// call on a nil *BootLock, so a --allow-multi-instance boot (which never takes
// one) can defer it unconditionally.
func (l *BootLock) Release() {
	if l == nil {
		return
	}

	// Stop the watcher and wait for it to exit, so it cannot swap or close the
	// pinned connection underneath us.
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done

	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()

	if conn == nil {
		return
	}

	// Best effort: on the shutdown path there is nothing useful left to do with
	// an error, and closing the session releases the lock regardless.
	_, _ = conn.Exec(context.Background(),
		"SELECT pg_advisory_unlock($1, $2)", bootLockClassID, bootLockObjID)

	conn.Release()
}
