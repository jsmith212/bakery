package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// Store is the pool plus a REAL transaction wrapper.
//
// The wrapper is the point. The obvious shape -- bind one *repository.Queries to
// the pool, hand it to every service, and let a service open a pgx.Tx alongside it
// -- LOOKS correct and is a no-op: the queries still execute on the POOL, outside
// the transaction, which opens, does nothing, and commits. (This is exactly what
// kbi ships, and grep confirms it never calls WithTx anywhere.)
//
// For Bakery that is not a stylistic problem, it is a correctness one. The blob
// refcount protocol is pg_advisory_xact_lock + SELECT ... FOR UPDATE + a recheck,
// and every one of those is meaningless unless the statements share a transaction:
// an advisory XACT lock taken on a pooled connection is released the instant that
// statement returns, and a FOR UPDATE in a different transaction locks nothing the
// caller can rely on.
//
// So: Tx REBINDS Queries onto the pgx.Tx (repository.Queries.WithTx) and hands
// THAT to the closure. A caller physically cannot run a statement outside the
// transaction it thinks it is in.
type Store struct {
	*repository.Queries

	pool *pgxpool.Pool
}

// NewStore binds the generated queries to the pool. *pgxpool.Pool satisfies the
// generated DBTX interface directly -- there is no database/sql adapter here.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Queries: repository.New(pool), pool: pool}
}

// Pool exposes the underlying pool for the few callers that need raw access --
// the boot lock, the pgxpool metrics collector.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Tx runs fn inside a single transaction, with a *repository.Queries bound to
// that transaction. It commits when fn returns nil and rolls back otherwise.
//
// The isolation level is deliberately left at pgx's default, READ COMMITTED, and
// blob.Service DEPENDS on that. Under REPEATABLE READ the snapshot is taken at the
// transaction's first statement -- which on the blob path is the advisory-lock
// acquisition, BEFORE the lock is granted -- so the post-lock SELECT would read a
// stale world and the anti-resurrection protocol would be actively unsafe. Do not
// pass pgx.TxOptions{IsoLevel: RepeatableRead} on any blob path.
func (s *Store) Tx(ctx context.Context, fn func(*repository.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Rollback after a successful Commit is a no-op that returns pgx.ErrTxClosed,
	// so this is safe unconditionally and covers the panic path too.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// Compile-time proof that the pool can back the generated queries with no
// database/sql anywhere in the path.
var _ repository.DBTX = (*pgxpool.Pool)(nil)

// ... and that a transaction can too.
var _ repository.DBTX = (pgx.Tx)(nil)
