package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/metrics"
)

// stubAPI stands in for api.API.Handler(). The routing tree must not care what is
// behind the /api/v1 mount, only that it is mounted.
func stubAPI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

// The headless mode contract: the console is NOT ROUTED, and that must be a 404
// from the mux. A 500 ("frontend not built") would be the SPA handler running and
// failing -- indistinguishable, to an operator, from a broken build.
func TestHeadlessDoesNotRouteTheSPA(t *testing.T) {
	tests := []struct {
		name     string
		headless bool
		path     string
		want     int
	}{
		{name: "console: root serves the SPA", headless: false, path: "/", want: http.StatusOK},
		{
			name: "console: deep link falls back to the SPA", headless: false,
			path: "/overview", want: http.StatusOK,
		},
		{name: "headless: root is not routed", headless: true, path: "/", want: http.StatusNotFound},
		{
			name: "headless: deep link is not routed", headless: true,
			path: "/overview", want: http.StatusNotFound,
		},
		{name: "headless: healthz still answers", headless: true, path: "/healthz", want: http.StatusOK},
		{
			name: "headless: the api is still mounted", headless: true,
			path: "/api/v1/me", want: http.StatusTeapot,
		},
		{
			name: "console: the api is still mounted", headless: false,
			path: "/api/v1/me", want: http.StatusTeapot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(Config{Dist: testDist(), Headless: tt.headless, API: stubAPI()})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.want {
				t.Fatalf("GET %s (headless=%v): got %d, want %d",
					tt.path, tt.headless, rec.Code, tt.want)
			}
		})
	}
}

// /metrics leaks every org and project slug and their stored byte counts. It must
// not be reachable on the public listener -- and a status-code assertion is not
// enough to prove that, because the SPA catch-all answers 200 for every unknown
// path. Assert on the BODY.
func TestMetricsIsNotOnThePublicMux(t *testing.T) {
	m := metrics.New()
	m.ObserveCacheRequest("acme", "widget", metrics.BackendSstate, "object", metrics.OpHead, metrics.ResultHit)

	for _, headless := range []bool{false, true} {
		h := NewHandler(Config{Dist: testDist(), Headless: headless, Metrics: m, API: stubAPI()})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		body := rec.Body.String()
		if strings.Contains(body, "bakery_cache_requests_total") {
			t.Fatalf("headless=%v: the public listener served the metrics exposition", headless)
		}

		if headless && rec.Code != http.StatusNotFound {
			t.Errorf("headless GET /metrics: got %d, want 404", rec.Code)
		}
	}
}

// A readyz that returns 200 with a dead database is worse than no readyz: it keeps
// the node in rotation while every request behind it fails. With no pool at all
// there is nothing that could succeed, so the answer is 503.
func TestReadyzWithoutAPoolIsNotReady(t *testing.T) {
	h := NewHandler(Config{Dist: testDist(), Pool: nil})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz with no pool: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// /healthz is liveness and must still say yes: the process is up, it just
	// cannot serve. Conflating the two makes an orchestrator restart a healthy
	// binary because Postgres is down.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz with no pool: got %d, want 200", rec.Code)
	}
}

func TestSPARejectsWriteMethods(t *testing.T) {
	h := NewHandler(Config{Dist: testDist(), API: stubAPI()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// The dual-listener lifecycle, over real sockets: both bind, /metrics answers on
// the private one and NOT on the public one, and cancelling the context takes both
// down.
func TestDualListenerLifecycle(t *testing.T) {
	m := metrics.New()
	m.ObserveCacheRequest("acme", "widget", metrics.BackendSstate, "object", metrics.OpHead, metrics.ResultHit)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan [2]string, 1)

	srv := New(Config{
		Addr:        "127.0.0.1:0",
		MetricsAddr: "127.0.0.1:0",
		Dist:        testDist(),
		Metrics:     m,
		Ready: func(public, _, metricsAddr net.Addr) {
			ready <- [2]string{public.String(), metricsAddr.String()}
		},
	})

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var addrs [2]string

	select {
	case addrs = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listeners never bound")
	}

	public, private := "http://"+addrs[0], "http://"+addrs[1]

	if got := get(t, private+"/metrics"); !strings.Contains(got, "bakery_cache_requests_total") {
		t.Errorf("metrics listener did not serve the exposition; got %.120q", got)
	}

	if got := get(t, public+"/metrics"); strings.Contains(got, "bakery_cache_requests_total") {
		t.Error("the PUBLIC listener served the metrics exposition")
	}

	if got := get(t, public+"/healthz"); got != "ok\n" {
		t.Errorf("public /healthz: got %q, want %q", got, "ok\n")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// Both ports must be gone. A live cache with a dead metrics listener is a
	// silently unmonitored server, so the shutdown is all-or-nothing too.
	for _, base := range []string{public, private} {
		if _, err := http.Get(base + "/healthz"); err == nil { //nolint:noctx,bodyclose // the point is that it fails
			t.Errorf("%s still accepts connections after shutdown", base)
		}
	}
}

// A metrics port that is already taken must fail STARTUP, not leave a running
// server nobody can scrape.
func TestMetricsPortClashFailsStartup(t *testing.T) {
	var lc net.ListenConfig

	squatter, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind squatter: %v", err)
	}

	defer squatter.Close()

	srv := New(Config{
		Addr:        "127.0.0.1:0",
		MetricsAddr: squatter.Addr().String(),
		Dist:        testDist(),
		Metrics:     metrics.New(),
	})

	err = srv.Run(t.Context())
	if err == nil {
		t.Fatal("Run returned nil with the metrics port already bound")
	}

	if !strings.Contains(err.Error(), "bind metrics listener") {
		t.Fatalf("Run: got %v, want a bind-metrics-listener error", err)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}

	return string(body)
}
