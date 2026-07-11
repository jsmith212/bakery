// Package server wires the HTTP routes and owns the listener lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jsmith212/bakery/internal/middleware"
)

const (
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
)

// Config is everything the server needs to run.
type Config struct {
	Addr    string
	Version string

	// Dist is the compiled SPA, rooted so that index.html is at the top.
	Dist fs.FS
}

// Server owns the HTTP listener.
type Server struct {
	http *http.Server
}

// NewHandler builds the routing tree. It is exported so tests can exercise the
// routes over httptest without binding a port.
func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("GET /", SPAHandler(cfg.Dist))

	return middleware.Default()(mux)
}

// New builds a Server bound to cfg.Addr.
func New(cfg Config) *Server {
	return &Server{
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           NewHandler(cfg),
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
}

// Run serves until ctx is cancelled or SIGINT/SIGTERM arrives, then drains
// in-flight requests within shutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	go func() {
		slog.Info("starting server", "address", s.http.Addr)

		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	// Unbind the signal handler so a second Ctrl-C kills the process immediately
	// rather than waiting out the drain.
	stop()
	slog.Info("shutting down", "timeout", shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	slog.Info("shutdown complete")

	return nil
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte("ok\n")); err != nil {
		slog.Error("failed to write healthz response", "error", err)
	}
}
