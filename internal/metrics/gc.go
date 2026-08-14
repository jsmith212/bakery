package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// GCReason is why the sweep deleted an object. A CLOSED set, and the distinction is
// the one an operator actually asks about after a cache shrinks: retention means the
// window elapsed, quota means the backend was over its cap and the object lost the
// eviction order, unreachable means the sstate root derivation found no surviving
// unihash for it.
type GCReason string

const (
	GCReasonRetention   GCReason = "retention"
	GCReasonQuota       GCReason = "quota"
	GCReasonUnreachable GCReason = "unreachable"
)

// gcCollectors are the bakery_gc_* families plus the two storage gauges the GC is
// the FIRST writer of (spec §9.9).
//
// Every label here is a slug, a backend KIND or a closed-set word. A digest, a
// cache key, a unihash or a backend id would each mint an unbounded label space --
// the same failure as labelling HTTP metrics on r.URL.Path.
//
// bakery_gc_sstate_unihash_coverage is the series that makes spec §5's silent
// degradation visible: with no hashserv data the sstate policy collapses to
// age-only retention and looks completely healthy while doing something else than
// what it says. A coverage of 0 on a backend whose paired hashserv holds rows is
// the shape that makes the engine refuse to sweep at all.
type gcCollectors struct {
	runs           *prometheus.CounterVec // {status,trigger}
	runDuration    prometheus.Histogram
	lastRun        prometheus.Gauge
	objectsDeleted *prometheus.CounterVec // {backend,namespace,reason}
	blobsMarked    prometheus.Counter
	blobsDeleted   prometheus.Counter
	bytesReclaimed prometheus.Counter
	pendingBacklog prometheus.Gauge
	sstateCoverage *prometheus.GaugeVec // {org,project}
	usageMeasured  *prometheus.GaugeVec // {org,project,backend}
	quotaRatio     *prometheus.GaugeVec // {org,project,backend}
	physicalBytes  prometheus.Gauge
	touchFlushRows prometheus.Counter
}

// gcRunBuckets span a sweep, which is minutes rather than milliseconds: at the
// shipped pacing (1000 rows per chunk, 100ms between chunks) a ten-million-row
// backend is ~17 minutes of pause alone.
var gcRunBuckets = []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200}

func newGCCollectors(f promauto.Factory) gcCollectors {
	return gcCollectors{
		runs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_gc_runs_total",
			Help: "GC sweeps by terminal status and what started them. " +
				"A `failed` run leaves its own re-drive work for the next one; a run " +
				"that never appears here at all is a loop that is not running.",
		}, []string{"status", "trigger"}),

		runDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "bakery_gc_run_duration_seconds",
			Help:    "Wall time of one GC sweep, start to terminal status.",
			Buckets: gcRunBuckets,
		}),

		lastRun: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_gc_last_run_timestamp_seconds",
			Help: "When the last GC sweep finished. The staleness alert an operator " +
				"wants: a stuck GC is invisible in every other series here, because a " +
				"loop that stopped emits nothing rather than something wrong.",
		}),

		objectsDeleted: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_gc_objects_deleted_total",
			Help: "cache_objects rows deleted, by backend kind, namespace and reason.",
		}, []string{"backend", "namespace", "reason"}),

		blobsMarked: f.NewCounter(prometheus.CounterOpts{
			Name: "bakery_gc_blobs_marked_total",
			Help: "Blobs moved to pending_delete (Layer B's mark). The grace period " +
				"between this and the reap is the recovery window.",
		}),

		blobsDeleted: f.NewCounter(prometheus.CounterOpts{
			Name: "bakery_gc_blobs_deleted_total",
			Help: "Blobs whose bytes were unlinked and whose row was reaped.",
		}),

		bytesReclaimed: f.NewCounter(prometheus.CounterOpts{
			Name: "bakery_gc_bytes_reclaimed_total",
			Help: "Bytes unlinked from storage by the reap.",
		}),

		pendingBacklog: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_gc_pending_delete_backlog",
			Help: "Blobs sitting in pending_delete at the end of the last sweep. " +
				"A backlog that only grows means the reap is failing, which no error " +
				"counter shows: a durable pending_delete row is re-driven forever.",
		}),

		sstateCoverage: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_gc_sstate_unihash_coverage",
			Help: "Fraction of scanned sstate keys whose unihash resolved to a surviving " +
				"row on the paired hashserv backend. Zero means the retention policy has " +
				"silently collapsed to age-only -- correct for a deployment with no " +
				"hashserv data, and a broken derivation otherwise.",
		}, []string{"org", "project"}),

		usageMeasured: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_gc_usage_measured_timestamp_seconds",
			Help: "When this backend's usage was last measured. Quota ratios and the " +
				"storage gauges are only as true as this timestamp is recent.",
		}, []string{"org", "project", "backend"}),

		quotaRatio: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_gc_quota_ratio",
			Help: "Logical bytes stored divided by the backend's quota. The console's " +
				"warning badge is 0.9. Absent for a backend with no quota.",
		}, []string{"org", "project", "backend"}),

		physicalBytes: f.NewGauge(prometheus.GaugeOpts{
			Name: "bakery_storage_physical_bytes",
			Help: "Live bytes on this instance, backend-blind and counted ONCE per " +
				"digest. This -- not the per-backend logical gauges, which charge a " +
				"deduped blob to every backend that names it -- is the number to alert " +
				"on for disk.",
		}),

		// UNWIRED (see TouchFlushRows' doc): registered now so the series exists at
		// boot and the smoke test covers its shape, but nothing increments it yet --
		// blob.Service.StartAccessToucher discards flushAccess's row count and the
		// hashserv toucher has no hook either. A permanently-zero series here would be
		// a lie if it claimed to measure anything; it does not yet.
		touchFlushRows: f.NewCounter(prometheus.CounterOpts{
			Name: "bakery_gc_touch_flush_rows_total",
			Help: "accessed_at rows written by the F/T-ramped toucher flushers " +
				"(cache_objects and hashserv_unihashes combined). Spec §6.1/§9.9.",
		}),
	}
}

// GCRecorder is the only way to touch the bakery_gc_* series.
//
// Unlike Recorder it resolves label children per call: every one of these is written
// from the sweep, which runs on an interval measured in hours and pauses 100ms
// between chunks. There is no hot path here to protect.
type GCRecorder struct {
	m *Metrics
}

// GC returns the GC recorder.
func (m *Metrics) GC() *GCRecorder { return &GCRecorder{m: m} }

// RunFinished records one terminal sweep.
func (r *GCRecorder) RunFinished(status, trigger string, d time.Duration, finishedAt time.Time) {
	r.m.gc.runs.WithLabelValues(status, trigger).Inc()
	r.m.gc.runDuration.Observe(d.Seconds())
	r.m.gc.lastRun.Set(float64(finishedAt.Unix()))
}

// ObjectsDeleted records deletions from one (backend kind, namespace) for one reason.
func (r *GCRecorder) ObjectsDeleted(backend Backend, namespace string, reason GCReason, n int64) {
	if n <= 0 {
		return
	}

	// An empty namespace is sstate's and downloads' real value in cache_objects; it
	// is rendered as a word so the label is readable in a query rather than blank.
	if namespace == "" {
		namespace = "default"
	}

	r.m.gc.objectsDeleted.WithLabelValues(string(backend), namespace, string(reason)).Add(float64(n))
}

// BlobsMarked records Layer B's mark.
func (r *GCRecorder) BlobsMarked(n int64) { r.m.gc.blobsMarked.Add(float64(n)) }

// BlobsReaped records Layer B's physical delete and the bytes it freed.
func (r *GCRecorder) BlobsReaped(n, bytes int64) {
	r.m.gc.blobsDeleted.Add(float64(n))
	r.m.gc.bytesReclaimed.Add(float64(bytes))
}

// PendingBacklog publishes the pending_delete depth observed at the end of a sweep.
func (r *GCRecorder) PendingBacklog(n int64) { r.m.gc.pendingBacklog.Set(float64(n)) }

// SstateCoverage publishes spec §5's observability gate for one sstate backend.
func (r *GCRecorder) SstateCoverage(org, project string, fraction float64) {
	r.m.gc.sstateCoverage.WithLabelValues(org, project).Set(fraction)
}

// Usage publishes one backend's measured usage: the two storage gauges, the
// measurement timestamp, and the quota ratio when the backend has a quota.
//
// quota <= 0 means "no cap": the ratio child is DELETED rather than set to zero, so
// a backend whose quota was removed stops exporting a ratio instead of exporting a
// permanent 0 that reads as "plenty of room".
func (r *GCRecorder) Usage(org, project string, backend Backend, objects, bytes, quota int64, at time.Time) {
	labels := []string{org, project, string(backend)}

	r.m.StorageObjects.WithLabelValues(labels...).Set(float64(objects))
	r.m.StorageBytes.WithLabelValues(labels...).Set(float64(bytes))
	r.m.gc.usageMeasured.WithLabelValues(labels...).Set(float64(at.Unix()))

	if quota <= 0 {
		r.m.gc.quotaRatio.DeleteLabelValues(labels...)

		return
	}

	r.m.gc.quotaRatio.WithLabelValues(labels...).Set(float64(bytes) / float64(quota))
}

// ResetUsage drops every per-backend usage series before a measurement pass
// re-publishes them (spec §8).
//
// A gauge vec is a MAP, not a snapshot: a backend that was deleted between two
// passes keeps exporting its last value forever, so a deleted project silently
// keeps counting against a storage dashboard. Resetting first is what makes a
// vanished backend actually vanish from the scrape.
func (r *GCRecorder) ResetUsage() {
	r.m.StorageObjects.Reset()
	r.m.StorageBytes.Reset()
	r.m.gc.usageMeasured.Reset()
	r.m.gc.quotaRatio.Reset()
}

// PhysicalBytes publishes the instance-wide live byte count.
func (r *GCRecorder) PhysicalBytes(n int64) { r.m.gc.physicalBytes.Set(float64(n)) }

// TouchFlushRows records one flusher tick's row count. NOT YET CALLED anywhere:
// see gcCollectors.touchFlushRows' doc for what is owed before this fires for
// real. It exists now so the wiring is a one-line call at the flush site rather
// than a new series to design under time pressure later.
func (r *GCRecorder) TouchFlushRows(n int64) {
	if n <= 0 {
		return
	}

	r.m.gc.touchFlushRows.Add(float64(n))
}
