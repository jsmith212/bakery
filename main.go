package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/alecthomas/kong"

	"github.com/jsmith212/bakery/internal/config"
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
	switch command {
	case "serve":
		return serve(cli.Serve)
	case "version":
		fmt.Println(buildVersion())

		return nil
	default:
		return fmt.Errorf("unknown command: %q", command)
	}
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
