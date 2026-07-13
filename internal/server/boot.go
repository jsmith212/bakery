package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
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

	// Ready is called once both listeners are bound, with the addresses they got.
	// See Config.Ready: it is how a caller that asked for port 0 learns its port.
	Ready func(public, metricsAddr net.Addr)
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

	// Background maintenance. Both are tied to ctx, so they stop when the server
	// does; both are loops, so both must be goroutines.
	go authSvc.StartKeyToucher(ctx, keyTouchInterval)
	go sessions.StartCleanup(ctx, sessionCleanupInterval)

	log.Info("bakery ready",
		"version", p.Version,
		"address", cmd.Addr(),
		"metrics", cmd.MetricsAddr,
		"headless", cmd.Headless,
		"console", !cmd.Headless,
		"oidc", cmd.OIDCIssuer != "",
	)

	return New(Config{
		Addr:        cmd.Addr(),
		Version:     p.Version,
		MetricsAddr: cmd.MetricsAddr,
		Dist:        p.Dist,
		Headless:    cmd.Headless,
		API:         apiSrv.Handler(),
		Pool:        pool,
		Metrics:     m,
		Storage:     byteStore,
		Ready:       p.Ready,
	}).Run(ctx)
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
