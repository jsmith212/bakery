// Package db owns the Postgres connection pool, the embedded schema migrations,
// and the boot advisory lock.
//
// Everything above this package talks to Postgres through *pgxpool.Pool and the
// sqlc-generated repository. There is no database/sql anywhere in the data path:
// the single *sql.DB this package constructs lives inside Migrate, is adapted
// from the pool by stdlib.OpenDBFromPool, and is discarded before the server
// serves a request. See migrate.go.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool tuning. sstate's HEAD storm is BB_NUMBER_THREADS-parallel, so MaxConns is
// the knob that decides whether the storm queues in the pool or in Postgres.
// These are starting points, not measured optima; bakery_db_pool_empty_acquires_total
// rising under load is the signal to raise MaxConns.
const (
	defaultMaxConns          = 16
	defaultMinConns          = 2
	defaultMaxConnLifetime   = time.Hour
	defaultMaxConnIdleTime   = 30 * time.Minute
	defaultHealthCheckPeriod = time.Minute

	pingTimeout = 5 * time.Second
)

// Config is what internal/db needs to reach Postgres.
type Config struct {
	// URL is the libpq connection string (DB_URL).
	URL string

	// MaxConns overrides the pool ceiling when non-zero.
	MaxConns int32
}

// NewPool builds the pool and PROVES it can reach Postgres before returning.
//
// pgxpool.New deliberately does not connect -- its own doc says "A pool returns
// without waiting for any connections to be established". Returning an unpinged
// pool means the process boots green against a dead database and the first
// request is what discovers it, which is exactly the bug kbi ships. So: Ping,
// with a timeout, and close the pool on failure so a caller that ignores the
// error cannot leak it.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DB_URL: %w", err)
	}

	poolCfg.MaxConns = defaultMaxConns
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}

	poolCfg.MinConns = defaultMinConns
	poolCfg.MaxConnLifetime = defaultMaxConnLifetime
	poolCfg.MaxConnIdleTime = defaultMaxConnIdleTime
	poolCfg.HealthCheckPeriod = defaultHealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
