package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// stubBackend is a minimal cache.Backend: it registers the same literal-segment cache
// patterns the real backends use and answers with a sentinel status, so the wiring test
// asserts NewHandler mounts them beside GET /, /api/v1/, /healthz and /readyz without a
// ServeMux panic AND that a cache request actually reaches the backend rather than the
// SPA catch-all.
type stubBackend struct {
	kind repository.BackendKind
	seg  string
	tail string
}

func (b stubBackend) Kind() repository.BackendKind { return b.kind }

func (b stubBackend) Register(mux *http.ServeMux) {
	pat := "/cache/{org}/{project}/" + b.seg + "/{" + b.tail + "}"
	if b.tail == "path" {
		pat = "/cache/{org}/{project}/" + b.seg + "/{path...}"
	}

	h := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }
	mux.HandleFunc("GET "+pat, h)
	mux.HandleFunc("HEAD "+pat, h)
	mux.HandleFunc("PUT "+pat, h)
}

// TestCacheBackendsMountBesideEverything proves the M2 wiring: two cache backends with
// literal 4th segments register on the REAL public mux (SPA + /api/v1/ + healthz +
// readyz) without panicking, and a /cache/... request routes to the backend rather than
// the SPA catch-all.
func TestCacheBackendsMountBesideEverything(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewHandler panicked registering cache backends: %v", r)
		}
	}()

	handler := NewHandler(Config{
		Dist:    testDist(),
		Version: "test",
		API:     http.NotFoundHandler(), // a methodless /api/v1/ subtree, as in production
		CacheBackends: []cache.Backend{
			stubBackend{kind: repository.BackendKindSstate, seg: "sstate", tail: "path"},
			stubBackend{kind: repository.BackendKindDownloads, seg: "downloads", tail: "basename"},
		},
	})

	cases := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"sstate HEAD reaches backend", http.MethodHead, "/cache/acme/widget/sstate/universal/aa/bb/sstate:zlib.tar.zst", http.StatusTeapot},
		{"downloads GET reaches backend", http.MethodGet, "/cache/acme/widget/downloads/zlib-1.3.tar.xz", http.StatusTeapot},
		{"sstate PUT reaches backend", http.MethodPut, "/cache/acme/widget/sstate/universal/aa/bb/x.tar.zst", http.StatusTeapot},
		{"healthz still answers", http.MethodGet, "/healthz", http.StatusOK},
		{"a console route still falls to the SPA", http.MethodGet, "/overview", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, tc.want)
			}
		})
	}
}
