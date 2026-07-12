// Package server wires the HTTP routes and owns the listener lifecycle.
//
// There are TWO listeners, and that is deliberate:
//
//   - the public one (--host/--port) serves the SPA, /api/v1, /healthz and
//     /readyz;
//   - the private one (--metrics-addr, loopback by default) serves /metrics and
//     nothing else.
//
// /metrics carries bakery_cache_requests_total{org,project,...} and
// bakery_storage_bytes{org,project,...}: every tenant slug, and how much each of
// them stores. Serving that on the same listener as the cache would hand the
// whole customer list to anyone who can fetch a cache object. There is no flag
// that merges them.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/middleware"
	"github.com/jsmith212/bakery/internal/storage"
)

const (
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second

	// readyTimeout bounds the database probe behind /readyz. A readiness check
	// that can block for the pool's entire acquire timeout hangs the load
	// balancer instead of failing to it.
	readyTimeout = 2 * time.Second
)

// Config is everything the server needs to run.
type Config struct {
	Addr    string
	Version string

	// MetricsAddr is the PRIVATE listener. Empty disables it -- which is a
	// supported choice (no metrics at all) and is not the same as serving them
	// publicly, which is not a choice this type can express.
	MetricsAddr string

	// Dist is the compiled SPA, rooted so that index.html is at the top. Ignored
	// when Headless is set.
	Dist fs.FS

	// Headless serves the API and metrics but NOT the console. The SPA routes are
	// then never registered, so an unknown path is a plain 404 from the mux -- not
	// a 500. "This deployment has no console" is a routing fact, not a failure.
	Headless bool

	// API is the /api/v1 subtree from api.API.Handler(). Nil in the tests that
	// only exercise the static routes.
	API http.Handler

	// Pool backs /readyz. Nil means "no database", and /readyz then reports 503 --
	// never 200.
	Pool *pgxpool.Pool

	// Metrics is served on MetricsAddr and nowhere else.
	Metrics *metrics.Metrics

	// Storage is the byte store the cache backends will write through. M1 has no
	// backend yet, so nothing READS it here -- but constructing it is what turns a
	// bad --storage-dir into a boot failure instead of a latent EACCES on the first
	// object written, and holding it on the server keeps the instrumented series
	// alive for the lifetime of the process. Nil is permitted only in tests that
	// exercise the static routes without a store.
	Storage storage.Store

	// Ready, when set, is called once BOTH listeners are bound and before either
	// serves, with the addresses they actually got. It exists because port 0 is the
	// only way to ask the kernel for a free port without a TOCTOU race, and a test
	// that asked for port 0 still has to learn which port it was given.
	// metricsAddr is nil when no metrics listener was configured.
	Ready func(public, metricsAddr net.Addr)
}

// Server owns both HTTP listeners.
type Server struct {
	public  *http.Server
	metrics *http.Server

	publicAddr  string
	metricsAddr string

	ready func(public, metricsAddr net.Addr)
}

// NewHandler builds the PUBLIC routing tree. It is exported so tests can exercise
// the routes over httptest without binding a port.
//
// /metrics is not here and must never be added: the metrics handler is mounted
// only on the private mux, in newMetricsHandler.
func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(cfg.Pool))

	if cfg.API != nil {
		// Methodless: the subtree owns its own method routing.
		mux.Handle(api.Prefix+"/", cfg.API)
	}

	if !cfg.Headless {
		// Registered WITHOUT a method verb. `GET /` alongside the methodless
		// `/api/v1/` leaves ServeMux with two patterns where neither is more
		// specific than the other, and registration PANICS: "/api/v1/ matches more
		// methods than GET /, but has a more specific path pattern". Verified on
		// Go 1.26. spaRoutes therefore does the method check itself.
		mux.Handle("/", spaRoutes(cfg.Dist))
	}

	return middleware.Default()(mux)
}

// spaRoutes serves the console and rejects the methods it has no meaning for. The
// SPA is a static asset tree: a POST to it is a client bug, and 405 says so where
// the mux's own method routing would have said 404.
func spaRoutes(dist fs.FS) http.Handler {
	spa := SPAHandler(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		spa.ServeHTTP(w, r)
	})
}

// newMetricsHandler is the PRIVATE mux: /metrics, plus a healthz so an operator
// can tell "the metrics listener is down" from "the scrape target is empty".
func newMetricsHandler(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /healthz", handleHealthz)

	return mux
}

// New builds a Server bound to cfg.Addr, plus the private metrics listener when
// cfg.MetricsAddr and cfg.Metrics are both set.
func New(cfg Config) *Server {
	s := &Server{
		public: &http.Server{
			Addr:              cfg.Addr,
			Handler:           NewHandler(cfg),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		metrics:     nil,
		publicAddr:  cfg.Addr,
		metricsAddr: cfg.MetricsAddr,
		ready:       cfg.Ready,
	}

	if cfg.Metrics != nil && cfg.MetricsAddr != "" {
		s.metrics = &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           newMetricsHandler(cfg.Metrics),
			ReadHeaderTimeout: readHeaderTimeout,
		}
	}

	return s
}

// Run serves until ctx is cancelled or SIGINT/SIGTERM arrives, then drains
// in-flight requests within shutdownTimeout.
//
// Both listeners BIND before either serves, so a port clash is a startup error
// rather than a half-started server; and if either Serve dies, both come down. A
// live cache with a dead metrics listener is a silently unmonitored server, which
// is worse than a crash: nothing pages, the graphs just go flat.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// noctx (enabled in .golangci.yml) rejects a bare net.Listen.
	var lc net.ListenConfig

	publicLn, err := lc.Listen(ctx, "tcp", s.publicAddr)
	if err != nil {
		return fmt.Errorf("bind listener %q: %w", s.publicAddr, err)
	}

	var metricsLn net.Listener

	if s.metrics != nil {
		metricsLn, err = lc.Listen(ctx, "tcp", s.metricsAddr)
		if err != nil {
			_ = publicLn.Close()

			return fmt.Errorf("bind metrics listener %q: %w", s.metricsAddr, err)
		}
	}

	if s.ready != nil {
		var metricsAddr net.Addr
		if metricsLn != nil {
			metricsAddr = metricsLn.Addr()
		}

		s.ready(publicLn.Addr(), metricsAddr)
	}

	errCh := make(chan error, 2)

	go func() {
		slog.Info("starting server", "address", publicLn.Addr().String())

		if err := s.public.Serve(publicLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve: %w", err)
		}
	}()

	if s.metrics != nil {
		go func() {
			slog.Info("starting metrics listener", "address", metricsLn.Addr().String())

			if err := s.metrics.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("serve metrics: %w", err)
			}
		}()
	}

	var serveErr error

	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	}

	// Unbind the signal handler so a second Ctrl-C kills the process immediately
	// rather than waiting out the drain.
	stop()
	slog.Info("shutting down", "timeout", shutdownTimeout)

	// WithoutCancel: by the time we reach here the parent context is usually
	// already cancelled, and handing that straight to Shutdown makes it return
	// instantly and cut every in-flight request instead of draining them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	// The public listener drains FIRST, so a final scrape of the metrics listener
	// can still observe it draining.
	publicErr := s.public.Shutdown(shutdownCtx)

	var metricsErr error
	if s.metrics != nil {
		metricsErr = s.metrics.Shutdown(shutdownCtx)
	}

	if err := errors.Join(serveErr, publicErr, metricsErr); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	slog.Info("shutdown complete")

	return nil
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	write(w, "ok\n")
}

// handleReadyz answers "would a request served right now succeed?", which for
// this binary means "can it reach Postgres?".
//
// It really pings the pool. A readyz that returns 200 because the process is up
// is a liveness check wearing a readiness check's name: it keeps a node in the
// load balancer's rotation while every request behind it 500s.
func handleReadyz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		if pool == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			write(w, "not ready: no database pool\n")

			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			slog.Warn("readyz: database ping failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			write(w, "not ready: database unreachable\n")

			return
		}

		w.WriteHeader(http.StatusOK)
		write(w, "ok\n")
	}
}

func write(w http.ResponseWriter, s string) {
	if _, err := w.Write([]byte(s)); err != nil {
		slog.Error("failed to write response", "error", err)
	}
}
