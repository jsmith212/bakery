package httpblob

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jsmith212/bakery/internal/cache"
)

// mount registers an sstate backend on a real ServeMux and returns the mux, so every
// test drives the REAL routing (path... decoding, r.PathValue, r.Pattern) and the REAL
// handler -- never a stand-in.
func mount(t *testing.T, blobs *testBlobs, route cache.Route, authn Authenticator) http.Handler {
	t.Helper()

	mux := http.NewServeMux()
	NewSstate(blobs.deps(), fakeResolver{route: route, found: true}, authn).Register(mux)

	return mux
}

const sstatePath = "/cache/acme/widget/sstate/universal/aa/bb/sstate:zlib:x86_64.tar.zst"

// decodedKey is what r.PathValue("path") yields for sstatePath: the tail after
// .../sstate/, colon already decoded.
const decodedKey = "universal/aa/bb/sstate:zlib:x86_64.tar.zst"

// TestHEADHit is the hot path. A hit is 200 + Content-Length + an EMPTY body, and it
// is served from Stat WITHOUT ever opening the object bytes -- the HEAD storm must not
// stream multi-GB files. The store's Get counter proves it.
func TestHEADHit(t *testing.T) {
	blobs := newTestBlobs(t)
	body := []byte("0123456789ABCDEFGHIJ")
	blobs.seed(t, decodedKey, body)

	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD hit status = %d, want 200", rec.Code)
	}

	if got := rec.Header().Get("Content-Length"); got != "20" {
		t.Errorf("Content-Length = %q, want 20", got)
	}

	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", rec.Header().Get("Accept-Ranges"))
	}

	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want empty", rec.Body.Len())
	}

	if got := blobs.store.gets.Load(); got != 0 {
		t.Errorf("HEAD opened the object bytes %d time(s); it must be served from Stat", got)
	}
}

// TestHEADMiss: a miss is 404, NEVER 403 and never a 200 with an empty body. BitBake
// retries a 403 as a full-body GET, turning the HEAD storm into a GET storm.
func TestHEADMiss(t *testing.T) {
	blobs := newTestBlobs(t) // nothing seeded
	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD miss status = %d, want 404 (never 403, never 200)", rec.Code)
	}

	if blobs.store.gets.Load() != 0 {
		t.Error("HEAD miss opened the object bytes")
	}
}

// TestGET covers the whole ServeContent seam over a real *os.File: full body, a
// satisfiable Range -> 206 with the exact partial bytes, and an unsatisfiable Range
// -> 416.
func TestGET(t *testing.T) {
	blobs := newTestBlobs(t)
	body := []byte("0123456789ABCDEFGHIJ") // 20 bytes
	blobs.seed(t, decodedKey, body)

	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	t.Run("full body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sstatePath, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", rec.Code)
		}

		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("GET body = %q, want %q", rec.Body.Bytes(), body)
		}
	})

	t.Run("range 206", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, sstatePath, nil)
		r.Header.Set("Range", "bytes=10-")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusPartialContent {
			t.Fatalf("range status = %d, want 206", rec.Code)
		}

		if got := rec.Header().Get("Content-Range"); got != "bytes 10-19/20" {
			t.Errorf("Content-Range = %q, want bytes 10-19/20", got)
		}

		if got := rec.Body.String(); got != "ABCDEFGHIJ" {
			t.Errorf("range body = %q, want ABCDEFGHIJ", got)
		}
	})

	t.Run("unsatisfiable range 416", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, sstatePath, nil)
		r.Header.Set("Range", "bytes=99999-")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("unsatisfiable range status = %d, want 416", rec.Code)
		}
	})
}

// TestPercentEncodedColon: bitbake sends the sstate colon percent-encoded. The mux
// decodes it, and the handler routes AND stores on the DECODED key -- so an object
// seeded under the decoded key is served whether the request encodes the colon or not.
func TestPercentEncodedColon(t *testing.T) {
	blobs := newTestBlobs(t)
	blobs.seed(t, decodedKey, []byte("payload"))

	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	const encoded = "/cache/acme/widget/sstate/universal/aa/bb/sstate%3Azlib%3Ax86_64.tar.zst"

	for _, target := range []string{encoded, sstatePath} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, target, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("HEAD %q status = %d, want 200 (decoded-key routing)", target, rec.Code)
		}
	}
}

// TestMetricsLabelFromRoute: a GET of TWO different sstate object keys must produce ONE
// time series on bakery_cache_requests_total, not one per object. The label set comes
// from Route.Ref (org/project/backend/kind), never from the URL path.
func TestMetricsLabelFromRoute(t *testing.T) {
	blobs := newTestBlobs(t)
	k1 := "universal/aa/bb/sstate:zlib:x86_64.tar.zst"
	k2 := "universal/cc/dd/sstate:busybox:aarch64.tar.zst"
	blobs.seed(t, k1, []byte("one"))
	blobs.seed(t, k2, []byte("two"))

	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	for _, k := range []string{k1, k2} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cache/acme/widget/sstate/"+k, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %q = %d, want 200", k, rec.Code)
		}
	}

	// The cardinality bound: two DIFFERENT object keys must collapse to ONE get/hit
	// series (value 2), and the object key must appear in NO label value. Labeling
	// from r.URL.Path instead would mint one series per key -- the failure Route.Ref
	// exists to prevent.
	hits, value := getHitSeries(t, blobs.metrics.Registry())

	if len(hits) != 1 {
		t.Fatalf("bakery_cache_requests_total{op=get,result=hit} has %d series, want 1 (two objects, one label set)", len(hits))
	}

	if value != 2 {
		t.Errorf("get/hit series value = %v, want 2", value)
	}

	want := map[string]string{
		"org": "acme", "project": "widget", "backend": "sstate", "kind": "object",
		"op": "get", "result": "hit",
	}
	for name, got := range hits[0] {
		if want[name] != got {
			t.Errorf("label %s = %q, want %q", name, got, want[name])
		}

		if strings.Contains(got, "zlib") || strings.Contains(got, "busybox") ||
			strings.Contains(got, "universal") || strings.Contains(got, "/") {
			t.Errorf("label %s = %q leaks the object key into the metric labels", name, got)
		}
	}
}

// getHitSeries returns the label sets and value of every bakery_cache_requests_total
// series with op=get,result=hit. A distinct label set is a distinct series, so one
// entry here for two different object keys is exactly the cardinality bound.
func getHitSeries(t *testing.T, reg *prometheus.Registry) ([]map[string]string, float64) {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var (
		out   []map[string]string
		value float64
	)

	for _, fam := range families {
		if fam.GetName() != "bakery_cache_requests_total" {
			continue
		}

		for _, mtr := range fam.GetMetric() {
			labels := map[string]string{}
			for _, lbl := range mtr.GetLabel() {
				labels[lbl.GetName()] = lbl.GetValue()
			}

			if labels["op"] == "get" && labels["result"] == "hit" {
				out = append(out, labels)
				value += mtr.GetCounter().GetValue()
			}
		}
	}

	return out, value
}

// TestRegisterDoesNotPanic mounts BOTH cache backends beside the actual server route
// shapes -- GET /healthz, GET /readyz, the methodless /api/v1/ and the methodless SPA /
// -- and asserts registration does not panic. The literal 4th segment and the enumerated
// verbs are what make that true; a wildcard {kind} there would panic.
func TestRegisterDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registration panicked: %v", r)
		}
	}()

	blobs := newTestBlobs(t)
	mux := http.NewServeMux()

	// The server's own route shapes.
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /readyz", func(http.ResponseWriter, *http.Request) {})
	mux.Handle("/api/v1/", http.NotFoundHandler())
	mux.Handle("/", http.NotFoundHandler())

	NewSstate(blobs.deps(), fakeResolver{}, &fakeAuthenticator{}).Register(mux)
	NewDownloads(blobs.deps(), fakeResolver{}, &fakeAuthenticator{}).Register(mux)
}

// TestBadKey: a traversal / bad-grammar key is a 400, decided by the handler after the
// route resolves.
func TestBadKey(t *testing.T) {
	blobs := newTestBlobs(t)
	h := mount(t, blobs, testRoute(), &fakeAuthenticator{})

	// ..%2F..%2Fetc decodes to ../../etc AFTER the mux segments the raw path, so it
	// reaches the handler as a traversal the validator must reject.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cache/acme/widget/sstate/..%2F..%2Fetc", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal key status = %d, want 400", rec.Code)
	}
}

// TestDisabledOrUnknownRouteIs404: a disabled backend and an unresolved route both look
// absent -- 404, never a 5xx, never a hint.
func TestDisabledOrUnknownRouteIs404(t *testing.T) {
	blobs := newTestBlobs(t)

	t.Run("disabled backend", func(t *testing.T) {
		route := testRoute()
		route.Enabled = false
		h := mount(t, blobs, route, &fakeAuthenticator{})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("disabled backend status = %d, want 404", rec.Code)
		}
	})

	t.Run("unresolved route", func(t *testing.T) {
		mux := http.NewServeMux()
		NewSstate(blobs.deps(), fakeResolver{found: false}, &fakeAuthenticator{}).Register(mux)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("unresolved route status = %d, want 404", rec.Code)
		}
	})
}

// TestReadAuth exercises the ReadAuthRequired contract end to end through the handler.
func TestReadAuth(t *testing.T) {
	body := []byte("payload")

	t.Run("open backend serves anonymously without authenticating", func(t *testing.T) {
		blobs := newTestBlobs(t)
		blobs.seed(t, decodedKey, body)

		authn := &fakeAuthenticator{err: errUnauth{}} // would fail if ever consulted
		route := testRoute()                          // ReadAuthRequired == false
		h := mount(t, blobs, route, authn)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("open backend HEAD = %d, want 200", rec.Code)
		}

		if authn.calls.Load() != 0 {
			t.Error("open backend authenticated the request; it must not")
		}
	})

	t.Run("auth-required with no credential is 401 + WWW-Authenticate Basic", func(t *testing.T) {
		blobs := newTestBlobs(t)
		blobs.seed(t, decodedKey, body)

		route := testRoute()
		route.ReadAuthRequired = true
		h := mount(t, blobs, route, &fakeAuthenticator{err: errUnauth{}})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, sstatePath, nil))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("no-credential status = %d, want 401", rec.Code)
		}

		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="bakery"` {
			t.Errorf("WWW-Authenticate = %q, want Basic realm=\"bakery\"", got)
		}
	})

	t.Run("auth-required with a valid read-granted key serves", func(t *testing.T) {
		blobs := newTestBlobs(t)
		blobs.seed(t, decodedKey, body)

		route := testRoute()
		route.ReadAuthRequired = true
		authn := &fakeAuthenticator{principal: fakePrincipal{canRead: true}}
		h := mount(t, blobs, route, authn)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sstatePath, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("valid-key GET = %d, want 200", rec.Code)
		}

		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("body = %q, want %q", rec.Body.Bytes(), body)
		}
	})

	t.Run("auth-required with a valid-but-unauthorized key is 401, not 403", func(t *testing.T) {
		blobs := newTestBlobs(t)
		blobs.seed(t, decodedKey, body)

		route := testRoute()
		route.ReadAuthRequired = true
		// Authenticated fine, but the key grants no read on this project.
		authn := &fakeAuthenticator{principal: fakePrincipal{canRead: false}}
		h := mount(t, blobs, route, authn)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sstatePath, nil))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized-key status = %d, want 401 (403 would trigger bitbake's GET fallback)", rec.Code)
		}
	})

	t.Run("the token never appears in a handler log line", func(t *testing.T) {
		var buf bytes.Buffer
		blobs := newTestBlobs(t)
		blobs.seed(t, decodedKey, body)

		deps := blobs.deps()
		deps.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		route := testRoute()
		route.ReadAuthRequired = true
		const token = "bkry_supersecrettokenvalue012345678901234567"
		authn := &fakeAuthenticator{principal: fakePrincipal{canRead: true}}

		mux := http.NewServeMux()
		b := &Backend{
			kind: route.Kind, seg: "sstate", tail: "path", policy: SstatePolicy,
			deps: deps, routes: fakeResolver{route: route, found: true}, authn: authn,
		}
		b.Register(mux)

		r := httptest.NewRequest(http.MethodGet, sstatePath, nil)
		r.SetBasicAuth("bkry", token)

		mux.ServeHTTP(httptest.NewRecorder(), r)

		if strings.Contains(buf.String(), token) {
			t.Error("the credential token leaked into a handler log line")
		}
	})
}

// errUnauth stands in for auth.ErrUnauthenticated at the handler boundary: the handler
// treats any non-nil error identically (401), so the exact type is irrelevant here.
type errUnauth struct{}

func (errUnauth) Error() string { return "unauthenticated" }
