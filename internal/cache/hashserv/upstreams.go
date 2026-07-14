package hashserv

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/metrics"
)

// backendConfig is the shape hashserv reads out of cache_backends.config.
//
// Chaining is OFF unless a backend names an upstream. Absent, empty, or globally disabled all
// mean the same thing -- no upstream -- and that is the default, because putting a third
// party's latency inside a build's setscene burst is a decision someone should have to make on
// purpose.
type backendConfig struct {
	// Upstream is a ws:// or wss:// hashserv address, e.g. wss://hashserv.yoctoproject.org/ws.
	// Userinfo in the URL (wss://user:token@host/ws) is accepted and is split out into the
	// in-band `auth` RPC -- it never becomes an Authorization header, because the protocol has
	// no such thing.
	Upstream string `json:"upstream"`
}

// Upstreams builds and caches one upstream client + backfill worker per backend.
//
// Lazy, and cached: a backend's upstream is dialled on its first miss, not at boot, so a dead
// or slow third party cannot stall startup. The cache is keyed by backend id AND by the
// address, so rewriting a backend's upstream through the control plane swaps the client rather
// than pinning the old one until restart.
type Upstreams struct {
	q        Queries
	metrics  *metrics.Metrics
	log      *slog.Logger
	disabled bool

	mu    sync.Mutex
	built map[int64]*chain
}

// chain is one backend's upstream client and its backfill worker.
type chain struct {
	addr string
	up   *Upstream
	bf   *Backfiller
}

// NewUpstreams builds the provider. disabled is the server-wide kill switch: when set, every
// backend behaves as though it had no upstream, whatever its config says. That switch exists
// for the day the public hashserv is down and it is showing up in customer builds -- it must
// not require a database migration to pull.
func NewUpstreams(q Queries, m *metrics.Metrics, log *slog.Logger, disabled bool) *Upstreams {
	if log == nil {
		log = slog.Default()
	}

	return &Upstreams{q: q, metrics: m, log: log, disabled: disabled, built: map[int64]*chain{}}
}

// For returns the upstream client and backfill worker for a route, or (nil, nil) when chaining
// is off for it.
//
// A misconfigured upstream is (nil, nil) too, not an error: a build must not fail because an
// operator typo'd a URL in a config blob. It logs, and serves from local data alone -- which is
// exactly what the backend did yesterday, before anyone configured an upstream.
func (u *Upstreams) For(_ context.Context, route cache.Route) (upstreamLookup, backfiller) {
	if u.disabled {
		return nil, nil
	}

	var cfg backendConfig
	if len(route.Config) > 0 {
		if err := json.Unmarshal(route.Config, &cfg); err != nil {
			u.log.Warn("hashserv: bad backend config",
				slog.String("org", route.Org), slog.String("project", route.Project),
				slog.Any("error", err))

			return nil, nil
		}
	}

	if cfg.Upstream == "" {
		return nil, nil
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	if c, ok := u.built[route.BackendID]; ok && c.addr == cfg.Upstream {
		return c.up, c.bf
	}

	c, err := u.build(route, cfg.Upstream)
	if err != nil {
		u.log.Warn("hashserv: upstream disabled for this backend",
			slog.String("org", route.Org), slog.String("project", route.Project),
			slog.String("upstream", cfg.Upstream), slog.Any("error", err))

		return nil, nil
	}

	// Replace, and close the client we are displacing, so a rewritten address does not leak a
	// pool of connections to the old one.
	if old, ok := u.built[route.BackendID]; ok {
		u.closeChain(old)
	}

	u.built[route.BackendID] = c

	return c.up, c.bf
}

func (u *Upstreams) build(route cache.Route, addr string) (*chain, error) {
	rec := u.metrics.Hashserv(route.Org, route.Project)

	up, err := NewUpstream(UpstreamConfig{URL: addr}, rec, u.log)
	if err != nil {
		return nil, err
	}

	bf, err := NewBackfiller(up, backfillWriter{q: u.q}, rec, u.log,
		BackfillConfig{BackendID: route.BackendID})
	if err != nil {
		_ = up.Close()

		return nil, err
	}

	return &chain{addr: addr, up: up, bf: bf}, nil
}

// Close tears down every upstream client and backfill worker.
func (u *Upstreams) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()

	for id, c := range u.built {
		u.closeChain(c)
		delete(u.built, id)
	}

	return nil
}

func (u *Upstreams) closeChain(c *chain) {
	if err := c.bf.Close(); err != nil {
		u.log.Warn("hashserv: closing backfiller", slog.Any("error", err))
	}

	if err := c.up.Close(); err != nil {
		u.log.Warn("hashserv: closing upstream", slog.Any("error", err))
	}
}
