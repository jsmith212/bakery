package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// BazelBytesOp is the direction of a ByteStream transfer. A CLOSED set: the `op`
// label on bakery_bazel_bytestream_bytes_total can be nothing else.
type BazelBytesOp string

const (
	BazelBSRead  BazelBytesOp = "read"
	BazelBSWrite BazelBytesOp = "write"
)

// bazelCollectors are the bakery_bazel_* / bakery_cache_ac_* families the REAPI
// backend owns.
//
// The bazel backend routes CAS and AC bytes through blob.Service, so it gets the
// headline bakery_cache_requests_total{backend="bazel"} series for free and must
// NOT re-emit it. These are the series blob.Service cannot know about: the
// FindMissingBlobs hit ratio (the number that says whether the REAPI HEAD storm is
// earning its keep), ByteStream throughput, and AC corruption -- an ac-grpc value
// that fails proto.Unmarshal, which in a namespace only we write can only be
// storage corruption, never traffic.
//
// Every label is a RESOLVED SLUG from a cache.Route -- never a digest, a resource
// name, or a raw instance_name.
type bazelCollectors struct {
	findMissingDigests *prometheus.CounterVec // {org,project}
	findMissingMissing *prometheus.CounterVec // {org,project}
	bytestreamBytes    *prometheus.CounterVec // {org,project,op}
	acUnparseable      *prometheus.CounterVec // {org,project}
}

func newBazelCollectors(f promauto.Factory) bazelCollectors {
	return bazelCollectors{
		findMissingDigests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_bazel_find_missing_digests_total",
			Help: "Digests asked about across FindMissingBlobs. The denominator of the " +
				"REAPI cache hit ratio.",
		}, []string{"org", "project"}),

		findMissingMissing: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_bazel_find_missing_missing_total",
			Help: "Digests FindMissingBlobs reported missing. Rising toward the digests " +
				"total means the cache is not serving this project.",
		}, []string{"org", "project"}),

		bytestreamBytes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_bazel_bytestream_bytes_total",
			Help: "Bytes transferred over ByteStream Read/Write.",
		}, []string{"org", "project", "op"}),

		acUnparseable: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_cache_ac_unparseable_total",
			Help: "ac-grpc values that failed proto.Unmarshal. Only UpdateActionResult " +
				"writes ac-grpc, so this is storage corruption, not traffic.",
		}, []string{"org", "project"}),
	}
}

// BazelRecorder is a PRE-RESOLVED view of every bakery_bazel_* series for one
// (org, project), and the only way to touch them.
//
// Resolve it once per RPC from a Route, whose slugs came from a DB row -- never with
// a raw instance_name path segment, which is attacker-controlled until the route
// resolves it.
type BazelRecorder struct {
	fmDigests     prometheus.Counter
	fmMissing     prometheus.Counter
	bsRead        prometheus.Counter
	bsWrite       prometheus.Counter
	acUnparseable prometheus.Counter
}

// Bazel resolves the bakery_bazel_* collectors for one project.
func (m *Metrics) Bazel(org, project string) *BazelRecorder {
	t := prometheus.Labels{"org": org, "project": project}
	bs := m.bazel.bytestreamBytes.MustCurryWith(t)

	return &BazelRecorder{
		fmDigests:     m.bazel.findMissingDigests.With(t),
		fmMissing:     m.bazel.findMissingMissing.With(t),
		bsRead:        bs.WithLabelValues(string(BazelBSRead)),
		bsWrite:       bs.WithLabelValues(string(BazelBSWrite)),
		acUnparseable: m.bazel.acUnparseable.With(t),
	}
}

// FindMissing records one FindMissingBlobs: how many digests it asked about (empty
// blobs excluded -- they are never missing) and how many were absent.
func (r *BazelRecorder) FindMissing(digests, missing int) {
	r.fmDigests.Add(float64(digests))
	r.fmMissing.Add(float64(missing))
}

// ByteStreamBytes records bytes moved by a ByteStream Read or Write.
func (r *BazelRecorder) ByteStreamBytes(op BazelBytesOp, n int64) {
	switch op {
	case BazelBSRead:
		r.bsRead.Add(float64(n))
	case BazelBSWrite:
		r.bsWrite.Add(float64(n))
	}
}

// ACUnparseable records one ac-grpc value that failed proto.Unmarshal on a
// GetActionResult read. It is a corruption signal, not a miss counter.
func (r *BazelRecorder) ACUnparseable() { r.acUnparseable.Inc() }
