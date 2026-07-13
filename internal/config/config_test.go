package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// dsn is the DB_URL every command now requires. Kong refuses to parse without it,
// which is the point: a server that boots with no database is not a useful state.
const dsn = "postgres://bakery:secret@localhost:5432/bakery?sslmode=disable"

func parse(t *testing.T, args ...string) (*kong.Context, *CLI) {
	t.Helper()

	var cli CLI

	parser, err := kong.New(&cli, kong.Name("bakery"))
	if err != nil {
		t.Fatalf("build parser: %v", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}

	return kctx, &cli
}

func TestServeCmdBinding(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		args     []string
		wantHost string
		wantPort int
		wantAddr string
	}{
		{
			name:     "defaults",
			env:      map[string]string{"DB_URL": dsn},
			args:     []string{"serve"},
			wantHost: "",
			wantPort: 8080,
			wantAddr: ":8080",
		},
		{
			name:     "env vars land in the struct",
			env:      map[string]string{"DB_URL": dsn, "HOST": "127.0.0.1", "PORT": "9999"},
			args:     []string{"serve"},
			wantHost: "127.0.0.1",
			wantPort: 9999,
			wantAddr: "127.0.0.1:9999",
		},
		{
			name:     "PORT alone leaves HOST empty",
			env:      map[string]string{"DB_URL": dsn, "PORT": "1234"},
			args:     []string{"serve"},
			wantHost: "",
			wantPort: 1234,
			wantAddr: ":1234",
		},
		{
			name:     "flags beat env",
			env:      map[string]string{"DB_URL": dsn, "HOST": "127.0.0.1", "PORT": "9999"},
			args:     []string{"serve", "--host", "0.0.0.0", "--port", "3000"},
			wantHost: "0.0.0.0",
			wantPort: 3000,
			wantAddr: "0.0.0.0:3000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			kctx, cli := parse(t, tt.args...)

			if got := kctx.Command(); got != "serve" {
				t.Fatalf("got command %q, want %q", got, "serve")
			}

			if cli.Serve.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", cli.Serve.Host, tt.wantHost)
			}

			if cli.Serve.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", cli.Serve.Port, tt.wantPort)
			}

			if got := cli.Serve.Addr(); got != tt.wantAddr {
				t.Errorf("Addr() = %q, want %q", got, tt.wantAddr)
			}

			if cli.Serve.DBURL != dsn {
				t.Errorf("DBURL = %q, want %q", cli.Serve.DBURL, dsn)
			}
		})
	}
}

func TestServeCmdDefaults(t *testing.T) {
	t.Setenv("DB_URL", dsn)

	_, cli := parse(t, "serve")

	// Loopback by default. /metrics leaks every org and project slug and their
	// stored byte counts, so a public default would be a tenant-list disclosure on
	// every deployment that never touched the flag.
	if got := cli.Serve.MetricsAddr; got != "127.0.0.1:9090" {
		t.Errorf("MetricsAddr default = %q, want 127.0.0.1:9090 -- /metrics must not default to a public bind", got)
	}

	if cli.Serve.Headless {
		t.Error("Headless defaults to true; the console should be served unless asked otherwise")
	}

	if cli.Serve.AllowMultiInstance {
		t.Error("AllowMultiInstance defaults to true; boot must refuse a second instance by default")
	}

	for _, want := range []string{"openid", "groups", "offline_access"} {
		if !slices.Contains(cli.Serve.OIDCScopes, want) {
			t.Errorf("default OIDC scopes %v do not request %q", cli.Serve.OIDCScopes, want)
		}
	}
}

// TestDevLoginDefaultsOffAndIsFlagOrEnvOnly is the invariant, and it is worth a
// test of its own: DEV_LOGIN_ENABLED mints a session for a synthetic site admin
// with no credential. Nothing but an operator's env var or flag may turn it on, and
// it must be off unless they did.
func TestDevLoginDefaultsOffAndIsFlagOrEnvOnly(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want bool
	}{
		{
			name: "off by default",
			env:  map[string]string{"DB_URL": dsn},
			args: []string{"serve"},
			want: false,
		},
		{
			name: "env var turns it on",
			env:  map[string]string{"DB_URL": dsn, "DEV_LOGIN_ENABLED": "true"},
			args: []string{"serve"},
			want: true,
		},
		{
			name: "flag turns it on",
			env:  map[string]string{"DB_URL": dsn},
			args: []string{"serve", "--dev-login-enabled"},
			want: true,
		},
		{
			name: "env var explicitly off stays off",
			env:  map[string]string{"DB_URL": dsn, "DEV_LOGIN_ENABLED": "false"},
			args: []string{"serve"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, cli := parse(t, tt.args...)

			if got := cli.Serve.DevLoginEnabled; got != tt.want {
				t.Errorf("DevLoginEnabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandTree(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "serve", args: []string{"serve"}, want: "serve"},
		{name: "version", args: []string{"version"}, want: "version"},
		{name: "migrate up", args: []string{"migrate", "up"}, want: "migrate up"},
		{name: "migrate down", args: []string{"migrate", "down"}, want: "migrate down"},
		{name: "migrate version", args: []string{"migrate", "version"}, want: "migrate version"},
		// `bakery migrate` with no subcommand is `migrate up`, because that is the
		// only thing anyone means by it.
		{name: "bare migrate defaults to up", args: []string{"migrate"}, want: "migrate up"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_URL", dsn)

			kctx, _ := parse(t, tt.args...)

			if got := kctx.Command(); got != tt.want {
				t.Errorf("got command %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDBURLIsRequired: every command that touches Postgres refuses to parse without
// a DSN, rather than booting and discovering it on the first request.
func TestDBURLIsRequired(t *testing.T) {
	for _, args := range [][]string{
		{"serve"},
		{"migrate", "up"},
		{"migrate", "version"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var cli CLI

			parser, err := kong.New(&cli, kong.Name("bakery"))
			if err != nil {
				t.Fatalf("build parser: %v", err)
			}

			if _, err := parser.Parse(args); err == nil {
				t.Errorf("%v parsed with no DB_URL", args)
			}
		})
	}
}

// TestVersionNeedsNoDatabase: `bakery version` must work on a machine with no
// Postgres and no configuration at all.
func TestVersionNeedsNoDatabase(t *testing.T) {
	kctx, _ := parse(t, "version")

	if got := kctx.Command(); got != "version" {
		t.Errorf("got command %q, want %q", got, "version")
	}
}
