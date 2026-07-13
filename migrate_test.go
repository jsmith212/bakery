package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// virginDB hands back the DSN of a database with NO SCHEMA AT ALL -- no tables, and
// crucially no enum TYPES. dbtest clones an already-migrated template, so the schema
// is wiped back out to reproduce the one state a fresh install is actually in.
func virginDB(t *testing.T) string {
	t.Helper()

	pool, dsn := dbtest.NewWithDSN(t)

	if _, err := pool.Exec(t.Context(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("wipe the schema: %v", err)
	}

	// The clone's pool is now holding connections against a schema that no longer
	// exists. Close it so it cannot interfere; the test opens its own.
	pool.Close()

	return dsn
}

// TestMigrateUpWorksOnAVirginDatabase.
//
// THE FRESH-INSTALL PATH, and it was broken. `bakery migrate up` opened a db.NewPool,
// which registers every enum type on every connection via AfterConnect and REFUSES a
// connection where one is missing. On a virgin database none of them exists -- they
// are created BY the migrations this command runs -- so it failed its own Ping with
//
//	connect to database: ping database: enum type missing from database: "site_role"
//
// and could never create the types that would have let it connect. A perfect
// deadlock, on the very first command an operator runs, reachable only on a database
// that has never been migrated -- which is to say, never in any test that starts from
// a migrated fixture.
//
// server.Boot got this right (ping/lock/migrate on a bootstrap pool, then open the
// serving pool); the three standalone migrate verbs were left behind, and they are
// what a deploy pipeline that migrates before rolling out the new binary actually
// calls.
func TestMigrateUpWorksOnAVirginDatabase(t *testing.T) {
	dsn := virginDB(t)

	if err := migrateUp(t.Context(), config.MigrateUpCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
	}); err != nil {
		t.Fatalf("migrate up on a virgin database: %v\n\n"+
			"This is a fresh install. If this is an \"enum type missing\" error, the command "+
			"is opening the SERVING pool, whose AfterConnect refuses a database that does not "+
			"yet have the enum types -- the very types these migrations create.", err)
	}

	// And the schema really is there: the serving pool, enum registration and all, now
	// connects. That is the assertion that the migration actually ran rather than the
	// command merely not erroring.
	pool, err := db.NewPool(t.Context(), db.Config{URL: dsn, MaxConns: 0})
	if err != nil {
		t.Fatalf("the serving pool cannot connect after migrate up: %v", err)
	}

	defer pool.Close()

	assertMigrated(t, pool)
}

func assertMigrated(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	version, dirty, applied, err := db.MigrationVersion(pool)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}

	if !applied {
		t.Fatal("applied = false after migrate up")
	}

	if dirty {
		t.Error("dirty = true after a clean migrate up")
	}

	if version == 0 {
		t.Error("version = 0 after migrate up")
	}
}

// TestMigrateVersionWorksOnAVirginDatabase: reporting "no migrations applied" is the
// whole job of this command on a fresh database, and it could not do it -- it failed
// to connect for the same reason.
func TestMigrateVersionWorksOnAVirginDatabase(t *testing.T) {
	dsn := virginDB(t)

	if err := migrateVersion(t.Context(), config.MigrateVersionCmd{
		DBFlags: config.DBFlags{DBURL: dsn},
	}); err != nil {
		t.Fatalf("migrate version on a virgin database: %v", err)
	}
}
