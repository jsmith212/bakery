package db

import (
	"context"
	"errors"
	"fmt"

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

// BootLock is a session-scoped advisory lock held for the process lifetime.
//
// It pins a DEDICATED connection, and that is not an optimisation. Advisory
// locks taken with pg_try_advisory_lock are scoped to the SESSION, so the lock
// lives and dies with the connection that took it. Taking it through
// pool.QueryRow would hand that connection straight back to the pool, and when
// pgxpool later recycled it (MaxConnLifetime, or a failed health check) the lock
// would release SILENTLY and a second instance could boot -- defeating the
// invariant while every log line still says the lock was taken.
type BootLock struct {
	conn *pgxpool.Conn
}

// AcquireBootLock takes the boot lock, or returns ErrLocked if another instance
// holds it. The caller must hold the returned *BootLock for the life of the
// process and Release it on shutdown.
func AcquireBootLock(ctx context.Context, pool *pgxpool.Pool) (*BootLock, error) {
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

	return &BootLock{conn: conn}, nil
}

// Release drops the lock and returns the connection to the pool. It is safe to
// call on a nil *BootLock, so a --allow-multi-instance boot (which never takes
// one) can defer it unconditionally.
func (l *BootLock) Release() {
	if l == nil || l.conn == nil {
		return
	}

	// Best effort: on the shutdown path there is nothing useful left to do with
	// an error, and closing the session releases the lock regardless.
	_, _ = l.conn.Exec(context.Background(),
		"SELECT pg_advisory_unlock($1, $2)", bootLockClassID, bootLockObjID)

	l.conn.Release()
	l.conn = nil
}
