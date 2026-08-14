package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// OCIOp is the upstream operation. A CLOSED set: the `op` label can be nothing else.
type OCIOp string

const (
	// OCIOpResolve is a tag -> digest resolution (an upstream HEAD on a manifest).
	OCIOpResolve OCIOp = "resolve"
	// OCIOpManifest is a manifest body fetch.
	OCIOpManifest OCIOp = "manifest"
	// OCIOpBlob is a blob body fetch.
	OCIOpBlob OCIOp = "blob"
	// OCIOpStatBlob is a blob HEAD -- size only, no body. It is what answers a
	// downstream HEAD on an uncached blob, and it is the reason a HEAD never pulls
	// hundreds of megabytes (distribution's own proxy does).
	OCIOpStatBlob OCIOp = "stat_blob"
)

// OCIRefreshResult is the outcome of one stale-tag revalidation. A CLOSED set.
const (
	// OCIRefreshUnchanged: the upstream tag still names the digest we hold, and the
	// object's updated_at was touched. This is the overwhelmingly common outcome, and
	// its absence would mean the freshness touch is broken -- see blob.Service.Touch.
	OCIRefreshUnchanged = "unchanged"
	// OCIRefreshRepointed: the tag moved and the manifest was re-ingested.
	OCIRefreshRepointed = "repointed"
	// OCIRefreshError: the revalidation failed. The cached tag keeps being served
	// stale, indefinitely and deliberately -- so THIS counter is the only place the
	// outage is visible. No client will ever complain: every registry client silently
	// falls back to the real registry.
	OCIRefreshError = "error"
)

// ociCollectors are the bakery_oci_* families the pull-through proxy owns.
//
// The proxy routes every manifest, blob and tag through blob.Service, so it gets the
// headline bakery_cache_requests_total{backend="oci",kind=manifest|blob|tag} series
// for free and must NOT re-emit it. These are the series blob.Service cannot know
// about, and they all describe the UPSTREAM leg -- the half of an OCI proxy that has
// no local analogue.
//
// THE CARDINALITY RULE IS ABSOLUTE HERE. `upstream` is a NORMALIZED REGISTRY HOST
// (docker.io, ghcr.io) drawn from a per-backend ALLOWLIST, so its label space is
// bounded by operator configuration. An image name or a digest is not, and labelling
// either would mint one time series per image pulled -- the same failure as labelling
// sstate metrics on r.URL.Path, which kills Prometheus inside a single build.
type ociCollectors struct {
	upstreamRequests  *prometheus.CounterVec   // {upstream,op,code}
	upstreamErrors    *prometheus.CounterVec   // {upstream,op}
	upstreamDuration  *prometheus.HistogramVec // {upstream,op}
	ratelimitRemains  *prometheus.GaugeVec     // {upstream}
	tagRefreshResults *prometheus.CounterVec   // {result}
}

// ociBuckets span a registry round trip over the public internet, which is a
// different order of magnitude from a local index probe: the fast end is a warm
// same-region HEAD and the slow end is a cold Docker Hub manifest under a rate limit.
var ociBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

func newOCICollectors(f promauto.Factory) ociCollectors {
	return ociCollectors{
		upstreamRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_oci_upstream_requests_total",
			Help: "Requests Bakery made to an upstream registry, by HTTP status. " +
				"The denominator of the pull-through hit ratio: every one of these is a " +
				"request the local cache could not answer.",
		}, []string{"upstream", "op", "code"}),

		upstreamErrors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_oci_upstream_errors_total",
			Help: "Upstream requests that failed outright (transport error, timeout, or a " +
				"status we cannot serve). THE ONLY SIGNAL an upstream outage produces: " +
				"clients silently fall back to the real registry, so a broken Bakery " +
				"never surfaces as a failed build.",
		}, []string{"upstream", "op"}),

		upstreamDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bakery_oci_upstream_duration_seconds",
			Help:    "Latency of the upstream leg. A tag MISS pays this synchronously; a stale tag does not.",
			Buckets: ociBuckets,
		}, []string{"upstream", "op"}),

		ratelimitRemains: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_oci_upstream_ratelimit_remaining",
			Help: "Pulls remaining in the upstream's current rate-limit window, parsed from " +
				"the ratelimit-remaining response header. SERVER-SUPPLIED and never " +
				"hardcoded: Docker's docs say the window is 6 hours and the live header " +
				"says w=3600, and only the header is true.",
		}, []string{"upstream"}),

		tagRefreshResults: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_oci_tag_refresh_total",
			Help: "Stale-while-revalidate outcomes: unchanged (touched), repointed, or error. " +
				"A tag that never reports `unchanged` is a tag whose freshness touch is " +
				"broken, which makes every tag permanently stale and silently so.",
		}, []string{"result"}),
	}
}

// OCIRecorder is a PRE-RESOLVED view of the bakery_oci_* series for ONE upstream
// host, and the only way to touch them.
//
// It is resolved per upstream rather than per (org, project) because these describe
// the upstream leg: Docker Hub's rate limit is shared by every tenant on this server,
// and splitting it per project would make the one number an operator actually needs
// -- "how close are we to the limit" -- unreadable.
type OCIRecorder struct {
	upstream string

	requests *prometheus.CounterVec // curried on upstream: {op,code}
	errors   *prometheus.CounterVec // curried on upstream: {op}
	duration prometheus.ObserverVec // curried on upstream: {op}
	remain   prometheus.Gauge

	refreshUnchanged prometheus.Counter
	refreshRepointed prometheus.Counter
	refreshError     prometheus.Counter
}

// OCI resolves the bakery_oci_* collectors for one NORMALIZED upstream host.
//
// The host must already be normalized (docker.io, not registry-1.docker.io) and must
// have come from the backend's allowlist -- an ?ns= value straight off the wire is
// attacker-controlled and would be an unbounded label.
func (m *Metrics) OCI(upstream string) *OCIRecorder {
	t := prometheus.Labels{"upstream": upstream}

	return &OCIRecorder{
		upstream:         upstream,
		requests:         m.oci.upstreamRequests.MustCurryWith(t),
		errors:           m.oci.upstreamErrors.MustCurryWith(t),
		duration:         m.oci.upstreamDuration.MustCurryWith(t),
		remain:           m.oci.ratelimitRemains.With(t),
		refreshUnchanged: m.oci.tagRefreshResults.WithLabelValues(OCIRefreshUnchanged),
		refreshRepointed: m.oci.tagRefreshResults.WithLabelValues(OCIRefreshRepointed),
		refreshError:     m.oci.tagRefreshResults.WithLabelValues(OCIRefreshError),
	}
}

// Upstream records one completed upstream request: its status, its latency, and --
// when it failed outright -- the error counter that is the outage's only witness.
//
// code 0 means "no HTTP response at all" (transport error, timeout, DNS). It is
// recorded as the literal string "0" rather than dropped, because a Hub that has
// stopped answering and a Hub that answers 429 are different operational problems and
// collapsing them loses the distinction.
func (r *OCIRecorder) Upstream(op OCIOp, code int, d time.Duration, err error) {
	r.requests.WithLabelValues(string(op), strconv.Itoa(code)).Inc()
	r.duration.WithLabelValues(string(op)).Observe(d.Seconds())

	if err != nil {
		r.errors.WithLabelValues(string(op)).Inc()
	}
}

// RateLimitRemaining publishes the pulls left in the upstream's current window.
// Call it only with a value actually parsed from a response header.
func (r *OCIRecorder) RateLimitRemaining(n float64) { r.remain.Set(n) }

// TagRefresh records one stale-while-revalidate outcome.
func (r *OCIRecorder) TagRefresh(result string) {
	switch result {
	case OCIRefreshUnchanged:
		r.refreshUnchanged.Inc()
	case OCIRefreshRepointed:
		r.refreshRepointed.Inc()
	default:
		r.refreshError.Inc()
	}
}
