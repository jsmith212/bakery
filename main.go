package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"github.com/alecthomas/kong"

	clicmd "github.com/jsmith212/bakery/internal/cli"
	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/server"
	"github.com/jsmith212/bakery/web"
)

// version is overridden at link time with -ldflags "-X main.version=...".
var version = ""

func main() {
	slog.SetDefault(slog.New(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	))

	var cli config.CLI

	kctx := kong.Parse(&cli,
		kong.Name("bakery"),
		kong.Description("A multi-tenant build cache server, and its API client."),
		kong.UsageOnError(),
	)

	if err := run(commandPath(kctx), cli); err != nil {
		fail(err)
	}
}

// fail reports a fatal error and exits.
//
// The SERVER's errors go through slog, because they are read out of a log
// aggregator. A CLIENT's errors are read by a person looking at their terminal
// two lines below the command they just typed, and a JSON envelope with a
// timestamp and a level is worse than a sentence. `bakery key list` against an
// expired token prints "not signed in to this server: run bakery login", and
// nothing else.
func fail(err error) {
	var ce clientError

	if errors.As(err, &ce) {
		fmt.Fprintln(os.Stderr, "bakery: "+ce.err.Error())
		os.Exit(1)
	}

	slog.Error("fatal", "error", err)
	os.Exit(1)
}

// clientError marks an error as coming from a client command, so fail renders it
// for a human rather than for a log aggregator.
type clientError struct{ err error }

func (e clientError) Error() string { return e.err.Error() }
func (e clientError) Unwrap() error { return e.err }

// commandPath is the selected command as a space-joined path of command names.
//
// kong.Context.Command() would also splice in the positional arguments -- it
// renders `bakery org create acme` as "org create <slug>" -- which makes the
// dispatch switch below depend on the spelling of every argument. Joining only
// the command nodes means renaming an argument cannot silently route a command
// into the default case.
func commandPath(kctx *kong.Context) string {
	parts := make([]string, 0, len(kctx.Path))

	for _, p := range kctx.Path {
		if p.Command != nil {
			parts = append(parts, p.Command.Name)
		}
	}

	return strings.Join(parts, " ")
}

func run(command string, cli config.CLI) error {
	ctx := context.Background()

	switch command {
	case "serve":
		return serve(cli.Serve)
	case "migrate up":
		return migrateUp(ctx, cli.Migrate.Up)
	case "migrate down":
		return migrateDown(ctx, cli.Migrate.Down)
	case "migrate version":
		return migrateVersion(ctx, cli.Migrate.Version)
	case "version":
		fmt.Println(buildVersion())

		return nil
	default:
		// Everything else is a client command. It needs no database pool and no
		// server config -- just an HTTP client and the token cache.
		if err := clicmd.Run(ctx, command, cli); err != nil {
			return clientError{err: err}
		}

		return nil
	}
}

func migrateUp(ctx context.Context, cmd config.MigrateUpCmd) error {
	pool, err := db.NewPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: 0})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	defer pool.Close()

	if err := db.Migrate(pool); err != nil {
		return err
	}

	version, dirty, applied, err := db.MigrationVersion(pool)
	if err != nil {
		return err
	}

	slog.Info("migrations applied", "version", version, "dirty", dirty, "applied", applied)

	return nil
}

// migrateDown drops every table. --yes is not decoration: there is no interactive
// confirmation to fall back on when this runs from a deploy pipeline, and the
// difference between the staging DSN and the production one is a few characters in
// an environment variable.
func migrateDown(ctx context.Context, cmd config.MigrateDownCmd) error {
	if !cmd.Yes {
		return errors.New("migrate down drops every table in the database; pass --yes to confirm")
	}

	pool, err := db.NewPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: 0})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	defer pool.Close()

	if err := db.MigrateDown(pool); err != nil {
		return err
	}

	slog.Info("migrations rolled back")

	return nil
}

func migrateVersion(ctx context.Context, cmd config.MigrateVersionCmd) error {
	pool, err := db.NewPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: 0})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	defer pool.Close()

	version, dirty, applied, err := db.MigrationVersion(pool)
	if err != nil {
		return err
	}

	if !applied {
		fmt.Println("no migrations applied")

		return nil
	}

	// A dirty schema means a migration failed part-way and the database is in a
	// state no migration file describes. Say so loudly: the fix is manual.
	if dirty {
		fmt.Printf("%d (DIRTY -- a migration failed part-way; repair by hand)\n", version)

		return nil
	}

	fmt.Println(version)

	return nil
}

func serve(cmd config.ServeCmd) error {
	dist, err := web.Dist()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	slog.Info("bakery starting", "version", buildVersion())

	return server.New(server.Config{
		Addr:    cmd.Addr(),
		Version: buildVersion(),
		Dist:    dist,
	}).Run(context.Background())
}

// buildVersion prefers the linker-injected version and falls back to the VCS
// revision the toolchain stamps into the binary, so a `go build` with no ldflags
// still reports something useful.
func buildVersion() string {
	if version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	var revision, modified string

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if revision == "" {
		return "dev"
	}

	if modified == "true" {
		return revision + "-dirty"
	}

	return revision
}
