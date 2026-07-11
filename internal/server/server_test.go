package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// testDist stands in for a real SvelteKit build: an HTML shell plus a hashed
// asset under _app/immutable. The server package must not depend on whether the
// frontend has actually been compiled.
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><title>bakery</title>"),
		},
		"_app/immutable/entry/app.abc123.js": &fstest.MapFile{
			Data: []byte("export default 1;\n"),
		},
		"favicon.png": &fstest.MapFile{
			Data: []byte("\x89PNG\r\n\x1a\n"),
		},
	}
}

func newTestHandler() http.Handler {
	return NewHandler(Config{Dist: testDist(), Version: "test"})
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	newTestHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: got status %d, want %d", rec.Code, http.StatusOK)
	}

	if got := rec.Body.String(); got != "ok\n" {
		t.Errorf("GET /healthz: got body %q, want %q", got, "ok\n")
	}
}

func TestHealthzRejectsNonGET(t *testing.T) {
	tests := []string{http.MethodPost, http.MethodPut, http.MethodDelete}

	for _, method := range tests {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/healthz", nil)
			rec := httptest.NewRecorder()

			newTestHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf(
					"%s /healthz: got status %d, want %d",
					method, rec.Code, http.StatusMethodNotAllowed,
				)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantType    string
		wantBody    string
		wantCache   string
		wantExactly bool
	}{
		{
			name:        "root serves the SPA shell",
			path:        "/",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantBody:    "<!doctype html><title>bakery</title>",
			wantCache:   "no-cache",
			wantExactly: true,
		},
		{
			name:        "hashed asset is served with a real content type",
			path:        "/_app/immutable/entry/app.abc123.js",
			wantStatus:  http.StatusOK,
			wantType:    "text/javascript; charset=utf-8",
			wantBody:    "export default 1;\n",
			wantCache:   "public, max-age=31536000, immutable",
			wantExactly: true,
		},
		{
			name:       "static asset is served",
			path:       "/favicon.png",
			wantStatus: http.StatusOK,
			wantType:   "image/png",
		},
		{
			name:        "unknown client route falls back to index.html",
			path:        "/orgs/acme/projects/kernel",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantBody:    "<!doctype html><title>bakery</title>",
			wantExactly: true,
		},
		{
			name:        "missing asset falls back to index.html",
			path:        "/_app/immutable/entry/does-not-exist.js",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantBody:    "<!doctype html><title>bakery</title>",
			wantExactly: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			newTestHandler().ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s: got status %d, want %d", tt.path, res.StatusCode, tt.wantStatus)
			}

			if got := res.Header.Get("Content-Type"); got != tt.wantType {
				t.Errorf("GET %s: got Content-Type %q, want %q", tt.path, got, tt.wantType)
			}

			if tt.wantCache != "" {
				if got := res.Header.Get("Cache-Control"); got != tt.wantCache {
					t.Errorf("GET %s: got Cache-Control %q, want %q", tt.path, got, tt.wantCache)
				}
			}

			if tt.wantExactly {
				body, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}

				if string(body) != tt.wantBody {
					t.Errorf("GET %s: got body %q, want %q", tt.path, body, tt.wantBody)
				}
			}
		})
	}
}

// The SPA fallback must not become a path-traversal escape hatch.
func TestSPADoesNotEscapeDist(t *testing.T) {
	tests := []string{
		"/../main.go",
		"/../../etc/passwd",
		"/_app/../../etc/passwd",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			newTestHandler().ServeHTTP(rec, req)

			// Either a redirect (http.FileServer normalising) or the SPA shell is
			// fine; leaking file contents is not.
			if rec.Code == http.StatusOK {
				if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
					t.Errorf("GET %s: served %q, expected the SPA shell", path, got)
				}
			}
		})
	}
}

// A binary built with no frontend must fail loudly rather than serve a blank 200.
func TestSPAWithoutIndexFailsLoudly(t *testing.T) {
	handler := NewHandler(Config{Dist: fstest.MapFS{}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("GET / with an empty dist: got status %d, want %d",
			rec.Code, http.StatusInternalServerError)
	}
}
