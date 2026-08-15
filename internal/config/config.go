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
	Login     LoginCmd     `cmd:"" help:"Sign in to a Bakery server with the OIDC device grant."`
	Logout    LogoutCmd    `cmd:"" help:"Clear the cached tokens for a Bakery server."`
	Whoami    WhoamiCmd    `cmd:"" help:"Print who you are signed in as, and what you may do."`
	Org       OrgCmd       `cmd:"" help:"Manage organizations."`
	Project   ProjectCmd   `cmd:"" help:"Manage projects."`
	Member    MemberCmd    `cmd:"" help:"Manage project memberships."`
	Key       KeyCmd       `cmd:"" help:"Manage project API keys."`
	Sstate    SstateCmd    `cmd:"" help:"Push a local Yocto sstate cache to a Bakery server."`
	Downloads DownloadsCmd `cmd:"" help:"Push a local Yocto downloads (source premirror) directory."`
	GC        GCCmd        `cmd:"" help:"Trigger and inspect GC sweeps. Site admins only." name:"gc"`

	// User is the ONLY command group that does not go through the API. It needs
	// DB_URL and it speaks to Postgres directly -- see UserCmd.
	User UserCmd `cmd:"" help:"Out-of-band user administration, straight against the database."`

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

// MemberCmd groups the membership verbs. `set` and `remove` are PROJECT roles.
//
// Note what is missing: there is no `member set` for an ORG role, and since M1.5
// that is a gap in the CLI rather than a property of the model. The API grants org
// membership in-app now (PUT /orgs/{org}/members/{user} writes the LOCAL half), and
// the console is the surface for it. The CLI simply has not grown the verb yet.
//
// What has NOT changed, and is the reason it is safe to add: an org role is HYBRID,
// so a hand-edit here writes local_role and nothing else. It cannot forge a claim,
// and the next login cannot evaporate it -- the reconciler owns the oidc_* columns
// and only those. Site roles are the same shape (see `bakery user site-admin`).
type MemberCmd struct {
	List   MemberListCmd   `cmd:"" help:"List an organization's or a project's members."`
	Set    MemberSetCmd    `cmd:"" help:"Grant or change someone's project role."`
	Remove MemberRemoveCmd `cmd:"" help:"Remove someone's project role."`
}

// MemberListCmd lists members. With no project, it lists the org's roster and their
// EFFECTIVE org roles -- greatest(oidc_role, local_role), whichever source is behind
// them.
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

// SstateCmd groups the sstate verbs. The only one is push: reads are BitBake's, and it
// speaks the HTTP mirror protocol directly.
type SstateCmd struct {
	Push SstatePushCmd `cmd:"" help:"Upload a local sstate cache's missing objects to a Bakery server."`
}

// SstatePushCmd walks a local SSTATE_DIR and PUTs the objects the server is missing.
//
// It HEADs every object first and uploads only the misses, so a warm cache is a cheap
// no-op. The default credential is the logged-in session (no --key needed); --key or
// BAKERY_API_KEY is the CI override, presented as HTTP Basic exactly as BitBake reads.
type SstatePushCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Dir     string `arg:"" help:"Local SSTATE_DIR to walk." type:"existingdir"`

	Concurrency int    `default:"8" help:"Parallel HEAD/PUT operations." name:"concurrency" short:"j"`
	Key         string `env:"BAKERY_API_KEY" help:"API key (bkry_...). Omit to use the logged-in session." name:"key"`
	DryRun      bool   `help:"Report what would upload; PUT nothing." name:"dry-run"`
}

// DownloadsCmd groups the downloads (source premirror) verbs.
type DownloadsCmd struct {
	Push DownloadsPushCmd `cmd:"" help:"Upload a local downloads directory's missing files to a Bakery server."`
}

// DownloadsPushCmd walks the top level of a local DL_DIR and PUTs the missing files. It
// is the sstate engine over a flat walk: subdirectories (git2/ and other VCS mirror
// trees) and .done / .lock / .tmp control files are skipped.
type DownloadsPushCmd struct {
	Org     string `arg:"" help:"Organization slug."`
	Project string `arg:"" help:"Project slug."`
	Dir     string `arg:"" help:"Local DL_DIR to walk." type:"existingdir"`

	Concurrency int    `default:"8" help:"Parallel HEAD/PUT operations." name:"concurrency" short:"j"`
	Key         string `env:"BAKERY_API_KEY" help:"API key (bkry_...). Omit to use the logged-in session." name:"key"`
	DryRun      bool   `help:"Report what would upload; PUT nothing." name:"dry-run"`
}

// GCCmd groups the M6 operator-surface verbs (spec §9.10): GET /gc/runs[, /{id}]
// and POST /gc/run, all AccessSiteAdmin on the server. Like every command in this
// group except `user site-admin`, this is an HTTP CLIENT, never DB-direct -- a
// sweep is server-side machinery (the process-local LRU invalidation and the
// single-writer boot advisory lock both make running one from a second process
// wrong), so there is nothing here that could run out-of-band even in principle.
type GCCmd struct {
	Run  GCRunCmd  `cmd:"" help:"Trigger a GC sweep."`
	List GCListCmd `cmd:"" help:"List recent GC runs, most recent first."`
}

// GCRunCmd triggers a sweep and, by default, returns as soon as the server has
// accepted it -- the server itself never blocks the request on the sweep
// finishing (spec §9.10), and neither does this by default. --wait opts into
// polling GET /gc/runs/{id} until the run reaches a terminal status.
type GCRunCmd struct {
	DryRun bool `help:"Read-only: report what the sweep would do; delete nothing." name:"dry-run"`
	Wait   bool `help:"Block until the run finishes, polling its status." name:"wait"`
}

// GCListCmd lists recent runs. Status is validated SERVER-SIDE (422 on an unknown
// value) rather than with a Kong enum: the closed vocabulary is the API's to own,
// and duplicating it here is exactly the kind of drift CLAUDE.md's protocol docs
// warn about -- two places asserting the same closed set eventually disagree.
type GCListCmd struct {
	Status string `help:"Filter by status: running, succeeded or failed. Omit for every status." name:"status"`
	Limit  int    `default:"20" help:"Maximum runs to list." name:"limit"`
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

	// offline_access buys the refresh token the CLI's device grant needs.
	//
	// `groups` carries the login gate and the OIDC half of the site and org roles.
	// It is not optional even for a deployment that grants every role in-app: a
	// groups claim we cannot READ refuses the login (see internal/auth/reconcile.go),
	// and dropping the scope is one way to make it unreadable.
	OIDCScopes []string `default:"openid,profile,email,groups,offline_access" env:"OIDC_SCOPES" help:"Scopes requested on the browser and device flows."`

	// The group map -- login gate, site-admin groups, group -> org mapping -- parsed
	// and validated by LoadGroupMap.
	//
	// This file IS the claim-derived half of the authorization policy, so a malformed
	// one is a boot failure, not a warning. It is OPTIONAL: with no group map, any
	// successful OIDC auth is admitted and every role is an in-app grant.
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

	// A THIRD listener, for the M4 Bazel REAPI (gRPC): Bazel and moon speak
	// Capabilities/ActionCache/CAS/ByteStream here, never on the public mux. It is its
	// own listener on purpose, not a demux of the public port: grpc-go's ServeHTTP
	// transport has no Drain, so GracefulStop panics on a shared port (and only under
	// load), and a shared h2 port would put the hashserv WebSocket at the mercy of an
	// ingress flipped to h2c -- see the invariant in server.Run. Empty disables the
	// REAPI listener entirely, exactly as an empty MetricsAddr disables metrics.
	// Loopback by default; set 0.0.0.0:PORT to accept remote Bazel/moon clients.
	GRPCAddr string `default:"127.0.0.1:9092" env:"GRPC_ADDR" help:"Address for the Bazel REAPI gRPC listener. Empty disables it." name:"grpc-addr"`

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

	// The hashserv upstream-chaining KILL SWITCH. Server-wide, and it overrides every
	// backend: when set, a backend whose cache_backends.config names an upstream behaves
	// as though it named none.
	//
	// Chaining puts a THIRD PARTY inside a build's setscene burst. It is off by default
	// and opt-in per backend, but the day the public Yocto hashserv is down -- or slow,
	// which is worse, because a build stalls rather than fails -- it is showing up in
	// every customer build at once, and the fix has to be reachable by an operator who
	// is not going to run a database migration under an incident. So it is a flag, not a
	// column: restart with it set and every backend serves from local data alone, which
	// is exactly what they all did yesterday.
	//
	// It cannot turn chaining ON. There is no server-wide enable, because "chain to
	// somewhere" needs an address and the address is per-backend.
	HashservDisableUpstream bool `env:"HASHSERV_DISABLE_UPSTREAM" help:"Ignore every hashserv backend's configured upstream. The kill switch for when the public hashserv is down." name:"hashserv-disable-upstream"`

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

	// Local site-admin grants: a site admin may make another user a site admin
	// through the API, recorded with provenance. Default ON.
	//
	// Off closes that path ENTIRELY -- the endpoint 403s for everyone, site admins
	// included -- for a deployment that wants the platform-admin roster to live in the
	// directory and nowhere else. Revoking an existing local grant keeps working when
	// it is off, because turning the flag off is exactly when an operator needs to
	// clean up the grants that predate it.
	//
	// It does NOT gate `bakery user site-admin` (see UserSiteAdminCmd). That is the
	// break-glass, it needs DB_URL, and anyone holding DB_URL could UPDATE the column
	// by hand anyway -- so gating it would buy no security and would make a fresh
	// deployment with no site admin unbootstrappable, which is the very thing the
	// break-glass exists to prevent. Every grant it makes is visible in the site-admin
	// listing, with no granter named, which is itself the tell.
	AllowLocalSiteAdmins bool `default:"true" env:"ALLOW_LOCAL_SITE_ADMINS" help:"Let a site admin grant another user the site-admin role in-app. Off restricts site admins to OIDC group claims." negatable:""`

	// DEV_LOGIN_ENABLED is reachable ONLY from here -- this flag or its env var.
	//
	// There is deliberately no UI control, no API endpoint and no database column
	// that can turn it on. It mints a session for a synthetic site admin with no
	// credential, so any path that could enable it at runtime would be a total
	// authentication bypass. Default off, and it stays a boot-time-only decision.
	DevLoginEnabled bool `env:"DEV_LOGIN_ENABLED" help:"Seed a dev site admin and expose an unauthenticated dev-login endpoint. Never enable this in production."`

	// ExternalURL is the server's public base URL, e.g. "https://bakery.example.com".
	//
	// M5's Docker Bearer challenge (WWW-Authenticate: Bearer realm=...) MUST carry an
	// ABSOLUTE realm URL, and a server behind a TLS-terminating reverse proxy cannot
	// derive its own scheme or host from the request -- r.TLS is nil on the inside of
	// that proxy, so a request-derived realm says "http://" and names the internal
	// hostname. That failure reproduces in NO test that talks to the server directly
	// and in EVERY production deployment that terminates TLS at an ingress. Empty
	// falls back to the request's own scheme and Host (correct for a direct
	// connection, e.g. `bakery serve` on a laptop) -- SET THIS IN ANY DEPLOYMENT WITH
	// A PROXY IN FRONT OF IT.
	ExternalURL string `env:"EXTERNAL_URL" help:"The server's public base URL (e.g. https://bakery.example.com), used as the OCI Bearer challenge realm and as the config-snippet generator's origin. Set this when a TLS-terminating proxy sits in front of Bakery -- request-derived URLs are wrong there." name:"external-url"`

	// GRPCExternalEndpoint is the PUBLIC gRPC authority Bazel and moon should dial,
	// e.g. "grpcs://bakery.example.com:9092".
	//
	// GRPCAddr above is where the process BINDS; this is where the world REACHES it,
	// and nothing in the process can derive the second from the first. The config
	// snippet generator needs the second: a Bazel or moon client pointed at a port
	// nothing listens on does not fail the build -- it disables its remote cache,
	// logs at DEBUG, and delivers 0% hits on green builds, which is the quietest
	// possible way for a cache server to be useless.
	//
	// Empty is supported: with GRPCAddr set, the generator derives
	// {scheme}://{public host}:{GRPCAddr's port} and warns once. That guess is right
	// for the single-host deployment and for local dev, and wrong the moment an
	// ingress maps the listener somewhere else -- which is what this flag is for.
	// Empty here AND an empty GRPCAddr is a 409 on a bazel/moon snippet: the REAPI
	// listener is switched off, so there is no endpoint to name.
	GRPCExternalEndpoint string `env:"GRPC_EXTERNAL_ENDPOINT" help:"The public gRPC endpoint Bazel and moon should dial (e.g. grpcs://bakery.example.com:9092), used verbatim in generated config snippets. Set this whenever an ingress maps the REAPI listener to a different host or port." name:"grpc-external-endpoint"`

	// OCIUpstreamAuth is the SERVER-LEVEL credential map for M5's upstream registry
	// fetches: "host=user:secret", repeated. It lives here and NOT in the database for
	// the same reason as everything else that would be a plaintext secret at rest --
	// there is no encryption-at-rest facility and pgcrypto is banned outright
	// (migration 000001) -- and because an upstream credential must be REPLAYED, so it
	// cannot be hashed the way a Bakery API key is. Parsed by oci.ParseUpstreamAuth; a
	// host absent from this map is fetched anonymously, which is correct for public
	// images.
	OCIUpstreamAuth []string `env:"BAKERY_OCI_UPSTREAM_AUTH" help:"Upstream registry credential, as host=user:secret. Repeatable. A host with no entry is fetched anonymously." name:"oci-upstream-auth"`

	// OCIDisableUpstream is the M5 kill switch, mirroring HashservDisableUpstream --
	// same rationale, same shape. It does not take the mirror down: every already
	// cached manifest, tag and blob keeps serving. It stops Bakery from making any
	// NEW outbound request to an upstream registry, server-wide, overriding every
	// backend's allowlist. The day Docker Hub is slow -- which is worse than down,
	// because it stalls builds rather than failing them -- this is the flag an
	// operator reaches for without a database migration.
	OCIDisableUpstream bool `env:"BAKERY_OCI_DISABLE_UPSTREAM" help:"Ignore every OCI backend's upstream: serve only what is already cached. The kill switch for when an upstream registry is down or slow." name:"oci-disable-upstream"`

	GC GCFlags `embed:""`
}

// GCFlags is M6's knob set (spec §9.8).
//
// RETENTION SHIPS ON, and it is opinionated: migration 000012 seeded real windows
// onto existing backends, so the first sweep after an upgrade deletes everything
// colder than the window. That was an accepted product decision, and these are its
// rails -- --gc-grace-period between the metadata delete and the byte reclaim,
// `bakery gc run --dry-run` to see what a sweep would do, and
// --gc-disable-retention as the brake.
//
// There is no --gc-touch-max-pending: the toucher's pending set is the LRU itself,
// so it is bounded by the LRU's capacity for free.
type GCFlags struct {
	GCEnabled bool `default:"true" env:"GC_ENABLED" help:"Run the garbage collector. Off means cached objects and blobs accumulate without bound." name:"gc-enabled" negatable:""`

	// Six hours, not one. At the shipped pacing (1000 rows per chunk, 100ms between
	// chunks) a ten-million-row backend is about seventeen minutes of PAUSE alone, so
	// an hourly sweep would spend a large fraction of its life scanning for work that
	// a day's worth of traffic has not yet created.
	GCInterval time.Duration `default:"6h" env:"GC_INTERVAL" help:"How often the sweep runs." name:"gc-interval"`

	// Usage measurement is DECOUPLED from retention being enabled: an operator who
	// has turned retention off still needs storage and quota numbers, and a dashboard
	// that goes stale exactly when someone reaches for the brake is a dashboard that
	// lies during the incident it exists for.
	GCUsageInterval time.Duration `default:"6h" env:"GC_USAGE_INTERVAL" help:"How often backend usage is measured, even with retention disabled." name:"gc-usage-interval"`

	// THE RECOVERY WINDOW. It is frozen per run at start (gc_runs.grace_period), so
	// raising it takes effect on the NEXT run -- it cannot rescue bytes a run in
	// flight has already marked.
	GCGracePeriod time.Duration `default:"24h" env:"GC_GRACE_PERIOD" help:"How long an unreferenced blob's bytes survive after nothing names them. The recovery window." name:"gc-grace-period"`

	GCBatchSize  int           `default:"1000"  env:"GC_BATCH_SIZE"  help:"Rows per scan and per batch delete." name:"gc-batch-size"`
	GCBatchPause time.Duration `default:"100ms" env:"GC_BATCH_PAUSE" help:"Pause between chunks. The whole of the sweep's rate limiting." name:"gc-batch-pause"`

	// THE BRAKE, and it halts Layer B's MARK as well as every retention stage. The
	// incident it serves is "we deleted things we wanted" -- and those bytes are still
	// sitting inside the grace window, so leaving the mark running would convert a
	// recoverable window into permanent loss at maximum speed. Blobs ALREADY marked
	// are still reclaimed: they are past recovery, and stalling them frees nothing.
	GCDisableRetention bool `env:"GC_DISABLE_RETENTION" help:"Halt every retention stage and Layer B's mark. The brake for 'we deleted things we wanted'." name:"gc-disable-retention"`

	// The accessed_at toucher. Reads MARK in memory and this flusher coalesces the
	// marks into one batched UPDATE per (backend, namespace) per tick, because one
	// UPDATE per HEAD would funnel a BB_NUMBER_THREADS-parallel storm into a row-lock
	// convoy on the hottest rows in the database.
	GCTouchInterval  time.Duration `default:"1m" env:"GC_TOUCH_INTERVAL"  help:"How often pending accessed_at marks are flushed." name:"gc-touch-interval"`
	GCTouchStaleness time.Duration `default:"1h" env:"GC_TOUCH_STALENESS" help:"Steady-state minimum age before a key's accessed_at is rewritten. Ramped longer while the corpus is still mostly untouched." name:"gc-touch-staleness"`
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

// UserCmd is the BREAK-GLASS, and it is the only command group that never speaks
// HTTP.
//
// # The bootstrap problem it exists for
//
// With `login_groups` empty and no `site_admin_groups`, a fresh deployment has no
// site admin -- and every path to making one requires already being one. The API
// cannot solve this: an endpoint that could mint the first site admin without
// already having one would be an unauthenticated privilege-escalation endpoint, and
// gating it on "only when there are no site admins yet" is a race with the first
// person to find it.
//
// So the path out of the deadlock is deliberately NOT ON THE NETWORK. It needs
// DB_URL, which means it needs infrastructure access, not a session -- exactly the
// shape of DEV_LOGIN_ENABLED, which is settable only by env var or flag and which no
// UI or API path can reach. Reaching this requires being able to reach the database,
// and anyone who can do that could write the column by hand regardless.
type UserCmd struct {
	SiteAdmin UserSiteAdminCmd `cmd:"" help:"Grant a user the site-admin role by writing to the database. The break-glass: no API, no session." name:"site-admin"`
}

// UserSiteAdminCmd grants (or revokes) a LOCAL site-admin role, in the database.
//
// It writes site_role_local and its provenance, and NEVER site_role_oidc: no group
// claim says this, and forging one would be a lie the user's next login would
// rightly reconcile away. The local half is the half the reconciler cannot touch, so
// the grant survives every login.
//
// It leaves site_granted_by NULL -- there is no session, so there is nobody to name
// -- and that is informative rather than lossy: a site admin with a local grant and
// no granter is one that was made with database access. The site-admin listing shows
// exactly that, which is the point. This grant is not invisible; it is the opposite.
//
// --allow-local-site-admins does NOT gate this. See ServeCmd.
type UserSiteAdminCmd struct {
	DBFlags

	Email string `arg:"" help:"Email of the user. They must have signed in at least once -- users are provisioned at their first login."`

	Revoke bool `help:"Remove the local site-admin grant instead of making one. A site role held by an OIDC group claim is untouched -- remove them from the group in the identity provider." name:"revoke"`
}

// VersionCmd takes no configuration.
type VersionCmd struct{}

// Addr renders the listen address for net.Listen.
func (c ServeCmd) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
