package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// mux builds the shape of route the real server registers: a wildcard-heavy
// pattern whose concrete paths are unbounded.
func mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("HEAD /cache/{org}/{project}/sstate/{path...}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return m
}

// gather collects every series and its label values from the registry.
func gather(t *testing.T, m *Metrics) []*dto.MetricFamily {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	return families
}

// seriesFor returns every label-value set recorded against the named metric.
func seriesFor(t *testing.T, m *Metrics, name string) []map[string]string {
	t.Helper()

	var out []map[string]string

	for _, fam := range gather(t, m) {
		if fam.GetName() != name {
			continue
		}

		for _, met := range fam.GetMetric() {
			labels := make(map[string]string, len(met.GetLabel()))
			for _, lp := range met.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}

			out = append(out, labels)
		}
	}

	return out
}

// TestHTTPMiddlewareLabelsOnThePattern is the invariant: label on r.Pattern, never
// r.URL.Path. 2000 distinct sstate URLs -- each embedding a different unihash, which
// is what a real setscene storm looks like -- must mint exactly ONE series.
// Labeling on r.URL.Path would mint 2000 and kill Prometheus inside one build.
func TestHTTPMiddlewareLabelsOnThePattern(t *testing.T) {
	m := New()
	h := m.HTTPMiddleware(mux())

	const requests = 2000

	for i := range requests {
		url := fmt.Sprintf(
			"/cache/acme/widget/sstate/aa/bb/sstate:busybox:x86_64:1.36:%064x_package.tar.zst", i)

		req := httptest.NewRequest(http.MethodHead, url, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	series := seriesFor(t, m, "bakery_http_request_duration_seconds")
	if len(series) != 1 {
		t.Fatalf("%d distinct URLs minted %d series, want 1 -- the label is not the route pattern",
			requests, len(series))
	}

	if got, want := series[0]["pattern"], "HEAD /cache/{org}/{project}/sstate/{path...}"; got != want {
		t.Errorf("pattern label = %q, want %q", got, want)
	}
}

// TestHTTPMiddlewareReadsPatternAfterNext pins the trap. ServeMux assigns r.Pattern
// DURING ServeHTTP by mutating the same *http.Request, so middleware wrapped
// outside the mux sees "" before the call and the real pattern after it. Reading it
// up front compiles, runs, and silently labels every request "".
func TestHTTPMiddlewareReadsPatternAfterNext(t *testing.T) {
	var before, after string

	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux().ServeHTTP(w, r)

		after = r.Pattern
	})

	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		before = r.Pattern

		probe.ServeHTTP(w, r)
	})

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if before != "" {
		t.Errorf("r.Pattern before next.ServeHTTP = %q, want empty", before)
	}

	if after != "GET /healthz" {
		t.Errorf("r.Pattern after next.ServeHTTP = %q, want %q", after, "GET /healthz")
	}
}

// TestHTTPMiddlewareCollapsesUnmatchedRoutes: an internet scanner hitting a
// thousand nonexistent paths must not mint a thousand series.
func TestHTTPMiddlewareCollapsesUnmatchedRoutes(t *testing.T) {
	m := New()
	h := m.HTTPMiddleware(mux())

	for i := range 500 {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/wp-admin/%d.php", i), nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	series := seriesFor(t, m, "bakery_http_request_duration_seconds")
	if len(series) != 1 {
		t.Fatalf("500 unmatched URLs minted %d series, want 1", len(series))
	}

	if got := series[0]["pattern"]; got != unmatchedPattern {
		t.Errorf("pattern label = %q, want %q", got, unmatchedPattern)
	}
}

// TestMethodLabelIsAllowListed. r.Method is FULLY attacker-controlled: net/http
// hands the handler any RFC 9110 token, so a raw "FROBNICATE /x HTTP/1.1" arrives
// with that string in r.Method. Passing it to a label is exactly as unbounded as
// r.URL.Path.
func TestMethodLabelIsAllowListed(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   string
	}{
		{name: "GET passes through", method: http.MethodGet, want: http.MethodGet},
		{name: "HEAD passes through", method: http.MethodHead, want: http.MethodHead},
		{name: "PUT passes through", method: http.MethodPut, want: http.MethodPut},
		{name: "unknown verb collapses", method: "FROBNICATE", want: "other"},
		{name: "garbage collapses", method: "AAAAAAAAAAAAAAAAAAAA", want: "other"},
		{name: "lowercase get is not GET", method: "get", want: "other"},
		{name: "empty collapses", method: "", want: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeMethod(tt.method); got != tt.want {
				t.Errorf("safeMethod(%q) = %q, want %q", tt.method, got, tt.want)
			}
		})
	}
}

// TestHTTPHistogramCarriesNoTenantLabels. The middleware sees {org} and {project}
// as ATTACKER-CONTROLLED path segments -- a request to /cache/AAAA/BBBB/sstate/x
// would mint a series per garbage slug. The tenant labels are emitted only from
// blob.Service, which never sees a raw URL segment. This asserts the label set,
// because "we just won't add them" is not an enforcement mechanism.
func TestHTTPHistogramCarriesNoTenantLabels(t *testing.T) {
	m := New()
	h := m.HTTPMiddleware(mux())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	series := seriesFor(t, m, "bakery_http_request_duration_seconds")
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}

	for _, banned := range []string{"org", "project"} {
		if _, ok := series[0][banned]; ok {
			t.Errorf("the HTTP histogram carries a %q label; it must not -- "+
				"the middleware only ever sees an unresolved URL segment", banned)
		}
	}
}

// TestNoLabelValueLeaksAKeyOrDigest is the cardinality audit. It drives the
// middleware and the headline series with realistic hostile input and then scans
// EVERY label value in the full exposition for a long hex string, an sstate key, or
// a path separator. Nothing that came off the wire may survive into a label.
func TestNoLabelValueLeaksAKeyOrDigest(t *testing.T) {
	m := New()
	h := m.HTTPMiddleware(mux())

	for i := range 500 {
		url := fmt.Sprintf("/cache/acme/widget/sstate/sstate:busybox:%064x.tar.zst", i)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, url, nil))
	}

	// The headline series, driven the way blob.Service will drive it: the labels
	// come from a RESOLVED project, so they are slugs and nothing else.
	rec := NewRecorderCache(m).Get("acme", "widget", BackendSstate, "object")
	for range 500 {
		rec.Observe(OpHead, ResultMiss)
	}

	longHex := regexp.MustCompile(`[0-9a-f]{16,}`)

	for _, fam := range gather(t, m) {
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				value := lp.GetValue()

				switch {
				case longHex.MatchString(value):
					t.Errorf("%s{%s=%q} leaks a digest or unihash into a label",
						fam.GetName(), lp.GetName(), value)
				case strings.Contains(value, "sstate:"):
					t.Errorf("%s{%s=%q} leaks a cache key into a label",
						fam.GetName(), lp.GetName(), value)
				case strings.Contains(value, "/") && lp.GetName() != "pattern":
					t.Errorf("%s{%s=%q} leaks a path into a label",
						fam.GetName(), lp.GetName(), value)
				}
			}
		}
	}
}

// TestRecorderIsBoundedAndCorrect. A Recorder eagerly resolves a 4x4 op-by-result
// table, so one (org, project, backend, kind) is exactly 16 headline series plus 4
// byte counters -- a number that does not grow with traffic.
func TestRecorderIsBoundedAndCorrect(t *testing.T) {
	m := New()
	rec := m.Recorder("acme", "widget", BackendBazel, "cas")

	for range 3 {
		rec.Observe(OpHead, ResultMiss)
	}

	rec.Observe(OpPut, ResultHit)
	rec.AddBytes(OpGet, 4096)

	series := seriesFor(t, m, "bakery_cache_requests_total")
	if len(series) != 16 {
		t.Errorf("one recorder minted %d bakery_cache_requests_total series, want 16", len(series))
	}

	got := counterValue(t, m, "bakery_cache_requests_total", map[string]string{
		"org": "acme", "project": "widget", "backend": "bazel", "kind": "cas",
		"op": "head", "result": "miss",
	})
	if got != 3 {
		t.Errorf("head/miss counter = %v, want 3", got)
	}

	got = counterValue(t, m, "bakery_cache_bytes_total", map[string]string{
		"org": "acme", "project": "widget", "backend": "bazel", "kind": "cas", "op": "get",
	})
	if got != 4096 {
		t.Errorf("get bytes counter = %v, want 4096", got)
	}
}

// TestRecorderCacheMemoizes: the same tuple must return the same Recorder, or the
// curry cost is back on the hot path and the counters are resolved twice.
func TestRecorderCacheMemoizes(t *testing.T) {
	c := NewRecorderCache(New())

	a := c.Get("acme", "widget", BackendSstate, "object")
	b := c.Get("acme", "widget", BackendSstate, "object")

	if a != b {
		t.Error("RecorderCache.Get returned a fresh Recorder for a tuple it already had")
	}

	if c.Get("acme", "widget", BackendSstate, "siginfo") == a {
		t.Error("RecorderCache.Get collided two different kinds onto one Recorder")
	}
}

// TestRegistryIsNotGlobal. New() must be callable twice in one process. Using
// prometheus.DefaultRegisterer would panic with AlreadyRegisteredError on the
// second call and leak counter state between table-driven subtests.
func TestRegistryIsNotGlobal(t *testing.T) {
	first, second := New(), New()

	first.ObserveCacheRequest("acme", "widget", BackendSstate, "object", OpHead, ResultHit)

	if got := counterValue(t, second, "bakery_cache_requests_total", map[string]string{
		"org": "acme", "project": "widget", "backend": "sstate", "kind": "object",
		"op": "head", "result": "hit",
	}); got != 0 {
		t.Errorf("a second Metrics saw the first's counter (%v) -- the registry is shared", got)
	}
}

// counterValue reads one counter out of the exposition, or 0 if the series does
// not exist.
func counterValue(t *testing.T, m *Metrics, name string, want map[string]string) float64 {
	t.Helper()

	for _, fam := range gather(t, m) {
		if fam.GetName() != name {
			continue
		}

		for _, met := range fam.GetMetric() {
			labels := make(map[string]string, len(met.GetLabel()))
			for _, lp := range met.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}

			match := true

			for k, v := range want {
				if labels[k] != v {
					match = false

					break
				}
			}

			if match {
				return met.GetCounter().GetValue()
			}
		}
	}

	return 0
}
