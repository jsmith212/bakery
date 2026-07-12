// Package config defines the Bakery command tree and its configuration.
package config

import (
	"net"
	"strconv"
)

// CLI is the root command tree. Kong parses argv and the environment into it.
//
// Later milestones hang their commands here (sstate push, gc, login); each is a
// struct with a `cmd:""` tag and a case in main's dispatch.
type CLI struct {
	Serve   ServeCmd   `cmd:"" help:"Run the Bakery cache server."`
	Migrate MigrateCmd `cmd:"" help:"Apply or inspect the database schema."`
	Version VersionCmd `cmd:"" help:"Print the Bakery version and exit."`
}

// DBFlags is the database connection, shared by every command that needs one.
//
// One DSN, not decomposed host/port/user/password: the password belongs in the
// connection string, the connection string belongs in the environment, and
// stack.env is gitignored.
type DBFlags struct {
	DBURL string `env:"DB_URL" help:"Postgres connection string." required:""`
}

// OIDCFlags is the identity provider. Secrets come from the environment; the
// nested group -> org mapping comes from a FILE, because it is a nested document
// and an env var is a bad place for one.
type OIDCFlags struct {
	OIDCIssuer       string `env:"OIDC_ISSUER"        help:"OIDC issuer URL. Discovery is fetched from {issuer}/.well-known/openid-configuration."`
	OIDCClientID     string `env:"OIDC_CLIENT_ID"     help:"OIDC client ID."`
	OIDCClientSecret string `env:"OIDC_CLIENT_SECRET" help:"OIDC client secret."`
	OIDCRedirectURL  string `env:"OIDC_REDIRECT_URL"  help:"Redirect URL registered with the provider, e.g. https://bakery.example.com/api/v1/auth/callback."`

	// offline_access buys the refresh token the CLI's device grant needs; groups is
	// what the site role and every org role are derived from.
	OIDCScopes []string `default:"openid,profile,email,groups,offline_access" env:"OIDC_SCOPES" help:"Scopes requested on the browser and device flows."`

	// The group -> org mapping, parsed and validated by LoadGroupMap.
	//
	// Org and site roles are 100% claim-derived and reconciled on EVERY login, so
	// this file IS the authorization policy. A malformed one is a boot failure, not
	// a warning.
	GroupMapFile string `env:"GROUP_MAP_FILE" help:"Path to the JSON group-to-org mapping file." type:"path"`
}

// ServeCmd configures the server.
//
// The env tags are explicit rather than derived from a Kong env prefix, so the
// variable names here are exactly the names in stack.env.tmpl.
type ServeCmd struct {
	DBFlags
	OIDCFlags

	Host string `env:"HOST" help:"Interface to bind. Empty binds every interface."`
	Port int    `default:"8080" env:"PORT" help:"Port to listen on."`

	// A SEPARATE listener, on LOOPBACK by default. /metrics exposes every org and
	// project slug and their stored byte counts, so putting it on the public
	// listener would hand the whole tenant list to anyone who can reach the cache.
	// Exposing it has to be an explicit act.
	MetricsAddr string `default:"127.0.0.1:9090" env:"METRICS_ADDR" help:"Address for the private metrics listener."`

	// API + metrics, no SPA. For a deployment that fronts the console elsewhere, or
	// does not want one.
	Headless bool `env:"HEADLESS" help:"Serve the API and metrics but not the web console."`

	// Boot takes pg_try_advisory_lock and REFUSES to start a second instance.
	//
	// That refusal is what makes the in-process route cache sound and the GC's
	// single-writer assumption true. This flag does not make a second instance
	// correct; it makes it your problem. It exists for a controlled rolling deploy,
	// not for scale-out.
	AllowMultiInstance bool `env:"ALLOW_MULTI_INSTANCE" help:"Boot even if another instance holds the database boot lock. You are asserting that only one instance writes."`

	// Local disk. S3 is explicitly deferred: there is no storage-backend column in
	// the schema and no S3 driver in the binary.
	StorageDir string `default:"./data" env:"STORAGE_DIR" help:"Directory the local storage driver writes blobs to." type:"path"`

	// DEV_LOGIN_ENABLED is reachable ONLY from here -- this flag or its env var.
	//
	// There is deliberately no UI control, no API endpoint and no database column
	// that can turn it on. It mints a session for a synthetic site admin with no
	// credential, so any path that could enable it at runtime would be a total
	// authentication bypass. Default off, and it stays a boot-time-only decision.
	DevLoginEnabled bool `env:"DEV_LOGIN_ENABLED" help:"Seed a dev site admin and expose an unauthenticated dev-login endpoint. Never enable this in production."`
}

// MigrateCmd groups the schema subcommands.
//
// Migrations are ALSO applied at boot, so this is not the only way the schema moves
// -- it is for operating on the schema without starting a server: a rollback, or a
// migrate step in a deploy pipeline that runs before the new binary rolls out.
type MigrateCmd struct {
	Up      MigrateUpCmd      `cmd:"" default:"withargs" help:"Apply every pending migration."`
	Down    MigrateDownCmd    `cmd:""                    help:"Roll every migration back. Destructive."`
	Version MigrateVersionCmd `cmd:""                    help:"Print the applied schema version."`
}

// MigrateUpCmd applies every pending migration.
type MigrateUpCmd struct {
	DBFlags
}

// MigrateDownCmd rolls every migration back.
//
// This DROPS EVERY TABLE, so it demands an explicit --yes. A `migrate down` typo'd
// at a production DSN is not something an interactive "are you sure" can save you
// from on a CI runner, where there is nobody to ask.
type MigrateDownCmd struct {
	DBFlags

	Yes bool `help:"Required. Confirms that dropping every table in this database is intended." name:"yes"`
}

// MigrateVersionCmd prints the applied schema version.
type MigrateVersionCmd struct {
	DBFlags
}

// VersionCmd takes no configuration.
type VersionCmd struct{}

// Addr renders the listen address for net.Listen.
func (c ServeCmd) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
