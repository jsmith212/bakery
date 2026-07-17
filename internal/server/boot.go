package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/bazel"
	"github.com/jsmith212/bakery/internal/cache/hashserv"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

const (
	// grpcMaxMsgSize is 4x the advertised max_batch_total_size_bytes (4 MiB). moon's
	// worst-case BatchUpdateBlobs at B=4 MiB is 4,097,613 bytes -- inside grpc-go's 4 MiB
	// default by only 2.3%, which is not a margin. 16 MiB gives real headroom for both the
	// recv (BatchUpdateBlobs) and send (BatchReadBlobs) directions without risking the
	// 64 MiB ceiling moon's own max_decoding_message_size imposes.
	grpcMaxMsgSize = 16 << 20
)

const (
	// keyTouchInterval is how often the coalescing flusher writes api_keys.last_used_at.
	// It is deliberately coarse: the sstate HEAD storm validates the same key
	// thousands of times a second, and an UPDATE per validation would put a write
	// on the hottest read path in the system.
	keyTouchInterval = time.Minute

	// sessionCleanupInterval sweeps expired session rows.
	sessionCleanupInterval = time.Hour
)

// BootParams is what a `bakery serve` needs from outside the config: the embedded
// frontend and the build version.
type BootParams struct {
	Cmd     config.ServeCmd
	Version string

	// Dist is the embedded SPA. It is IGNORED when Cmd.Headless is set -- main
	// still passes it, because "was the frontend embedded" and "does this
	// deployment serve a console" are different questions and the second one is
	// not the embedder's business.
	Dist fs.FS

	// Ready is called once all three listeners are bound, with the addresses they got.
	// See Config.Ready: it is how a caller that asked for port 0 learns its port.
	Ready func(public, grpcAddr, metricsAddr net.Addr)
}

// bootstrapMaxConns sizes the bootstrap pool: one connection pinned by BootLock
// for the process lifetime, one for the migrations, one spare.
const bootstrapMaxConns = 3

// Boot brings up a full server: pool, boot lock, migrations, metrics, auth, the
// control-plane API, and both listeners. It returns when the server has drained.
//
// The ORDER below is load-bearing:
//
//  1. Connect (and PING -- pgxpool.New alone does not connect, so an unpinged pool
//     boots green against a dead database).
//  2. Take the boot lock. BEFORE migrations, so two instances starting together
//     cannot race each other through the same migration.
//  3. Migrate.
//  4. Open the SERVING pool -- it registers the enum types, which only exist
//     once (3) has run.
//  5. Everything else.
func Boot(ctx context.Context, p BootParams) error {
	cmd := p.Cmd
	log := slog.Default()

	// The BOOTSTRAP pool: no enum type registration, because on a fresh database
	// the enum types do not exist yet. A pool that registered them would fail its
	// own Ping here and the migrations that create them could never run.
	//
	// It outlives this function: BootLock pins one of its connections for the
	// process lifetime, so closing it would drop the advisory lock. It serves no
	// queries -- the serving pool below does.
	bootPool, err := db.NewBootstrapPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: bootstrapMaxConns})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	defer bootPool.Close()

	// The advisory lock is what makes the in-process route cache and the LRU sound:
	// they are only coherent if exactly one process writes this database. A second
	// instance is refused, loudly, unless the operator has explicitly asserted that
	// they know what they are doing.
	lock, err := db.AcquireBootLock(ctx, bootPool)

	switch {
	case errors.Is(err, db.ErrLocked) && cmd.AllowMultiInstance:
		log.Warn("another instance holds the boot lock; continuing because --allow-multi-instance is set")
	case err != nil:
		return fmt.Errorf("acquire boot lock: %w", err)
	default:
		defer lock.Release()
	}

	if err := db.Migrate(bootPool); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	// Only NOW can the serving pool exist: its AfterConnect loads and registers
	// every enum type, and it refuses a connection where one is missing. Before
	// the migrations above, all seven were missing.
	pool, err := db.NewPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: 0})
	if err != nil {
		return fmt.Errorf("open serving pool: %w", err)
	}

	defer pool.Close()

	version, dirty, applied, err := db.MigrationVersion(pool)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	log.Info("schema ready", "version", version, "dirty", dirty, "applied", applied)

	m := metrics.New()
	if err := m.RegisterPool(pool); err != nil {
		return fmt.Errorf("register pool collector: %w", err)
	}

	// The byte store. Constructing it here is load-bearing even though no cache
	// backend reads it yet in M1: NewLocal creates and probes --storage-dir, so a
	// typo'd or unwritable path is a LOUD boot failure rather than an EACCES the
	// first time M2 tries to write an object into a directory that was never
	// there. The instrumented decorator both times every future storage call and
	// makes bakery_storage_operations_total exist from boot.
	localStore, err := storage.NewLocal(cmd.StorageDir)
	if err != nil {
		return fmt.Errorf("prepare storage directory: %w", err)
	}

	byteStore := storage.NewInstrumented(localStore, m, metrics.DriverLocal)

	log.Info("storage ready", "driver", metrics.DriverLocal, "root", localStore.Root())

	store := db.NewStore(pool)

	authSvc, sessions, err := buildAuth(ctx, cmd, store, m, log)
	if err != nil {
		return err
	}

	// Boot-time reconciliation, in this order: the orgs named by the group map must
	// exist before any login can be reconciled into them, and the dev seed needs
	// its own org.
	if err := authSvc.EnsureOrgs(ctx); err != nil {
		return fmt.Errorf("ensure orgs from the group map: %w", err)
	}

	if err := authSvc.SeedDevLogin(ctx); err != nil {
		return fmt.Errorf("seed dev login: %w", err)
	}

	if authSvc.DevLoginEnabled() {
		log.Warn("DEV LOGIN IS ENABLED: POST /api/v1/auth/dev-login mints a site-admin session " +
			"with no credential. This is a total authentication bypass. Never run this in production.")
	}

	apiSrv, err := api.New(api.Config{
		Store: store, Auth: authSvc, Metrics: m, Log: log,
		AllowSelfServeOrgs:   cmd.AllowSelfServeOrgs,
		AllowLocalSiteAdmins: cmd.AllowLocalSiteAdmins,
	})
	if err != nil {
		return fmt.Errorf("build api: %w", err)
	}

	// The blob service: the ONE writer of object metadata and the only door to the
	// bytes. Its Reader/Txer are the same *db.Store; dedup, refcounts, the LRU,
	// singleflight and the headline cache metrics all live behind it.
	blobs, err := blob.New(blob.Config{
		Reader:  store,
		Tx:      store,
		Storage: byteStore,
		Metrics: m,
	})
	if err != nil {
		return fmt.Errorf("build blob service: %w", err)
	}

	// The M2 cache backends: sstate and downloads, both the shared httpblob handler
	// with different Policy. The route resolver fronts ResolveRoute + GetBackend with
	// the in-process cache the boot advisory lock makes sound; the auth adapter widens
	// auth.Principal to httpblob's narrow capability interface.
	cacheDeps := cache.Deps{Blobs: blobs, Metrics: m, Logger: log}
	if err := cacheDeps.Validate(); err != nil {
		return fmt.Errorf("build cache deps: %w", err)
	}

	routes := httpblob.NewCachedResolver(store, log)
	authn := cacheAuth{svc: authSvc}

	// M3: hashserv. It shares the route resolver and the auth service with the M2
	// backends, but nothing else -- it is the one backend that does not route through
	// blob.Service, so it takes the Store directly (a narrow, hashserv-only Queries
	// surface) and owns its own metrics.
	//
	// Upstreams is lazy: no upstream is dialled until a backend that configures one takes
	// its first miss, so a dead or slow third party can never stall boot. Its Close tears
	// down the pooled upstream connections and the backfill workers.
	upstreams := hashserv.NewUpstreams(store, m, log, cmd.HashservDisableUpstream)

	// Run below BLOCKS until both listeners have drained, so this defer really does fire
	// at shutdown rather than at the top of a still-serving process.
	defer func() {
		if err := upstreams.Close(); err != nil {
			log.Warn("closing hashserv upstreams", "error", err)
		}
	}()

	if cmd.HashservDisableUpstream {
		log.Warn("hashserv upstream chaining is disabled server-wide: every backend's configured " +
			"upstream is ignored")
	}

	// M4: the Bazel REAPI. bazel.New OWNS the /ac and /cas httpblob mounts (it constructs
	// them internally); sccache is a sibling httpblob backend on its own WebDAV route and
	// namespace. Both are cache.Backends and mount in headless mode too -- "no console" is
	// not "no cache". The bazel backend ALSO implements cache.GRPCBackend, which the loop
	// below detects and registers on the gRPC server.
	//
	// The gRPC server is constructed unconditionally so RegisterGRPC always runs; the
	// listener that serves it only binds when --grpc-addr is non-empty (see server.New).
	grpcSrv := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpcMaxMsgSize),
		grpc.MaxSendMsgSize(grpcMaxMsgSize),
	)

	cacheBackends := []cache.Backend{
		httpblob.NewSstate(cacheDeps, routes, authn),
		httpblob.NewDownloads(cacheDeps, routes, authn),
		hashserv.New(cacheDeps, routes, hashservAuth{svc: authSvc}, store, upstreams),
		bazel.New(cacheDeps, routes, authn, bazelAuth{svc: authSvc}),
		httpblob.NewSccache(cacheDeps, routes, authn),
	}

	// Any backend that speaks gRPC registers its services here. Today only bazel does; the
	// loop is the seam so a future gRPC backend needs no boot change. A registration
	// failure is a wrapped boot error, never a panic -- a half-registered gRPC server is
	// not a state we serve from.
	for _, backend := range cacheBackends {
		gb, ok := backend.(cache.GRPCBackend)
		if !ok {
			continue
		}

		if err := gb.RegisterGRPC(grpcSrv); err != nil {
			return fmt.Errorf("register grpc backend %q: %w", backend.Kind(), err)
		}
	}

	// Background maintenance. Both are tied to ctx, so they stop when the server
	// does; both are loops, so both must be goroutines.
	go authSvc.StartKeyToucher(ctx, keyTouchInterval)
	go sessions.StartCleanup(ctx, sessionCleanupInterval)

	log.Info("bakery ready",
		"version", p.Version,
		"address", cmd.Addr(),
		"grpc", cmd.GRPCAddr,
		"metrics", cmd.MetricsAddr,
		"headless", cmd.Headless,
		"console", !cmd.Headless,
		"oidc", cmd.OIDCIssuer != "",
	)

	return New(Config{
		Addr:          cmd.Addr(),
		Version:       p.Version,
		MetricsAddr:   cmd.MetricsAddr,
		GRPC:          grpcSrv,
		GRPCAddr:      cmd.GRPCAddr,
		Dist:          p.Dist,
		Headless:      cmd.Headless,
		API:           apiSrv.Handler(),
		Pool:          pool,
		Metrics:       m,
		Storage:       byteStore,
		CacheBackends: cacheBackends,
		Ready:         p.Ready,
	}).Run(ctx)
}

// cacheAuth adapts *auth.Service to httpblob.Authenticator. It exists only to widen
// auth.Principal (the sealed, unforgeable identity) to httpblob.Principal (the narrow
// CanReadProject/CanWriteProject surface the cache handler needs) -- an auth.Principal
// satisfies httpblob.Principal structurally, so this is a pure type-widening delegate.
// The token stays inside internal/auth; nothing here touches it.
type cacheAuth struct{ svc *auth.Service }

func (a cacheAuth) AuthenticateCache(ctx context.Context, r *http.Request) (httpblob.Principal, error) {
	return a.svc.AuthenticateCache(ctx, r)
}

// hashservAuth adapts *auth.Service to hashserv.Authenticator. Same pure type-widening
// delegate as cacheAuth above, and it exists for the same reason -- but it wraps
// AuthenticateToken, not AuthenticateCache, because the hashserv credential does not
// arrive in an *http.Request.
//
// It cannot: a stock bitbake client sends NO Authorization header on the WebSocket
// upgrade (asyncrpc/client.py calls websockets.connect with no headers). The token
// arrives IN BAND, in the `auth` RPC, after the upgrade has already completed -- so by
// the time there is a credential to check there is no request left to check it against.
// AuthenticateToken is the same authenticateKey the Basic path runs, with the HTTP
// peeled off; the widening here is identical.
type hashservAuth struct{ svc *auth.Service }

func (a hashservAuth) AuthenticateToken(ctx context.Context, token string) (hashserv.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// bazelAuth adapts *auth.Service to bazel.Authenticator. Same pure type-widening delegate
// as hashservAuth: the REAPI credential arrives in gRPC "authorization" metadata, not an
// *http.Request, so the gRPC handlers need AuthenticateToken (the same constant-time,
// zero-join, index-only key probe the Basic path runs), widened from auth.Principal to
// bazel's narrow CanReadProject/CanWriteProject surface. The /ac and /cas HTTP mounts the
// bazel backend owns keep using cacheAuth (AuthenticateCache) -- this is only for gRPC.
type bazelAuth struct{ svc *auth.Service }

func (a bazelAuth) AuthenticateToken(ctx context.Context, token string) (bazel.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// buildAuth assembles the auth service: the pgxpool-backed session store, the
// OIDC provider (when an issuer is configured), and the group -> org map.
//
// A malformed group map is a BOOT FAILURE, not a warning. It carries the login gate
// and the OIDC half of the site and org roles, so it IS the claim-derived half of the
// authorization policy; booting with HALF of it would silently admit users at the
// wrong role or refuse them all.
//
// Booting with NONE of it is fine and supported: with no group map, any successful
// OIDC auth is admitted (the login gate is empty) and every role is an in-app grant.
// That is a deployment choice, not a misconfiguration -- which is exactly why the
// malformed case must be distinguished from the absent one and hard-fail.
func buildAuth(
	ctx context.Context,
	cmd config.ServeCmd,
	store *db.Store,
	m *metrics.Metrics,
	log *slog.Logger,
) (*auth.Service, *auth.SessionStore, error) {
	sessionStore := auth.NewSessionStore(store.Pool(), log)

	// Secure cookies everywhere except a dev-login deployment, which is by
	// definition local and by definition plaintext http://localhost -- where a
	// Secure cookie is simply never sent back and every login silently fails.
	// DEV_LOGIN_ENABLED is already the "this is not production" switch; it does not
	// need a second one that could be set independently and wrongly.
	sessions := auth.NewSessionManager(sessionStore, !cmd.DevLoginEnabled)

	var provider *auth.Provider

	if cmd.OIDCIssuer != "" {
		p, err := auth.NewProvider(ctx, auth.OIDCConfig{
			Issuer:        cmd.OIDCIssuer,
			ClientID:      cmd.OIDCClientID,
			ClientSecret:  cmd.OIDCClientSecret,
			RedirectURL:   cmd.OIDCRedirectURL,
			Scopes:        cmd.OIDCScopes,
			GroupsClaim:   "",
			DeviceAuthURL: "",
		})
		if err != nil {
			return nil, nil, fmt.Errorf("configure oidc provider: %w", err)
		}

		provider = p

		if !provider.HasDeviceGrant() {
			// Said at boot rather than discovered by a developer whose `bakery login`
			// hangs: the device endpoint is RFC 8414 metadata and legitimately
			// optional, so a provider without one is misconfiguration, not a bug.
			log.Warn("the OIDC provider advertises no device authorization endpoint: " +
				"`bakery login` cannot work against it")
		}
	}

	var groups *config.GroupMap

	if cmd.GroupMapFile != "" {
		g, err := config.LoadGroupMap(cmd.GroupMapFile)
		if err != nil {
			return nil, nil, fmt.Errorf("load group map: %w", err)
		}

		groups = g
	}

	if provider == nil && !cmd.DevLoginEnabled {
		// Not fatal -- the API still answers, and a headless deployment fronted by
		// something else may want exactly this -- but nobody can log in, so say so.
		log.Warn("no OIDC issuer is configured and dev login is off: nobody can obtain a session")
	}

	svc, err := auth.New(auth.Deps{
		Store:    store,
		Sessions: sessions,
		Provider: provider,
		Groups:   groups,
		Metrics:  m,
		Log:      log,
		DevLogin: cmd.DevLoginEnabled,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build auth service: %w", err)
	}

	return svc, sessionStore, nil
}
