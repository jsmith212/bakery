package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/alecthomas/kong"

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
		kong.Description("A multi-tenant build cache server."),
		kong.UsageOnError(),
	)

	if err := run(kctx.Command(), cli); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
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
		return fmt.Errorf("unknown command: %q", command)
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
