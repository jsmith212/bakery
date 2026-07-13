package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Recorder is a PRE-RESOLVED view of the headline series for one
// (org, project, backend, kind) tuple.
//
// WHY IT EXISTS. prometheus' WithLabelValues hashes the label values and takes a
// lock on the metric map on EVERY call. Measured on the 6-label
// bakery_cache_requests_total vec that is ~95ns -- about the cost of the entire
// LRU-hit lookup it is instrumenting (~123ns). Two of them per Exists() roughly
// DOUBLES the sstate HEAD path, which is the one path this whole project is judged
// on. Resolving the counters once costs ~7ns per Inc instead: a 13x reduction, and
// it takes metrics from ~50% of the HEAD path down to ~5%.
//
// The label values are fixed for the lifetime of a project, so there is nothing to
// recompute per request. Resolve once (RecorderCache), Inc forever.
type Recorder struct {
	// [op][result] -> counter. Both dimensions are closed sets, so this is a fixed
	// 4x4 table resolved eagerly at construction. 16 series per
	// (org, project, backend, kind).
	counters [4][4]prometheus.Counter
	bytes    [4]prometheus.Counter

	lruHit   prometheus.Counter
	lruMiss  prometheus.Counter
	lruAdd   prometheus.Counter
	lruEvict prometheus.Counter

	sfShared prometheus.Counter
	sfSolo   prometheus.Counter
}

var (
	allOps     = [4]Op{OpGet, OpPut, OpHead, OpExists}
	allResults = [4]Result{ResultHit, ResultMiss, ResultStale, ResultError}
)

// opIndex and resultIndex fold an unknown value onto the last slot rather than
// panicking. A metrics call must never be able to take down a cache request.
func opIndex(o Op) int {
	switch o {
	case OpGet:
		return 0
	case OpPut:
		return 1
	case OpHead:
		return 2
	case OpExists:
		return 3
	}

	return 3
}

func resultIndex(r Result) int {
	switch r {
	case ResultHit:
		return 0
	case ResultMiss:
		return 1
	case ResultStale:
		return 2
	case ResultError:
		return 3
	}

	return 3
}

// Recorder resolves the headline counters for one project+backend+kind.
//
// Call it once when the project is loaded, NOT per request -- or better, go through
// RecorderCache, which memoizes it for you.
func (m *Metrics) Recorder(org, project string, b Backend, kind string) *Recorder {
	labels := prometheus.Labels{
		"org": org, "project": project, "backend": string(b), "kind": kind,
	}

	reqs := m.CacheRequests.MustCurryWith(labels)
	byts := m.CacheBytes.MustCurryWith(labels)

	r := &Recorder{
		counters: [4][4]prometheus.Counter{},
		bytes:    [4]prometheus.Counter{},
		lruHit:   m.LRUEvents.WithLabelValues(EventHit),
		lruMiss:  m.LRUEvents.WithLabelValues(EventMiss),
		lruAdd:   m.LRUEvents.WithLabelValues(EventAdd),
		lruEvict: m.LRUEvents.WithLabelValues(EventEvict),
		sfShared: m.SingleflightCalls.WithLabelValues("true"),
		sfSolo:   m.SingleflightCalls.WithLabelValues("false"),
	}

	for i, op := range allOps {
		r.bytes[i] = byts.WithLabelValues(string(op))

		for j, res := range allResults {
			r.counters[i][j] = reqs.WithLabelValues(string(op), string(res))
		}
	}

	return r
}

// Observe is the hot-path call: two array indexes and one atomic add.
func (r *Recorder) Observe(op Op, res Result) {
	r.counters[opIndex(op)][resultIndex(res)].Inc()
}

// AddBytes records payload bytes moved by op.
func (r *Recorder) AddBytes(op Op, n int64) {
	r.bytes[opIndex(op)].Add(float64(n))
}

// LRU records a metadata-cache hit or miss.
func (r *Recorder) LRU(hit bool) {
	if hit {
		r.lruHit.Inc()

		return
	}

	r.lruMiss.Inc()
}

// LRUAdd records an insertion into the metadata cache.
func (r *Recorder) LRUAdd() { r.lruAdd.Inc() }

// LRUEvict records an eviction from the metadata cache.
func (r *Recorder) LRUEvict() { r.lruEvict.Inc() }

// Singleflight records whether a lookup rode an in-flight call (shared) or issued
// its own. Pass the third return value of singleflight.Group.Do.
func (r *Recorder) Singleflight(shared bool) {
	if shared {
		r.sfShared.Inc()

		return
	}

	r.sfSolo.Inc()
}

// RecorderCache memoizes Recorders so the curry-and-resolve cost is paid once per
// (org, project, backend, kind) rather than per request.
//
// It is safe for concurrent use, and the read path takes only an RLock -- it sits
// in front of the BB_NUMBER_THREADS-parallel HEAD storm.
type RecorderCache struct {
	m *Metrics

	mu sync.RWMutex
	rs map[string]*Recorder
}

// NewRecorderCache returns an empty cache over m.
func NewRecorderCache(m *Metrics) *RecorderCache {
	return &RecorderCache{m: m, mu: sync.RWMutex{}, rs: make(map[string]*Recorder)}
}

// Get returns the memoized Recorder for the tuple, resolving it on first use.
func (c *RecorderCache) Get(org, project string, b Backend, kind string) *Recorder {
	key := org + "\x00" + project + "\x00" + string(b) + "\x00" + kind

	c.mu.RLock()
	r, ok := c.rs[key]
	c.mu.RUnlock()

	if ok {
		return r
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if r, ok := c.rs[key]; ok {
		return r
	}

	r = c.m.Recorder(org, project, b, kind)
	c.rs[key] = r

	return r
}
