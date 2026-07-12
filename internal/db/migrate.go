package db

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	// The pgx/v5 database driver. NOT migrate's `database/postgres` driver: that
	// one is lib/pq, which this project forbids, and blank-importing it drags
	// lib/pq into the module graph with no compile error to warn you.
	//
	// It must be aliased. Its package name is literally `pgx`, which collides
	// with github.com/jackc/pgx/v5.
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// migrationsFS carries the schema. Plain `migrations`, not `all:migrations`:
// there are no dotfiles or underscore-prefixed files in here, and the `all:`
// prefix is a frontend concern (SvelteKit's `_app/`), not a SQL one.
//
//go:embed migrations
var migrationsFS embed.FS

// Migrations exposes the embedded migration files. sqlc reads this directory
// from disk as its schema; this is for anything that wants them at runtime.
func Migrations() embed.FS { return migrationsFS }

// migrateDriverName is the name golang-migrate's pgx/v5 driver registers itself
// under: `database.Register("pgx5", &db)`. It is NOT "pgx", and it is NOT
// "pgx/v5" (that is the database/sql driver name the thing opens internally).
// Getting it wrong fails at runtime, not at compile time.
const migrateDriverName = "pgx5"

// Migrate applies every embedded up-migration.
//
// It runs over the EXISTING pool. golang-migrate's only instance constructor is
// WithInstance(*sql.DB, *Config), so a *sql.DB is unavoidable -- but
// stdlib.OpenDBFromPool adapts the pool we already have rather than opening a
// second connection, sets MaxIdleConns to zero so it cannot starve the pool, and
// does not close the pool when the *sql.DB is closed. The *sql.DB is a boot-only
// shim and never a data path.
func Migrate(pool *pgxpool.Pool) error {
	return withMigrator(pool, func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("apply migrations: %w", err)
		}

		return nil
	})
}

// MigrateDown rolls every migration back. It exists for `bakery migrate down`
// and for the migration test, which must prove up -> down -> up is clean.
func MigrateDown(pool *pgxpool.Pool) error {
	return withMigrator(pool, func(m *migrate.Migrate) error {
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("roll migrations back: %w", err)
		}

		return nil
	})
}

// MigrationVersion reports the applied schema version. dirty means a previous
// migration failed part-way and the schema is in an unknown state; a dirty
// database must be repaired by hand. version is 0 and applied is false when no
// migration has ever run.
func MigrationVersion(pool *pgxpool.Pool) (version uint, dirty, applied bool, err error) {
	err = withMigrator(pool, func(m *migrate.Migrate) error {
		v, d, verr := m.Version()
		if errors.Is(verr, migrate.ErrNilVersion) {
			return nil
		}

		if verr != nil {
			return fmt.Errorf("read schema version: %w", verr)
		}

		version, dirty, applied = v, d, true

		return nil
	})

	return version, dirty, applied, err
}

// withMigrator builds a migrator over pool, hands it to fn, and tears it down.
//
// The teardown is load-bearing for the test harness: Postgres refuses
// CREATE DATABASE ... TEMPLATE t while any backend is attached to t, so the
// *sql.DB migrate opened must actually be closed before the template is cloned.
func withMigrator(pool *pgxpool.Pool, fn func(*migrate.Migrate) error) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)

	drv, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		_ = sqlDB.Close()

		return fmt.Errorf("open migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, migrateDriverName, drv)
	if err != nil {
		_ = sqlDB.Close()

		return fmt.Errorf("init migrator: %w", err)
	}

	fnErr := fn(m)

	// m.Close() closes both the source and the database driver, and the database
	// driver owns sqlDB.
	srcErr, dbErr := m.Close()

	return errors.Join(fnErr, srcErr, dbErr)
}
