// Package metrics owns the Prometheus registry and every series Bakery emits.
//
// There are NO package-level collectors and no use of the default registry.
// Metrics is a dependency: construct one with New and pass it to the components
// that need it (blob.Service, the HTTP middleware, storage, auth). Tests construct
// their own, so a table-driven run can assert on counter deltas with no cross-test
// interference and no AlreadyRegisteredError panic.
//
// # The cardinality rule, and where it is actually enforced
//
// CLAUDE.md: "Prometheus labels use slugs, never keys or digests." Two structural
// consequences that are easy to get wrong and impossible to notice at review time:
//
//  1. The HTTP histogram is labeled on r.Pattern -- the registered route TEMPLATE,
//     wildcards unsubstituted -- and NEVER on r.URL.Path. The latter mints one time
//     series per sstate object and kills Prometheus inside a single build.
//
//  2. The HTTP histogram carries NO org or project label, and that is not an
//     oversight. The middleware sees {org} and {project} as attacker-controlled
//     path segments: a request to /cache/AAAA/BBBB/sstate/x would mint a series per
//     garbage slug. org and project are emitted ONLY from blob.Service, which never
//     sees a raw URL segment -- only a project already resolved to a DB row. That is
//     the structural reason the headline series lives in blob.Service and not here.
//
// # The performance rule
//
// prometheus' WithLabelValues hashes the label values and locks the metric map on
// every call. On the 6-label headline vec that is ~95ns -- about the same cost as
// the entire LRU-hit lookup it instruments (~123ns), so naive instrumentation
// roughly DOUBLES the sstate HEAD path. Do not call WithLabelValues on a hot path:
// take a Recorder (recorder.go), which resolves the counters once per
// (org, project, backend, kind) and costs ~7ns per Inc.
package metrics

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Op is the normalized blob operation. A CLOSED set: adding a value is a
// deliberate act, and that is what keeps the `op` label bounded.
type Op string

const (
	OpGet    Op = "get"
	OpPut    Op = "put"
	OpHead   Op = "head"
	OpExists Op = "exists"
)

// Result is the normalized outcome of a blob operation. A CLOSED set.
//
// The mapping is not obvious for writes, so it is pinned here rather than left to
// each backend to guess:
//
//	get / head / exists:
//	    hit    served from the cache (LRU or store)
//	    miss   not present. sstate MUST render this 404, never 403: BitBake retries
//	           a 403 as a full-body GET.
//	    stale  present but past its freshness horizon (OCI stale-while-revalidate)
//	    error  storage or DB failure
//
//	put:
//	    hit    content was ALREADY PRESENT; dedup elided the byte write
//	    miss   bytes were newly written
//	    error  storage or DB failure
//
// `stale` is not legal for put or exists.
type Result string

const (
	ResultHit   Result = "hit"
	ResultMiss  Result = "miss"
	ResultStale Result = "stale"
	ResultError Result = "error"
)

// Backend is the cache backend family. A CLOSED set, and it mirrors the
// backend_kind enum in the schema.
type Backend string

const (
	BackendSstate    Backend = "sstate"
	BackendDownloads Backend = "downloads"
	BackendHashserv  Backend = "hashserv"
	BackendBazel     Backend = "bazel"
	BackendOCI       Backend = "oci"
)

// Storage driver labels. M1 ships local only; S3 is deferred.
const (
	DriverLocal = "local"
)

// LRU event labels.
const (
	EventHit   = "hit"
	EventMiss  = "miss"
	EventAdd   = "add"
	EventEvict = "evict"
)

// unmatchedPattern is the sentinel the HTTP middleware uses when no route
// matched (404 / 405). Collapsing them to one series is the difference between one
// time series and one per bogus URL an internet scanner tries.
const unmatchedPattern = "<unmatched>"

// Metrics holds every collector Bakery emits, plus the registry they live in.
//
// The CounterVec and HistogramVec label shapes below are a CONTRACT. blob.Service
// and the HTTP middleware consume them; changing a label set is a breaking change
// to every dashboard and alert built on it.
type Metrics struct {
	reg *prometheus.Registry

	// THE HEADLINE SERIES.
	//
	//	bakery_cache_requests_total{org,project,backend,kind,op,result}
	//	bakery_cache_bytes_total{org,project,backend,kind,op}
	//
	// Emitted from blob.Service, so every future backend is normalized for free --
	// and so org/project are only ever labels that came from a resolved DB row.
	//
	// `kind` is the sub-namespace within a backend: sstate -> object|siginfo,
	// downloads -> file, bazel -> ac|cas, oci -> blob|manifest. hashserv does not
	// route through blob.Service and emits its own bakery_hashserv_* series.
	//
	// DO NOT call WithLabelValues on these from a hot path. Use Recorder.
	CacheRequests *prometheus.CounterVec
	CacheBytes    *prometheus.CounterVec

	// Storage. bakery_storage_objects / _bytes are per-project gauges; the
	// operation counters and the latency histogram are per-DRIVER and carry no
	// project (they measure the store, not the tenant).
	StorageObjects *prometheus.GaugeVec     // {org,project,backend}
	StorageBytes   *prometheus.GaugeVec     // {org,project,backend}
	StorageOps     *prometheus.CounterVec   // {driver,op,result}
	StorageLatency *prometheus.HistogramVec // {driver,op}

	// LRU. Deliberately NO org/project: the LRU is process-global and keying it per
	// project would multiply series for no analytic gain.
	LRUEvents  *prometheus.CounterVec // {event}
	LRUEntries prometheus.Gauge
	LRUBytes   prometheus.Gauge

	// Singleflight -- the HEAD-storm dedup signal. shared=true means the caller rode
	// an in-flight lookup instead of issuing its own query.
	SingleflightCalls    *prometheus.CounterVec // {shared}
	SingleflightInFlight prometheus.Gauge

	// The series the HEAD benchmark gates on. Labeled on the sqlc QUERY NAME, which
	// is a compile-time-bounded set. An LRU hit must add ZERO to this.
	DBQueries *prometheus.CounterVec // {query}

	// Auth.
	AuthAttempts *prometheus.CounterVec // {method,result}
	AuthKeyAge   prometheus.Histogram

	// HTTP. Labeled on the route PATTERN -- a template, never a concrete path -- and
	// on an ALLOW-LISTED method. r.Method is fully attacker-controlled: a raw
	// "AAAAAAAAAAAA /path HTTP/1.1" reaches the handler with that string in r.Method,
	// so passing it straight to a label is exactly as unbounded as r.URL.Path.
	HTTPDuration *prometheus.HistogramVec // {pattern,method,code}
	HTTPInFlight prometheus.Gauge
}

// httpBuckets start at 100us. prometheus' DefBuckets start at 5ms, which would
// collapse the entire sstate HEAD hot path into a single bucket and make the
// histogram useless for the one thing it exists to measure.
var httpBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025,
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// storageBuckets span a disk write, which is a different order of magnitude from
// an index probe.
var storageBuckets = []float64{
	0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30,
}

// New builds a Metrics backed by its own private registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return &Metrics{
		reg: reg,

		CacheRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_cache_requests_total",
			Help: "Cache operations by backend and outcome.",
		}, []string{"org", "project", "backend", "kind", "op", "result"}),

		CacheBytes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_cache_bytes_total",
			Help: "Bytes served from and written to the cache.",
		}, []string{"org", "project", "backend", "kind", "op"}),

		StorageObjects: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_storage_objects",
			Help: "Objects currently stored.",
		}, []string{"org", "project", "backend"}),

		StorageBytes: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_storage_bytes",
			Help: "Bytes currently stored.",
		}, []string{"org", "project", "backend"}),

		StorageOps: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_storage_operations_total",
			Help: "storage.Store calls by driver, operation and outcome.",
		}, []string{"driver", "op", "result"}),

		StorageLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bakery_storage_operation_duration_seconds",
			Help:    "storage.Store call latency.",
			Buckets: storageBuckets,
		}, []string{"driver", "op"}),

		LRUEvents: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_lru_events_total",
			Help: "In-process metadata LRU events.",
		}, []string{"event"}),

		LRUEntries: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_lru_entries",
			Help: "Entries resident in the metadata LRU.",
		}),

		LRUBytes: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_lru_bytes",
			Help: "Approximate heap bytes held by the metadata LRU.",
		}),

		SingleflightCalls: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_singleflight_calls_total",
			Help: "blob.Service lookups routed through singleflight. " +
				"shared=true means the caller rode an in-flight lookup instead of issuing its own.",
		}, []string{"shared"}),

		SingleflightInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_singleflight_inflight",
			Help: "Distinct keys with a lookup in flight.",
		}),

		DBQueries: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_db_queries_total",
			Help: "Queries issued to Postgres, by sqlc query name. " +
				"The HEAD benchmark gates on this: an LRU hit must add zero.",
		}, []string{"query"}),

		AuthAttempts: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_auth_attempts_total",
			Help: "Authentication attempts by presentation and outcome.",
		}, []string{"method", "result"}),

		AuthKeyAge: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "bakery_auth_api_key_age_days",
			Help:    "Age of API keys at time of use.",
			Buckets: []float64{1, 7, 30, 90, 180, 365, 730},
		}),

		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bakery_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern.",
			Buckets: httpBuckets,
		}, []string{"pattern", "method", "code"}),

		HTTPInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
}

// Registry exposes the Gatherer, for the /metrics handler and for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the exposition format.
//
// Mount it ONLY on the private metrics listener (--metrics-addr, loopback by
// default). /metrics leaks every org and project slug and their stored byte counts,
// so exposing it publicly must be an explicit act.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		EnableOpenMetrics:   true,
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	})
}

// ObserveCacheRequest is the COLD-path entry point for the headline series --
// convenient, and it pays a full WithLabelValues label lookup. On the sstate HEAD
// path use Recorder.Observe instead.
func (m *Metrics) ObserveCacheRequest(org, project string, b Backend, kind string, op Op, res Result) {
	m.CacheRequests.WithLabelValues(org, project, string(b), kind, string(op), string(res)).Inc()
}

// RegisterPool registers a collector over pool.Stat() into m's registry.
//
// Hand-written on purpose: collectors.NewDBStatsCollector takes a *sql.DB, and
// database/sql is banned here. Call it once, after the pool is constructed --
// Metrics is built before the pool, so this cannot happen in New.
//
// bakery_db_pool_empty_acquires_total rising under a HEAD storm is the signal that
// the pool, not Postgres, is the bottleneck.
func (m *Metrics) RegisterPool(pool *pgxpool.Pool) error {
	if err := m.reg.Register(newPoolCollector(pool)); err != nil {
		return fmt.Errorf("register pgxpool collector: %w", err)
	}

	return nil
}

// --- pgxpool collector -------------------------------------------------------

type poolCollector struct {
	pool *pgxpool.Pool

	acquiredConns     *prometheus.Desc
	idleConns         *prometheus.Desc
	totalConns        *prometheus.Desc
	maxConns          *prometheus.Desc
	constructingConns *prometheus.Desc
	acquireCount      *prometheus.Desc
	acquireDuration   *prometheus.Desc
	emptyAcquireCount *prometheus.Desc
	canceledAcquire   *prometheus.Desc
	newConnsCount     *prometheus.Desc
}

func newPoolCollector(pool *pgxpool.Pool) *poolCollector {
	return &poolCollector{
		pool: pool,
		acquiredConns: prometheus.NewDesc("bakery_db_pool_acquired_conns",
			"Connections currently acquired from the pool.", nil, nil),
		idleConns: prometheus.NewDesc("bakery_db_pool_idle_conns",
			"Idle connections in the pool.", nil, nil),
		totalConns: prometheus.NewDesc("bakery_db_pool_total_conns",
			"Total connections in the pool.", nil, nil),
		maxConns: prometheus.NewDesc("bakery_db_pool_max_conns",
			"Configured pool ceiling.", nil, nil),
		constructingConns: prometheus.NewDesc("bakery_db_pool_constructing_conns",
			"Connections currently being constructed.", nil, nil),
		acquireCount: prometheus.NewDesc("bakery_db_pool_acquires_total",
			"Cumulative successful acquires.", nil, nil),
		acquireDuration: prometheus.NewDesc("bakery_db_pool_acquire_duration_seconds_total",
			"Cumulative time blocked waiting to acquire a connection.", nil, nil),
		emptyAcquireCount: prometheus.NewDesc("bakery_db_pool_empty_acquires_total",
			"Acquires that had to wait for an empty pool. "+
				"Rising under a HEAD storm means the pool is the bottleneck.", nil, nil),
		canceledAcquire: prometheus.NewDesc("bakery_db_pool_canceled_acquires_total",
			"Acquires canceled by context.", nil, nil),
		newConnsCount: prometheus.NewDesc("bakery_db_pool_new_conns_total",
			"Cumulative new connections opened.", nil, nil),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.acquiredConns, c.idleConns, c.totalConns, c.maxConns, c.constructingConns,
		c.acquireCount, c.acquireDuration, c.emptyAcquireCount, c.canceledAcquire,
		c.newConnsCount,
	} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()

	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}

	gauge(c.acquiredConns, float64(s.AcquiredConns()))
	gauge(c.idleConns, float64(s.IdleConns()))
	gauge(c.totalConns, float64(s.TotalConns()))
	gauge(c.maxConns, float64(s.MaxConns()))
	gauge(c.constructingConns, float64(s.ConstructingConns()))
	counter(c.acquireCount, float64(s.AcquireCount()))
	counter(c.acquireDuration, s.AcquireDuration().Seconds())
	counter(c.emptyAcquireCount, float64(s.EmptyAcquireCount()))
	counter(c.canceledAcquire, float64(s.CanceledAcquireCount()))
	counter(c.newConnsCount, float64(s.NewConnsCount()))
}

// --- HTTP middleware ---------------------------------------------------------

type responseRecorder struct {
	http.ResponseWriter

	status int
	wrote  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}

	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}

	n, err := r.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("write response: %w", err)
	}

	return n, nil
}

// knownMethods is an allow-list, not a formality. See the note on
// Metrics.HTTPDuration.
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPut: {},
	http.MethodPost: {}, http.MethodPatch: {}, http.MethodDelete: {},
	http.MethodOptions: {}, http.MethodConnect: {}, http.MethodTrace: {},
}

func safeMethod(m string) string {
	if _, ok := knownMethods[m]; ok {
		return m
	}

	return "other"
}

// HTTPMiddleware records latency and status, labeled on the route PATTERN.
//
// THE TRAP: ServeMux assigns r.Pattern DURING ServeHTTP, by mutating the same
// *http.Request pointer. In middleware wrapped OUTSIDE the mux -- which is where
// this runs -- r.Pattern is therefore EMPTY before next.ServeHTTP and only
// populated after it returns. Snapshotting it up front compiles, runs, passes a
// naive smoke test, and silently labels every single request "".
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		m.HTTPInFlight.Inc()
		defer m.HTTPInFlight.Dec()

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK, wrote: false}

		next.ServeHTTP(rec, r)

		// AFTER, never before.
		pattern := r.Pattern
		if pattern == "" {
			pattern = unmatchedPattern
		}

		m.HTTPDuration.WithLabelValues(
			pattern,
			safeMethod(r.Method),
			strconv.Itoa(rec.status),
		).Observe(time.Since(start).Seconds())
	})
}
