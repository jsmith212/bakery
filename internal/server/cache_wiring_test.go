package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/hashserv"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// stubBackend is a minimal cache.Backend: it registers the same literal-segment cache
// patterns the real backends use and answers with a sentinel status, so the wiring test
// asserts NewHandler mounts them beside GET /, /api/v1/, /healthz and /readyz without a
// ServeMux panic AND that a cache request actually reaches the backend rather than the
// SPA catch-all.
//
// tail selects the SHAPE of the pattern, and all three shapes must coexist:
//
//	"path" -> /cache/{org}/{project}/sstate/{path...}    (trailing wildcard)
//	"x"    -> /cache/{org}/{project}/downloads/{x}       (single trailing segment)
//	""     -> /cache/{org}/{project}/hashserv            (no tail at all, methodless)
//
// The empty one is hashserv's, and it is registered METHODLESS exactly as
// hashserv.Backend.Register does -- the upgrade is a GET, but pinning the verb here is
// what makes ServeMux weigh it against the methodless SPA catch-all and panic.
type stubBackend struct {
	kind repository.BackendKind
	seg  string
	tail string
}

func (b stubBackend) Kind() repository.BackendKind { return b.kind }

func (b stubBackend) Register(mux *http.ServeMux) {
	h := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }

	base := "/cache/{org}/{project}/" + b.seg

	if b.tail == "" {
		mux.HandleFunc(base, h)

		return
	}

	pat := base + "/{" + b.tail + "}"
	if b.tail == "path" {
		pat = base + "/{path...}"
	}

	mux.HandleFunc("GET "+pat, h)
	mux.HandleFunc("HEAD "+pat, h)
	mux.HandleFunc("PUT "+pat, h)
}

// TestCacheBackendsMountBesideEverything proves the wiring: three cache backends whose
// patterns have three different shapes -- sstate's trailing wildcard, downloads' single
// segment, and hashserv's bare methodless literal -- register on the REAL public mux (SPA
// + /api/v1/ + healthz + readyz) without panicking, and a /cache/... request routes to the
// backend rather than the SPA catch-all.
//
// It runs in BOTH modes because `bakery serve --headless` serves the API, metrics AND the
// cache backends: "no console" is not "no cache". A headless deployment that quietly
// stopped answering hashserv would strand every bitbake pointed at it, and the failure
// mode is a build that goes green with a silently degraded cache.
func TestCacheBackendsMountBesideEverything(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewHandler panicked registering cache backends: %v", r)
		}
	}()

	backends := []cache.Backend{
		stubBackend{kind: repository.BackendKindSstate, seg: "sstate", tail: "path"},
		stubBackend{kind: repository.BackendKindDownloads, seg: "downloads", tail: "basename"},
		stubBackend{kind: repository.BackendKindHashserv, seg: "hashserv", tail: ""},
	}

	cases := []struct {
		name   string
		method string
		target string
		want   int

		// wantHeadless overrides want when the console is not routed. Zero means "the
		// same in both modes", which is the answer for every cache route -- that is the
		// whole point of the test.
		wantHeadless int
	}{
		{
			name: "sstate HEAD reaches backend", method: http.MethodHead,
			target: "/cache/acme/widget/sstate/universal/aa/bb/sstate:zlib.tar.zst", want: http.StatusTeapot,
		},
		{
			name: "downloads GET reaches backend", method: http.MethodGet,
			target: "/cache/acme/widget/downloads/zlib-1.3.tar.xz", want: http.StatusTeapot,
		},
		{
			name: "sstate PUT reaches backend", method: http.MethodPut,
			target: "/cache/acme/widget/sstate/universal/aa/bb/x.tar.zst", want: http.StatusTeapot,
		},
		{
			name: "hashserv GET reaches backend", method: http.MethodGet,
			target: "/cache/acme/widget/hashserv", want: http.StatusTeapot,
		},
		{
			name: "healthz still answers", method: http.MethodGet,
			target: "/healthz", want: http.StatusOK,
		},
		{
			name:   "a console route falls to the SPA, and 404s when there is no console",
			method: http.MethodGet, target: "/overview",
			want: http.StatusOK, wantHeadless: http.StatusNotFound,
		},
	}

	for _, headless := range []bool{false, true} {
		handler := NewHandler(Config{
			Dist:          testDist(),
			Version:       "test",
			Headless:      headless,
			API:           http.NotFoundHandler(), // a methodless /api/v1/ subtree, as in production
			CacheBackends: backends,
		})

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				want := tc.want
				if headless && tc.wantHeadless != 0 {
					want = tc.wantHeadless
				}

				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

				if rec.Code != want {
					t.Errorf("headless=%v: %s %s = %d, want %d",
						headless, tc.method, tc.target, rec.Code, want)
				}
			})
		}
	}
}

// TestUnroutedCachePathsAre404 pins the catch-all that keeps the methodless SPA `/` from
// swallowing an unrouted /cache/... or /v2/... path.
//
// Without it, `mux.Handle("/", spaRoutes(...))` matches every path no backend claimed, so a
// GET to a /cache/ key that no backend routes returns 200 + index.html -- a POISONED cache
// HIT: ccache/sccache/moon read the console shell as a cache object and feed HTML into their
// entry parser. Worse, a HEAD on such a path returns 200 with an EMPTY body, the exact thing
// the shipped sstate invariant forbids ("misses return 404 ... never a 200 with an empty
// body"), on the exact verb it was written for. The hole is invisible to every existing
// cache test because they all assert on ROUTED paths, and headless mode 404s cleanly -- so
// the bug lives only in the default (console) mode.
//
// The fix is `/cache/` and `/v2/` NotFoundHandlers, registered UNCONDITIONALLY (the bug IS
// the divergence between console and headless; making them identical is the point). Every
// backend pattern matches a strict subset of `/cache/`, so ServeMux does not panic and the
// real routes still win where they apply -- proven by TestCacheBackendsMountBesideEverything
// and the routed controls below.
//
// The assertions are on the BODY and the Content-Type, never the status alone: the SPA
// answers 200 for everything, so a status-only check would go green for the wrong reason the
// day someone "simplifies" the handler.
func TestUnroutedCachePathsAre404(t *testing.T) {
	backends := []cache.Backend{
		stubBackend{kind: repository.BackendKindSstate, seg: "sstate", tail: "path"},
		stubBackend{kind: repository.BackendKindDownloads, seg: "downloads", tail: "basename"},
		stubBackend{kind: repository.BackendKindHashserv, seg: "hashserv", tail: ""},
	}

	// Every target's 4th segment is deliberately NOT a real backend kind (sstate, downloads,
	// hashserv, and the future ac/cas/sccache), and the /v2/ ones are the OCI shape M5 has
	// not landed -- so all of these stay unrouted across the milestones this test outlives.
	cases := []struct {
		name          string
		method        string
		target        string
		routedControl bool // a real backend route: must 404 via its OWN pattern, not the catch-all
	}{
		{name: "ccache subdirs layout", method: http.MethodGet, target: "/cache/acme/widget/ab/cdef0123"},
		{name: "ccache subdirs HEAD (the 200+empty-body case)", method: http.MethodHead, target: "/cache/acme/widget/ab/cdef0123"},
		{name: "moon http-mode /status probe", method: http.MethodGet, target: "/cache/status"},
		{name: "garbage kind segment", method: http.MethodGet, target: "/cache/acme/widget/nope/x"},
		{name: "bare cache prefix", method: http.MethodGet, target: "/cache/acme"},
		{name: "oci manifest (BuildKit shape, pre-M5)", method: http.MethodGet, target: "/v2/acme/widget/manifests/latest"},
		{name: "oci blob (pre-M5)", method: http.MethodHead, target: "/v2/acme/widget/blobs/sha256:deadbeef"},

		// Controls: a routed-but-unconfigured path must still 404, and it must reach the
		// backend's own pattern (here the stub answers 418) -- proving the catch-all did not
		// shadow the real routes.
		{name: "routed sstate still reaches its backend", method: http.MethodGet, target: "/cache/acme/widget/sstate/x", routedControl: true},
	}

	for _, headless := range []bool{false, true} {
		handler := NewHandler(Config{
			Dist:          testDist(),
			Headless:      headless,
			API:           http.NotFoundHandler(),
			CacheBackends: backends,
		})

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

				if tc.routedControl {
					// The stub answers 418 on GET/HEAD/PUT; the point is it reached the
					// backend, not the SPA and not the catch-all.
					if rec.Code != http.StatusTeapot {
						t.Errorf("headless=%v: %s %s = %d, want 418 (routed to backend)",
							headless, tc.method, tc.target, rec.Code)
					}

					return
				}

				if rec.Code != http.StatusNotFound {
					t.Errorf("headless=%v: %s %s = %d, want 404",
						headless, tc.method, tc.target, rec.Code)
				}

				if body := rec.Body.String(); strings.Contains(body, "<title>bakery</title>") {
					t.Errorf("headless=%v: %s %s returned the console shell -- a poisoned hit",
						headless, tc.method, tc.target)
				}

				if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
					t.Errorf("headless=%v: %s %s served Content-Type %q -- a cache client would "+
						"ingest HTML as a cache object", headless, tc.method, tc.target, ct)
				}
			})
		}
	}
}

// stubRoutes is a hashserv.RouteResolver. ok=false is "there is no cache_backends row for
// this project and kind" -- the unconfigured backend.
type stubRoutes struct {
	route cache.Route
	ok    bool
}

func (s stubRoutes) Resolve(
	_ context.Context, _, _ string, _ repository.BackendKind,
) (cache.Route, bool) {
	return s.route, s.ok
}

// TestHashservUnconfiguredBackendIs404 drives the REAL hashserv.Backend -- its own
// Register, its own handler -- mounted on the REAL public mux beside the M2 backends and
// the SPA.
//
// A project with no hashserv cache_backends row must get a 404 at the upgrade. Not a 500,
// and above all not a WebSocket upgrade into a backend that has nothing to serve: the
// server never mounts a mount point it cannot serve. bitbake turns the 404 into a
// ConnectionError, warns, and builds on with unihash = taskhash -- degraded but correct,
// which is the right outcome for a mount that does not exist.
//
// What must NOT happen here is an auth-shaped answer. A 401/403 is a *different* failure
// with a *different* handling in bb.siggen, and the whole M3 auth design turns on keeping
// the two apart: an unconfigured backend degrades, a denied credential halts.
func TestHashservUnconfiguredBackendIs404(t *testing.T) {
	// The upstreams provider, the authenticator and the query surface are all nil: an
	// unconfigured route is refused before any of them is reachable. If a refactor ever
	// moved the upgrade ahead of the route lookup, this test would panic rather than
	// pass, which is the correct outcome.
	backend := func(routes hashserv.RouteResolver) cache.Backend {
		deps := cache.Deps{Metrics: metrics.New(), Logger: slog.Default()}

		return hashserv.New(deps, routes, nil, nil, nil)
	}

	// Both ways a backend can be absent. The second one matters: a row that exists but is
	// disabled is not "misconfigured", it is off, and off must look exactly like absent.
	resolvers := map[string]hashserv.RouteResolver{
		"no cache_backends row": stubRoutes{ok: false},
		"row exists but is disabled": stubRoutes{ok: true, route: cache.Route{
			Org: "acme", Project: "widget", BackendID: 1,
			Kind: repository.BackendKindHashserv, Enabled: false,
		}},
	}

	for name, routes := range resolvers {
		for _, headless := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				handler := NewHandler(Config{
					Dist:     testDist(),
					Headless: headless,
					API:      http.NotFoundHandler(),
					CacheBackends: []cache.Backend{
						stubBackend{kind: repository.BackendKindSstate, seg: "sstate", tail: "path"},
						backend(routes),
					},
				})

				// A REAL bitbake upgrade attempt, headers and all. Sending a plain GET
				// would leave the interesting branch -- "we accepted the upgrade and then
				// discovered we had nothing to serve" -- untested.
				req := httptest.NewRequest(http.MethodGet, "/cache/acme/widget/hashserv", nil)
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Sec-WebSocket-Version", "13")
				req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusNotFound {
					t.Errorf("headless=%v: GET /cache/acme/widget/hashserv = %d, want 404",
						headless, rec.Code)
				}

				// Never an auth denial. 401 sends bitbake down the retry-then-warn path and
				// the build completes with a silently degraded cache; 403 is the same class
				// of lie. The upgrade is never the place a credential is judged.
				if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
					t.Errorf("headless=%v: the upgrade answered %d -- an unconfigured backend "+
						"must 404, never deny", headless, rec.Code)
				}

				// And it must not have upgraded. 101 is a status check; the Upgrade header
				// catches a handler that started the handshake and then thought better of it.
				if got := rec.Header().Get("Upgrade"); got != "" {
					t.Errorf("headless=%v: unconfigured backend negotiated an upgrade (Upgrade: %q)",
						headless, got)
				}

				// Assert on the BODY, not just the status: in console mode the SPA catch-all
				// answers 200 + index.html for every unrouted path, so a hashserv route that
				// was never registered would be swallowed by it. A status-only assertion
				// would still be red here -- but the moment someone "fixes" the status it
				// would go green for the wrong reason, and this is the line that says so.
				if body := rec.Body.String(); strings.Contains(body, "<title>bakery</title>") {
					t.Errorf("headless=%v: the SPA catch-all swallowed the hashserv route; "+
						"got the console shell, not the backend", headless)
				}
			})
		}
	}
}
