package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/bazel"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/cache/oci"
	"github.com/jsmith212/bakery/internal/metrics"
)

// TestOCIMountsInBothModes proves the M5 boot wiring: oci.Backend registers its TWO
// route families, its TWO pings and its TWO token endpoints beside the SPA, /api/v1
// and every M2/M3/M4 backend, in BOTH console and headless mode, with no ServeMux
// panic -- exactly the shape TestBazelAndSccacheMountInBothModes proved for M4.
//
// It assembles the backend slice the same way Boot does (see boot.go's cacheBackends),
// so a regression in that construction -- a route family that collides with an
// existing pattern, or a pattern registered twice -- is caught here without a
// database. routes is UNCONFIGURED (ok=false): every resolved-route request 404s
// before auth or upstream is ever reached, which is why nil is safe for oci.New's
// Authenticator and Fetcher arguments here, same as bazel.New(deps, routes, nil, nil)
// above.
func TestOCIMountsInBothModes(t *testing.T) {
	deps := cache.Deps{Metrics: metrics.New()}
	routes := stubRoutes{ok: false}

	// The pings and token endpoints answer WITHOUT resolving a route at all -- see
	// oci.Backend.serveGlobalPing/serveTenantPing/serveToken -- so they must succeed
	// even against a totally unconfigured resolver. Everything else is a resolved
	// route on an unconfigured backend, which must 404 but still REACH oci.Backend
	// rather than the SPA catch-all.
	targets := []struct {
		name       string
		method     string
		target     string
		wantStatus int
	}{
		{"global ping", http.MethodGet, "/v2/", http.StatusOK},
		{"buildkit/podman tenant ping", http.MethodGet, "/v2/acme/widget/", http.StatusOK},
		{"containerd/docker-engine tenant ping", http.MethodGet, "/cache/acme/widget/docker/v2/", http.StatusOK},
		{"global token, GET", http.MethodGet, "/v2/token", http.StatusOK},
		{"global token, POST (containerd's OAuth2 form)", http.MethodPost, "/v2/token", http.StatusOK},
		{"tenant token, GET", http.MethodGet, "/cache/acme/widget/docker/v2/token", http.StatusOK},
		{"tenant token, POST", http.MethodPost, "/cache/acme/widget/docker/v2/token", http.StatusOK},
		{"buildkit/podman manifest, unconfigured", http.MethodGet, "/v2/acme/widget/library/alpine/manifests/latest", http.StatusNotFound},
		{"containerd/docker-engine blob HEAD, unconfigured", http.MethodHead, "/cache/acme/widget/docker/v2/library/alpine/blobs/sha256:" + strings.Repeat("a", 64), http.StatusNotFound},
	}

	var ociBackend cache.Backend

	for _, headless := range []bool{false, true} {
		// A FRESH oci.Backend per iteration: it guards its own GLOBAL routes (the
		// bare /v2/ ping and /v2/token) against a double Register, which a real
		// process never triggers -- Boot constructs the slice exactly once -- but this
		// table drives NewHandler twice against the same backend LIST, once per mode,
		// exactly as TestBazelAndSccacheMountInBothModes does for bazel and sccache
		// (which carry no such guard). bazel and sccache tolerate reuse; oci does not,
		// by design (see oci.Backend.Register's doc), so it alone is rebuilt here.
		b := oci.New(deps, routes, nil, nil, oci.Config{ExternalURL: "https://bakery.example.com"})
		ociBackend = b

		backends := []cache.Backend{
			bazel.New(deps, routes, nil, nil),
			httpblob.NewSccache(deps, routes, nil),
			b,
		}

		handler := NewHandler(Config{
			Dist:          testDist(),
			Headless:      headless,
			API:           http.NotFoundHandler(),
			CacheBackends: backends,
		})

		for _, tc := range targets {
			t.Run(fmt.Sprintf("headless=%v/%s", headless, tc.name), func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

				if rec.Code != tc.wantStatus {
					t.Errorf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, tc.wantStatus)
				}

				if body := rec.Body.String(); strings.Contains(body, "<title>bakery</title>") {
					t.Errorf("%s %s returned the console shell -- the SPA catch-all swallowed the "+
						"route, so oci.Backend was never mounted", tc.method, tc.target)
				}

				if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
					t.Errorf("%s %s served Content-Type %q -- a registry client would ingest HTML",
						tc.method, tc.target, ct)
				}
			})
		}
	}

	// oci.Backend is HTTP-only: unlike bazel it must not implement cache.GRPCBackend.
	if _, ok := ociBackend.(cache.GRPCBackend); ok {
		t.Error("the oci backend implements cache.GRPCBackend -- it must not; REAPI is bazel's alone")
	}
}

// TestOCIPingCarriesChallengeThroughBoot proves the highest-stakes M5 assertion
// (see oci.Backend.challenge's doc) survives the REAL boot wiring, not just oci's own
// package tests: podman/skopeo/CRI-O harvest auth challenges from the /v2/ ping
// response and nowhere else, so a ping mounted with no challenge header would
// silently convince every one of those clients that this registry needs no
// authentication.
func TestOCIPingCarriesChallengeThroughBoot(t *testing.T) {
	deps := cache.Deps{Metrics: metrics.New()}
	backend := oci.New(deps, stubRoutes{ok: false}, nil, nil,
		oci.Config{ExternalURL: "https://bakery.example.com"})

	handler := NewHandler(Config{
		Dist:          testDist(),
		API:           http.NotFoundHandler(),
		CacheBackends: []cache.Backend{backend},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v2/ = %d, want 200", rec.Code)
	}

	challenge := rec.Header().Get("WWW-Authenticate")
	if challenge == "" {
		t.Fatal("GET /v2/ answered 200 with no WWW-Authenticate -- every containers/image " +
			"client will now skip authentication and silently bypass this cache")
	}

	if !strings.Contains(challenge, `realm="https://bakery.example.com/v2/token"`) {
		t.Errorf("WWW-Authenticate = %q, want an absolute realm built from ExternalURL", challenge)
	}
}

// TestUnroutedCachePathsAre404OCI is TestUnroutedCachePathsAre404's counterpart WITH
// the real oci.Backend mounted: it proves the M4 poisoned-hit invariant still holds
// once M5's two extra route families exist on the same mux. oci.Backend's own
// patterns are strict subsets of `/v2/` and `/cache/{org}/{project}/docker/v2/`
// (see oci.Backend.Register's doc on why that does not panic ServeMux), so a path
// that is neither a ping/token endpoint nor a valid <name>/manifests|blobs/<ref> tail
// must still fall through oci.Backend's own 404 or -- for a path shaped like neither
// route family at all -- the `/v2/`/`/cache/` NotFoundHandlers from server.go, and
// NEVER the SPA catch-all.
func TestUnroutedCachePathsAre404OCI(t *testing.T) {
	deps := cache.Deps{Metrics: metrics.New()}
	routes := stubRoutes{ok: false}

	cases := []struct {
		name   string
		method string
		target string
	}{
		// Shaped like neither /v2/{org}/{project}/... nor
		// /cache/{org}/{project}/docker/v2/..., so oci.Backend never even sees these --
		// they must be caught by the server.go NotFoundHandlers, same as pre-M5. (A bare
		// "/v2" with no trailing slash is deliberately excluded here: ServeMux 307s it to
		// the registered "/v2/" ping, which is a redirect to a real, correct 200 -- not a
		// poisoned hit.)
		{name: "/v2/ with only an org, no project", method: http.MethodGet, target: "/v2/acme"},
		{name: "cache prefix that is not the docker mount", method: http.MethodGet, target: "/cache/acme/widget/sstate/x"},

		// Shaped like a tenant route but with a tail oci's own parser rejects (no
		// manifests/blobs marker): reaches oci.Backend, and IT answers 404 -- proven
		// by splitRef's own package tests -- but must still never be the SPA shell.
		{name: "buildkit shape, unparseable tail", method: http.MethodGet, target: "/v2/acme/widget/not-a-registry-path"},
		{name: "docker-engine shape, push path (unsupported, not a miss)", method: http.MethodPost,
			target: "/cache/acme/widget/docker/v2/library/alpine/blobs/uploads/"},
	}

	for _, headless := range []bool{false, true} {
		backend := oci.New(deps, routes, nil, nil, oci.Config{ExternalURL: "https://bakery.example.com"})

		handler := NewHandler(Config{
			Dist:          testDist(),
			Headless:      headless,
			API:           http.NotFoundHandler(),
			CacheBackends: []cache.Backend{backend},
		})

		for _, tc := range cases {
			t.Run(fmt.Sprintf("headless=%v/%s", headless, tc.name), func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s = %d, want 404", tc.method, tc.target, rec.Code)
				}

				if body := rec.Body.String(); strings.Contains(body, "<title>bakery</title>") {
					t.Errorf("headless=%v: %s %s returned the console shell -- a poisoned hit",
						headless, tc.method, tc.target)
				}

				if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
					t.Errorf("headless=%v: %s %s served Content-Type %q -- a registry client "+
						"would ingest HTML as a cache object", headless, tc.method, tc.target, ct)
				}
			})
		}
	}
}
