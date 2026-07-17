// Package server wires the HTTP routes and owns the listener lifecycle.
//
// There are THREE listeners, and that is deliberate:
//
//   - the public one (--host/--port) serves the SPA, /api/v1, /healthz, /readyz and
//     the /cache HTTP surface (sstate, downloads, ac, cas, sccache, hashserv WS);
//   - the gRPC one (--grpc-addr, loopback by default) serves the M4 Bazel REAPI and
//     NOTHING else -- on its own listener so grpc-go's GracefulStop can drain it
//     without the ServeHTTP-transport panic a shared port would hit;
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
	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/cache"
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

	// GRPC is the M4 Bazel REAPI server. It is served on GRPCAddr and NOWHERE else --
	// never on the public mux. Nil disables the gRPC listener entirely (the static-route
	// tests pass nil); even when set, the listener binds only when GRPCAddr is non-empty,
	// exactly as Metrics/MetricsAddr gate the private listener.
	GRPC *grpc.Server

	// GRPCAddr is the REAPI listener address. Empty disables it. It is its OWN listener,
	// not a demux of the public port, and Run's shutdown sequence relies on that: on a
	// dedicated net.Listener grpc-go builds its real http2Server, whose Drain is
	// implemented, so GracefulStop drains an in-flight stream cleanly. On the shared
	// ServeHTTP path GracefulStop PANICS (serverHandlerTransport.Drain is unimplemented)
	// and only under load -- which is why gRPC must never ride the public mux.
	GRPCAddr string

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

	// CacheBackends are the M2+ cache backends (sstate, downloads, ...). Each mounts
	// its own /cache/{org}/{project}/... patterns on the public mux. They are served
	// in headless mode too -- "no console" does not mean "no cache". Nil is fine: a
	// deployment with no cache backends (or the static-route tests) simply registers
	// none.
	CacheBackends []cache.Backend

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

	// Ready, when set, is called once ALL THREE listeners are bound and before any
	// serves, with the addresses they actually got. It exists because port 0 is the
	// only way to ask the kernel for a free port without a TOCTOU race, and a test
	// that asked for port 0 still has to learn which port it was given. grpcAddr and
	// metricsAddr are nil when their respective listeners were not configured.
	Ready func(public, grpcAddr, metricsAddr net.Addr)
}

// Server owns all three listeners: public (HTTP), gRPC (REAPI), and metrics (HTTP).
type Server struct {
	public  *http.Server
	grpc    *grpc.Server
	metrics *http.Server

	publicAddr  string
	grpcAddr    string
	metricsAddr string

	ready func(public, grpcAddr, metricsAddr net.Addr)
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

	// The cache backends mount their /cache/{org}/{project}/... patterns. They
	// register GET/HEAD/PUT on LITERAL 4th segments (sstate, downloads), which is why
	// they coexist with each other and with the methodless /api/v1/ and SPA / without
	// the ServeMux "neither is more specific" panic. Served in headless mode too.
	for _, backend := range cfg.CacheBackends {
		backend.Register(mux)
	}

	// Unrouted /cache/ and /v2/ paths are 404 in BOTH modes. Without this the methodless
	// SPA "/" below swallows them and answers 200 + index.html -- a POISONED cache HIT for
	// any HTTP cache client (ccache, sccache, moon, Bazel), and 200 + an EMPTY body on HEAD,
	// which the sstate hot path forbids outright. Every backend pattern matches a strict
	// subset of these prefixes, so registration does not panic and the real routes still win
	// where they apply. Registered UNCONDITIONALLY: the bug IS the divergence between console
	// and headless (headless already 404s via the mux default), so making them identical is
	// the fix. r.Pattern here is the CONSTANT "/cache/" or "/v2/", so the Prometheus-label
	// invariant (label on the pattern, never the URL) holds.
	mux.Handle("/cache/", http.NotFoundHandler())
	mux.Handle("/v2/", http.NotFoundHandler())

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

// New builds a Server bound to cfg.Addr, plus the gRPC REAPI listener when cfg.GRPC and
// cfg.GRPCAddr are both set, plus the private metrics listener when cfg.MetricsAddr and
// cfg.Metrics are both set.
func New(cfg Config) *Server {
	s := &Server{
		public: &http.Server{
			Addr:              cfg.Addr,
			Handler:           NewHandler(cfg),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		metrics:     nil,
		publicAddr:  cfg.Addr,
		grpcAddr:    cfg.GRPCAddr,
		metricsAddr: cfg.MetricsAddr,
		ready:       cfg.Ready,
	}

	// The gRPC server is served on its OWN listener, bound only when both a server and
	// an address are configured -- the same gating as the metrics listener below.
	if cfg.GRPC != nil && cfg.GRPCAddr != "" {
		s.grpc = cfg.GRPC
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
// All THREE listeners BIND before any serves, so a port clash is a startup error
// rather than a half-started server; and if any Serve dies, all come down. A live
// cache with a dead metrics listener is a silently unmonitored server, which is worse
// than a crash: nothing pages, the graphs just go flat.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// noctx (enabled in .golangci.yml) rejects a bare net.Listen.
	var lc net.ListenConfig

	publicLn, err := lc.Listen(ctx, "tcp", s.publicAddr)
	if err != nil {
		return fmt.Errorf("bind listener %q: %w", s.publicAddr, err)
	}

	var grpcLn net.Listener

	if s.grpc != nil {
		grpcLn, err = lc.Listen(ctx, "tcp", s.grpcAddr)
		if err != nil {
			_ = publicLn.Close()

			return fmt.Errorf("bind grpc listener %q: %w", s.grpcAddr, err)
		}
	}

	var metricsLn net.Listener

	if s.metrics != nil {
		metricsLn, err = lc.Listen(ctx, "tcp", s.metricsAddr)
		if err != nil {
			_ = publicLn.Close()

			if grpcLn != nil {
				_ = grpcLn.Close()
			}

			return fmt.Errorf("bind metrics listener %q: %w", s.metricsAddr, err)
		}
	}

	if s.ready != nil {
		var grpcAddr, metricsAddr net.Addr

		if grpcLn != nil {
			grpcAddr = grpcLn.Addr()
		}

		if metricsLn != nil {
			metricsAddr = metricsLn.Addr()
		}

		s.ready(publicLn.Addr(), grpcAddr, metricsAddr)
	}

	errCh := make(chan error, 3)

	go func() {
		slog.Info("starting server", "address", publicLn.Addr().String())

		if err := s.public.Serve(publicLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve: %w", err)
		}
	}()

	if s.grpc != nil {
		go func() {
			slog.Info("starting grpc listener", "address", grpcLn.Addr().String())

			// grpc.Server.Serve returns nil once GracefulStop or Stop is called, so a
			// clean shutdown pushes nothing here; anything else is a real serve failure.
			if err := s.grpc.Serve(grpcLn); err != nil {
				errCh <- fmt.Errorf("serve grpc: %w", err)
			}
		}()
	}

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

	// gRPC drains SECOND. GracefulStop is safe here ONLY because s.grpc.Serve was given
	// a dedicated net.Listener: that path builds grpc-go's real http2Server, whose Drain
	// is implemented, so GracefulStop drains an in-flight stream cleanly. On the shared
	// ServeHTTP path GracefulStop PANICS -- serverHandlerTransport.Drain is
	// `panic("Drain() is not implemented")` -- and only when an RPC is in flight, so a
	// naive test passes and production crashes under load. That is the whole reason gRPC
	// gets its own listener and never rides the public mux. If the deadline elapses
	// before the drain finishes, Stop() hard-cuts the remaining streams.
	if s.grpc != nil {
		done := make(chan struct{})

		go func() {
			s.grpc.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
		case <-shutdownCtx.Done():
			s.grpc.Stop()
		}
	}

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
