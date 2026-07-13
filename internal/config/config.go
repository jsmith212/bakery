// Package config defines the Bakery command tree and its configuration.
package config

import (
	"net"
	"strconv"
	"time"
)

// CLI is the root command tree. Kong parses argv and the environment into it.
//
// Later milestones hang their commands here (sstate push, gc, login); each is a
// struct with a `cmd:""` tag and a case in main's dispatch.
type CLI struct {
	Serve   ServeCmd   `cmd:"" help:"Run the Bakery cache server."`
	Migrate MigrateCmd `cmd:"" help:"Apply or inspect the database schema."`
	Version VersionCmd `cmd:"" help:"Print the Bakery version and exit."`

	// The same binary is the API client. There is no second `bakeryctl` to ship,
	// version-skew or forget to update: the client speaks the wire types the server
	// it was built from defines.
	Login   LoginCmd   `cmd:"" help:"Sign in to a Bakery server with the OIDC device grant."`
	Logout  LogoutCmd  `cmd:"" help:"Clear the cached tokens for a Bakery server."`
	Whoami  WhoamiCmd  `cmd:"" help:"Print who you are signed in as, and what you may do."`
	Org     OrgCmd     `cmd:"" help:"Manage organizations."`
	Project ProjectCmd `cmd:"" help:"Manage projects."`
	Member  MemberCmd  `cmd:"" help:"Manage project memberships."`
	Key     KeyCmd     `cmd:"" help:"Manage project API keys."`

	// Server is global rather than per-command because it is the one thing every
	// client command needs and no client command chooses: it is a property of the
	// installation, and it belongs in the environment of a shell that talks to one.
	Server string `default:"http://localhost:8080" env:"BAKERY_SERVER" help:"Bakery server to talk to." name:"server"`

	// JSON is the machine-readable escape hatch. The default output is for a human
	// reading a terminal; this is for the pipeline that comes after.
	JSON bool `env:"BAKERY_JSON" help:"Print the server's JSON instead of a table." name:"json"`
}

// LoginCmd runs the OIDC device grant.
//
// It takes no flags: everything the flow needs -- issuer, client id, scopes, the
// device authorization endpoint -- is fetched from the server's /auth/config, so
// the CLI cannot disagree with the server about which identity provider it
// trusts, and a workstation needs no configuration beyond --server.
type LoginCmd struct{}

// LogoutCmd clears the cached tokens for --server.
type LogoutCmd struct{}

// WhoamiCmd is GET /me.
type WhoamiCmd struct{}

// OrgCmd groups the organization verbs.
type OrgCmd struct {
	List   OrgListCmd   `cmd:"" help:"List the organizations you can see."`
	Create OrgCreateCmd `cmd:"" help:"Create an organization. Site admins only."`
	Show   OrgShowCmd   `cmd:"" help:"Show one organization."`
	Rename OrgRenameCmd `cmd:"" help:"Change an organization's display name."`
	Delete OrgDeleteCmd `cmd:"" help:"Delete an organization and everything in it."`
}

// OrgListCmd lists the orgs the caller can see.
type OrgListCmd struct{}

// OrgCreateCmd creates an organization.
type OrgCreateCmd struct {
	Slug string `arg:""  help:"URL slug. Becomes the first path segment of every cache URL under this org."`
	Name string `help:"Display name. Defaults to the slug."`
}

// OrgShowCmd shows one organization.
type OrgShowCmd struct {
	Org string `arg:"" help:"Organization slug."`
}

// OrgRenameCmd changes an organization's display name.
//
// There is no `org move`: the SLUG is immutable, because it is the first path
// segment of every cache URL and a rename would silently break every configured
// BitBake, Bazel and Docker client pointed at it.
type OrgRenameCmd struct {
	Org  string `arg:"" help:"Organization slug."`
	Name string `arg:"" help:"New display name."`
}

// OrgDeleteCmd deletes an organization.
type OrgDeleteCmd struct {
	Org string `arg:"" help:"Organization slug."`
	Yes bool   `help:"Required. Confirms that deleting this org's projects, keys and cached objects is intended." name:"yes"`
}

// ProjectCmd groups the project verbs.
type ProjectCmd struct {
	List   ProjectListCmd   `cmd:"" help:"List an organization's projects."`
	Create ProjectCreateCmd `cmd:"" help:"Create a project."`
	Show   ProjectShowCmd   `cmd:"" help:"Show one project."`
	Rename ProjectRenameCmd `cmd:"" help:"Change a project's display name."`
	Delete ProjectDeleteCmd `cmd:"" help:"Delete a project and everything in it."`
}

// ProjectListCmd lists an organization's projects.
type ProjectListCmd struct {
	Org string `arg:"" help:"Organization slug."`
}

// ProjectCreateCmd creates a project.
type ProjectCreateCmd struct {
	Org  string `arg:"" help:"Organization slug."`
	Slug string `arg:"" help:"URL slug. Becomes the second path segment of every cache URL for this project."`
	Name string `help:"Display name. Defaults to the slug."`
}

// ProjectShowCmd shows one project.
type ProjectShowCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
}

// ProjectRenameCmd changes a project's display name. The slug is immutable.
type ProjectRenameCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Name    string `arg:"" help:"New display name."`
}

// ProjectDeleteCmd deletes a project.
type ProjectDeleteCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Yes     bool   `help:"Required. Confirms that deleting this project's keys and cached objects is intended." name:"yes"`
}

// MemberCmd groups the membership verbs.
//
// Note what is missing: there is no `member set` for an ORG role. Org and site
// roles are derived from OIDC group claims and reconciled on every login, so a
// hand-edit would either evaporate at the user's next login or -- worse, in the
// granting direction -- confer authority the IdP never granted, with the audit
// trail living in Bakery rather than in the IdP where the access review looks.
// The API refuses those writes with a 409; the CLI does not offer them.
type MemberCmd struct {
	List   MemberListCmd   `cmd:"" help:"List an organization's or a project's members."`
	Set    MemberSetCmd    `cmd:"" help:"Grant or change someone's project role."`
	Remove MemberRemoveCmd `cmd:"" help:"Remove someone's project role."`
}

// MemberListCmd lists members. With no project, it lists the org's roster and
// their claim-derived org roles.
type MemberListCmd struct {
	Org     string `arg:""             help:"Organization slug."`
	Project string `arg:"" optional:"" help:"Project slug. Omit to list the organization's members."`
}

// MemberSetCmd grants or changes a project role.
//
// A DOWNGRADE also revokes the API keys that now exceed the new role, in the same
// transaction on the server. That is not a courtesy: key validation deliberately
// never re-checks the membership table (it would be a second database probe on
// the sstate HEAD storm), so a key's scope is capped at grant time and never
// re-examined. Without the revoke, a demoted writer keeps write access forever.
type MemberSetCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	User    string `arg:"" help:"User email or id."`
	Role    string `arg:"" enum:"reader,writer,admin" help:"reader, writer or admin."`
}

// MemberRemoveCmd removes a project role.
type MemberRemoveCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	User    string `arg:"" help:"User email or id."`
}

// KeyCmd groups the API-key verbs.
type KeyCmd struct {
	List   KeyListCmd   `cmd:"" help:"List a project's API keys. Metadata only; a token is never listed."`
	Create KeyCreateCmd `cmd:"" help:"Mint an API key. The token is shown once and never again."`
	Revoke KeyRevokeCmd `cmd:"" help:"Revoke an API key."`
}

// KeyListCmd lists a project's keys.
type KeyListCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
}

// KeyCreateCmd mints an API key for the CALLER.
//
// There is deliberately no --user flag. Keys are per-user as well as
// project-scoped, and a key you minted for someone else would carry your identity
// in the audit trail while sitting in their CI config. The API has no field to
// ask for one.
type KeyCreateCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Name    string `arg:"" help:"A label, so the key can be told apart later."`

	Scope string `default:"read" enum:"read,write" help:"read or write. Capped at what your project role allows." name:"scope"`

	// A duration, not a date: `--expires-in 720h` is what a human types, and it
	// cannot be off by a timezone.
	ExpiresIn time.Duration `help:"Lifetime, e.g. 720h. Omit for a key that never expires." name:"expires-in"`
}

// KeyRevokeCmd revokes an API key.
type KeyRevokeCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Key     string `arg:"" help:"Key id, as shown by bakery key list."`
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

	// Self-serve orgs: ANY signed-in human may create an organization, and becomes
	// its LOCAL OWNER (see internal/api's handleCreateOrg). Default ON.
	//
	// Off restricts creation to site admins. It does NOT restore the M1 dead-end:
	// the creator still gets the owner grant, because an org whose creator holds no
	// membership in it is an org nobody can ever join.
	//
	// An API key can never reach the endpoint whatever this says -- the route is
	// AccessUser, which the guard admits no key to. A delegation must not become a
	// master key, least of all the master of a brand-new tenant.
	AllowSelfServeOrgs bool `default:"true" env:"ALLOW_SELF_SERVE_ORGS" help:"Let any signed-in user create an organization. They become its owner. Off restricts creation to site admins." negatable:""`

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
