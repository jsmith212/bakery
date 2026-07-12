// Package dbtest hands each test its own migrated, isolated Postgres database.
//
// It resolves a Postgres server in one of two ways:
//
//  1. TEST_DB_URL is set  -- use that server (CI's `services: postgres:`, or a
//     long-lived local container). Nothing is started or stopped.
//  2. otherwise            -- start postgres:18-alpine with `docker run` on a
//     daemon-assigned free port, and kill it when the package's tests finish.
//
// With neither, tests Skip with instructions rather than fail.
//
// Isolation is one real database per test, cloned from a migrated template with
// CREATE DATABASE ... TEMPLATE -- not a per-test schema, and emphatically not a
// transaction rolled back at the end. TestConcurrentTransactions_ForUpdate and
// TestAdvisoryLocksAreDatabaseScoped are the proofs of why.
//
// Usage:
//
//	func TestMain(m *testing.M) { dbtest.Main(m) }
//
//	func TestThing(t *testing.T) {
//	    t.Parallel()
//	    pool := dbtest.New(t)
//	    ...
//	}
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
)

// DSNEnv is the environment variable that points dbtest at an existing server
// instead of making it start one. CI sets this to its postgres service.
const DSNEnv = "TEST_DB_URL"

// errUnavailable means "no Postgres, and that is not the test's fault" -- the
// signal to Skip. Every other error is a real failure and must fail the test.
var errUnavailable = errors.New("no postgres available")

// server is the process-wide Postgres the whole package's tests clone from.
type server struct {
	admin    *pgxpool.Pool // connected to the maintenance database
	adminDSN string
	template string     // name of the migrated database we CREATE ... TEMPLATE from
	ctr      *container // non-nil only when we started it ourselves
}

var (
	once    sync.Once
	shared  *server
	initErr error
)

// Main installs the package-wide container lifecycle. Every package with dbtest
// tests must call it:
//
//	func TestMain(m *testing.M) { dbtest.Main(m) }
//
// TestMain is the only hook Go gives us that runs after the last test in a
// package, so it is the only correct place to stop a container shared by all of
// them. t.Cleanup handles the per-test database (and runs on panic and on
// failure); this handles the server.
func Main(m *testing.M) {
	code := m.Run()

	teardown()
	os.Exit(code)
}

// New returns a pool on a freshly created, fully migrated database that no
// other test can see. The database is dropped when the test ends -- including
// when it panics or fails.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, _ := NewWithDSN(t)

	return pool
}

// NewWithDSN is New, plus the DSN of the new database. Tests that need a second
// independent connection to the same database -- the boot advisory-lock test
// simulating a second bakery instance, for one -- need the DSN to open it.
func NewWithDSN(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	srv := ensure(t)

	name := dbName(t)
	ctx := t.Context()

	// CREATE DATABASE ... TEMPLATE is a filesystem copy of an already-migrated
	// cluster directory. It costs milliseconds; re-running the migrations per
	// test would cost hundreds.
	if err := createFromTemplate(ctx, srv, name); err != nil {
		t.Fatalf("dbtest: create database %s: %v", name, err)
	}

	dsn := replaceDBName(srv.adminDSN, name)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		dropDatabase(srv, name)
		t.Fatalf("dbtest: open pool on %s: %v", name, err)
	}

	t.Cleanup(func() {
		// A test that leaked an acquired connection would make Close block
		// forever, hanging the whole run; DROP ... WITH (FORCE) below evicts
		// whatever is left, so a bounded wait is enough.
		closePool(pool)
		dropDatabase(srv, name)
	})

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("dbtest: ping %s: %v", name, err)
	}

	return pool, dsn
}

// ensure resolves the process-wide server exactly once.
func ensure(t *testing.T) *server {
	t.Helper()

	once.Do(func() { shared, initErr = setup() })

	if initErr != nil {
		if errors.Is(initErr, errUnavailable) {
			t.Skip(skipMessage(initErr))
		}

		t.Fatalf("dbtest: %v", initErr)
	}

	return shared
}

func setup() (*server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	srv := &server{admin: nil, adminDSN: "", template: "", ctr: nil}

	switch dsn := strings.TrimSpace(os.Getenv(DSNEnv)); {
	case dsn != "":
		srv.adminDSN = dsn

	default:
		if err := dockerAvailable(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", errUnavailable, err)
		}

		ctr, err := startContainer(ctx)
		if err != nil {
			return nil, err
		}

		srv.ctr = ctr
		srv.adminDSN = ctr.dsn
	}

	admin, err := pgxpool.New(ctx, srv.adminDSN)
	if err != nil {
		teardownServer(srv)

		return nil, fmt.Errorf("open admin pool: %w", err)
	}

	srv.admin = admin

	if err := waitPool(ctx, admin); err != nil {
		teardownServer(srv)

		return nil, fmt.Errorf("server never became ready: %w", err)
	}

	// One template per PROCESS, not a fixed name: `go test ./...` runs each
	// package as its own binary, and against a shared TEST_DB_URL server they
	// would otherwise race to create and migrate the same template.
	srv.template = "bk_tpl_" + randSuffix()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(srv.template)); err != nil {
		teardownServer(srv)

		return nil, fmt.Errorf("create template database: %w", err)
	}

	if err := migrateTemplate(ctx, replaceDBName(srv.adminDSN, srv.template)); err != nil {
		teardownServer(srv)

		return nil, fmt.Errorf("migrate template database: %w", err)
	}

	return srv, nil
}

// migrateTemplate applies the embedded schema to the template database and then
// drops every connection to it.
//
// The close is not tidiness: Postgres refuses CREATE DATABASE ... TEMPLATE t
// (SQLSTATE 55006) while ANY backend is attached to t, so a pool left open here
// would make every single per-test clone fail.
func migrateTemplate(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open template pool: %w", err)
	}
	defer closePool(pool)

	if err := waitPool(ctx, pool); err != nil {
		return fmt.Errorf("template database never became ready: %w", err)
	}

	if err := db.Migrate(pool); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	return nil
}

func teardown() {
	if shared != nil {
		teardownServer(shared)
	}
}

func teardownServer(srv *server) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	if srv.admin != nil {
		if srv.template != "" {
			_, _ = srv.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(srv.template)+" WITH (FORCE)")
		}

		closePool(srv.admin)
	}

	// Kill the container last: the DROPs above are pointless if it is already
	// gone, but harmless, and when TEST_DB_URL is in use they are the only
	// cleanup there is.
	if srv.ctr != nil {
		srv.ctr.remove()
	}
}

// createFromTemplate clones the migrated template into a new database.
func createFromTemplate(ctx context.Context, srv *server, name string) error {
	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", quote(name), quote(srv.template))

	// Postgres refuses to copy a template that any backend is connected to
	// (SQLSTATE 55006). We never hold a connection to it, but golang-migrate's
	// database/sql handle can take a moment to actually drop its socket, and a
	// stray `psql` on the template would do it too. Retry rather than flake.
	var err error

	for attempt := range 10 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w (last: %w)", ctx.Err(), err)
			case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
			}
		}

		if _, err = srv.admin.Exec(ctx, stmt); err == nil {
			return nil
		}

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}

	return fmt.Errorf("template %s stayed busy: %w", srv.template, err)
}

func dropDatabase(srv *server, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// WITH (FORCE) terminates any backend still attached -- a connection the
	// test leaked, or one a panic never got to close. Without it a leaked
	// connection turns cleanup into a permanent error and leaks the database.
	_, _ = srv.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
}

// waitPool is genuine readiness: a full Postgres handshake plus a query that
// the server actually answers.
//
// A TCP dial is NOT readiness. `docker run -p` binds the host port through
// docker-proxy the instant the container is created, so a dial succeeds long
// before postgres is listening -- and then the connection resets. Nor is a
// fixed sleep: it is either flaky or slow, usually both.
func waitPool(ctx context.Context, pool *pgxpool.Pool) error {
	var last error

	backoff := 25 * time.Millisecond

	for {
		err := ping(ctx, pool)
		if err == nil {
			return nil
		}

		last = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting: %w (last attempt: %w)", ctx.Err(), last)
		case <-time.After(backoff):
		}

		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}
}

func ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("select 1: %w", err)
	}

	return nil
}

// waitReady waits for a container we started, failing fast (with logs) if it
// dies rather than burning the whole timeout.
func waitReady(ctx context.Context, c *container) error {
	pool, err := pgxpool.New(ctx, c.dsn)
	if err != nil {
		return fmt.Errorf("open probe pool: %w", err)
	}
	defer closePool(pool)

	deadline := time.Now().Add(startupTimeout)
	backoff := 25 * time.Millisecond

	var last error

	for time.Now().Before(deadline) {
		err := ping(ctx, pool)
		if err == nil {
			return nil
		}

		last = err

		if !c.running(ctx) {
			return fmt.Errorf("container %s exited during startup (last attempt: %w)", c.name, last)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s: %w", c.name, ctx.Err())
		case <-time.After(backoff):
		}

		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}

	return fmt.Errorf("container %s never became ready (last attempt: %w)", c.name, last)
}

// closePool closes pool, but refuses to block forever on a connection the test
// forgot to release.
func closePool(pool *pgxpool.Pool) {
	done := make(chan struct{})

	go func() {
		pool.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// dbName derives a Postgres-legal, unique, greppable name from the test name.
// Postgres truncates identifiers at 63 bytes, so the random suffix -- the part
// that guarantees uniqueness -- is appended after truncation, never before.
func dbName(t *testing.T) string {
	t.Helper()

	var b strings.Builder

	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	const (
		prefix = "bk_"
		suffix = 13 // "_" + 12 hex
		max    = 63
	)

	stem := b.String()
	if len(stem) > max-len(prefix)-suffix {
		stem = stem[:max-len(prefix)-suffix]
	}

	return prefix + stem + "_" + randSuffix()
}

func randSuffix() string {
	return fmt.Sprintf("%012x", rand.Uint64()&0xffffffffffff)
}

// quote makes an identifier safe to interpolate. Database names cannot be bound
// as parameters -- CREATE DATABASE takes no placeholders -- so this is the only
// safe way to build the statement.
func quote(ident string) string {
	return pgx.Identifier{ident}.Sanitize()
}

// replaceDBName swaps the database out of a DSN, keeping host, credentials and
// every query parameter (sslmode, and anything CI's DSN carries) intact.
func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}

	u.Path = "/" + name

	return u.String()
}

func skipMessage(err error) string {
	return fmt.Sprintf(`
================================================================================
SKIPPING DATABASE TESTS -- no Postgres available.

  reason: %v

  Fix by doing ONE of these:

  1. Start Docker. dbtest will run %s itself, on a free port,
     and tear it down afterwards. Nothing else to configure.

  2. Point %s at a Postgres you can create databases in:

       export %s='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'

     The role must have CREATEDB. This is the path CI takes.

  These tests did not run. They did not pass.
================================================================================`,
		err, image, DSNEnv, DSNEnv)
}
