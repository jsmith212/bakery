package metrics

import (
	"testing"
	"time"
)

// TestGCUnlabeledSeriesRegisteredAtBoot: run_duration, last_run, blobs_marked,
// blobs_deleted, bytes_reclaimed, pending_delete_backlog, physical_bytes and
// touch_flush_rows are plain Counters/Gauges/a Histogram, not Vecs, so they are
// collectible the instant New() returns -- before a single sweep has run. A GC loop
// that never fires must still read as a metric reporting zero, not as a series an
// operator cannot tell from "never scraped".
//
// The LABEL-carrying series (runs_total, objects_deleted_total,
// sstate_unihash_coverage, usage_measured_timestamp_seconds, quota_ratio, and the
// shared bakery_storage_objects/_bytes gauges) are deliberately NOT asserted here:
// GCRecorder resolves org/project per call rather than pre-registering (unlike
// hashserv's eager RecorderCache), because org/project is an unbounded label space
// GC must not mint before it has ever swept anything real. TestGCRecorderWritesEverySeries
// below exercises those the only way they can be exercised -- by calling the
// recorder and reading back what it wrote.
func TestGCUnlabeledSeriesRegisteredAtBoot(t *testing.T) {
	m := New()

	for _, name := range []string{
		"bakery_gc_run_duration_seconds",
		"bakery_gc_last_run_timestamp_seconds",
		"bakery_gc_blobs_marked_total",
		"bakery_gc_blobs_deleted_total",
		"bakery_gc_bytes_reclaimed_total",
		"bakery_gc_pending_delete_backlog",
		"bakery_storage_physical_bytes",
		"bakery_gc_touch_flush_rows_total",
		"bakery_gc_touch_aux_dropped_total",
	} {
		if got := seriesFor(t, m, name); len(got) == 0 {
			t.Errorf("%s has no series at boot -- it must be collectible before any sweep runs", name)
		}
	}
}

// TestGCRecorderWritesEverySeries calls every GCRecorder method once and checks the
// value landed in the series its own name promises -- the smoke test for the whole
// §9.9 list, and the only way to prove the label-per-call series (see the doc above)
// are wired at all: they mint nothing until called.
func TestGCRecorderWritesEverySeries(t *testing.T) {
	m := New()
	rec := m.GC()

	finishedAt := time.Unix(1_700_000_000, 0)

	rec.RunFinished("succeeded", "interval", 42*time.Second, finishedAt)
	rec.ObjectsDeleted(BackendSstate, "", GCReasonRetention, 3)
	rec.BlobsMarked(2)
	rec.BlobsReaped(2, 4096)
	rec.PendingBacklog(1)
	rec.SstateCoverage("acme", "widget", 0.75)
	rec.Usage("acme", "widget", BackendSstate, 10, 2048, 4096, finishedAt)
	rec.PhysicalBytes(8192)
	rec.TouchFlushRows(6)
	rec.TouchAuxDropped(4)

	tenant := map[string]string{"org": "acme", "project": "widget", "backend": "sstate"}

	counters := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"bakery_gc_runs_total", map[string]string{"status": "succeeded", "trigger": "interval"}, 1},
		{
			"bakery_gc_objects_deleted_total",
			map[string]string{"backend": "sstate", "namespace": "default", "reason": "retention"},
			3,
		},
		{"bakery_gc_blobs_marked_total", nil, 2},
		{"bakery_gc_blobs_deleted_total", nil, 2},
		{"bakery_gc_bytes_reclaimed_total", nil, 4096},
		{"bakery_gc_touch_flush_rows_total", nil, 6},
		{"bakery_gc_touch_aux_dropped_total", nil, 4},
	}

	for _, c := range counters {
		t.Run(c.name, func(t *testing.T) {
			if got := counterValue(t, m, c.name, c.labels); got != c.want {
				t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
			}
		})
	}

	gauges := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"bakery_gc_pending_delete_backlog", nil, 1},
		{"bakery_gc_last_run_timestamp_seconds", nil, float64(finishedAt.Unix())},
		{"bakery_storage_physical_bytes", nil, 8192},
		{
			"bakery_gc_sstate_unihash_coverage",
			map[string]string{"org": "acme", "project": "widget"}, 0.75,
		},
		{"bakery_gc_usage_measured_timestamp_seconds", tenant, float64(finishedAt.Unix())},
		{"bakery_gc_quota_ratio", tenant, 2048.0 / 4096.0},
		{"bakery_storage_objects", tenant, 10},
		{"bakery_storage_bytes", tenant, 2048},
	}

	for _, g := range gauges {
		t.Run(g.name, func(t *testing.T) {
			if got := gaugeValue(t, m, g.name, g.labels); got != g.want {
				t.Errorf("%s%v = %v, want %v", g.name, g.labels, got, g.want)
			}
		})
	}
}

// TestGCUsageQuotaRatioAbsentWithNoQuota: quota <= 0 means "no cap", and the ratio
// child must be DELETED rather than set to zero -- a lingering zero reads as
// "plenty of room" instead of "this backend has no cap at all" (gc.go's own doc on
// GCRecorder.Usage).
func TestGCUsageQuotaRatioAbsentWithNoQuota(t *testing.T) {
	m := New()
	rec := m.GC()

	rec.Usage("acme", "widget", BackendBazel, 5, 1024, 0, time.Now())

	if got := seriesFor(t, m, "bakery_gc_quota_ratio"); len(got) != 0 {
		t.Errorf("bakery_gc_quota_ratio has %d series for a quota-less backend, want 0", len(got))
	}
}

// TestGCResetUsageDropsThePreviousPass: a backend that vanishes between two usage
// passes must vanish from the scrape too (spec §9.9) -- a gauge vec is a map, not a
// snapshot, and re-Setting only the survivors would leave a deleted project's last
// value exported forever.
func TestGCResetUsageDropsThePreviousPass(t *testing.T) {
	m := New()
	rec := m.GC()

	rec.Usage("acme", "widget", BackendSstate, 10, 2048, 4096, time.Now())

	if got := seriesFor(t, m, "bakery_storage_objects"); len(got) != 1 {
		t.Fatalf("bakery_storage_objects has %d series before reset, want 1", len(got))
	}

	rec.ResetUsage()

	for _, name := range []string{
		"bakery_storage_objects", "bakery_storage_bytes",
		"bakery_gc_usage_measured_timestamp_seconds", "bakery_gc_quota_ratio",
	} {
		if got := seriesFor(t, m, name); len(got) != 0 {
			t.Errorf("%s has %d series after ResetUsage, want 0", name, len(got))
		}
	}
}
