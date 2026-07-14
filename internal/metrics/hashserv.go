package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HashservRPC is the name of a hashserv RPC. A CLOSED set, and the only thing that
// may ever reach the `method` label.
//
// THE TRAP THIS TYPE EXISTS TO CLOSE. The hashserv wire protocol has its own field
// called "method": it is the value of SSTATE_HASHEQUIV_METHOD, an opaque
// client-controlled string carried on every get, get-stream and report. Putting THAT
// on a label mints a time series per string a client invents -- the same class of bug
// as labeling HTTP on r.URL.Path, and it kills Prometheus inside a single build. The
// `method` label on bakery_hashserv_rpcs_total is the RPC NAME and nothing else, and
// a raw string cannot be passed here.
//
// A value outside the set folds onto RPCOther (hashservRPCIndex). That matters
// because the RPC name itself arrives off the wire: an unrecognized command is logged
// and the connection dropped, and the one thing it must not be able to do on the way
// out is name a series.
type HashservRPC string

const (
	RPCPing         HashservRPC = "ping"
	RPCAuth         HashservRPC = "auth"
	RPCGet          HashservRPC = "get"
	RPCGetStream    HashservRPC = "get-stream"
	RPCExistsStream HashservRPC = "exists-stream"
	RPCReport       HashservRPC = "report"
	RPCReportEquiv  HashservRPC = "report-equiv"
	RPCGetOuthash   HashservRPC = "get-outhash"
	RPCRemove       HashservRPC = "remove"
	RPCBackfillWait HashservRPC = "backfill-wait"

	// RPCOther collapses every command this server does not implement: the GC and
	// user-admin RPCs it refuses on purpose, and outright garbage.
	RPCOther HashservRPC = "other"
)

// HashservResult is the outcome of an RPC. A CLOSED set.
//
//	ok      the RPC ran and answered
//	error   the RPC failed (storage, DB, malformed request)
//	denied  the caller's permissions did not admit it -- an in-band invoke-error,
//	        never a 401 or 403 on the upgrade
type HashservResult string

const (
	RPCOK     HashservResult = "ok"
	RPCError  HashservResult = "error"
	RPCDenied HashservResult = "denied"
)

// HashservStream names the streaming RPC whose lines are being counted. A CLOSED set.
//
// bakery_hashserv_stream_lines_total is the hot path's REAL volume: one connection
// and one bakery_hashserv_rpcs_total increment can carry the whole setscene graph.
type HashservStream string

const (
	StreamGet    HashservStream = "get-stream"
	StreamExists HashservStream = "exists-stream"
)

// HashservDropReason is why a report did not write. A CLOSED set.
//
// Every value here is a SILENT non-write: the client is answered normally and
// believes it reported. That is upstream's behavior and it is correct for an open
// mirror, but unmetered it is indistinguishable from a healthy build -- so it is
// counted, and it is counted separately from an error.
type HashservDropReason string

const (
	// DropReadOnly: the caller holds @read but not @report (anonymous against a
	// backend with read_auth_required = false, or a read-scoped key). Upstream's
	// report_readonly path: look up, return stored-or-echoed, never write.
	DropReadOnly HashservDropReason = "read_only"

	// DropInvalidUnihash: the reported unihash failed ^[0-9a-f]{64}$.
	DropInvalidUnihash HashservDropReason = "invalid_unihash"

	// DropError: the write was attempted and failed.
	DropError HashservDropReason = "error"
)

// HashservUpstreamOp is an operation issued against the chained upstream hashserv.
// A CLOSED set.
type HashservUpstreamOp string

const (
	UpstreamGet        HashservUpstreamOp = "get"
	UpstreamGetOuthash HashservUpstreamOp = "get-outhash"
	UpstreamExists     HashservUpstreamOp = "exists"
	UpstreamBackfill   HashservUpstreamOp = "backfill"
)

// HashservUpstreamResult is the outcome of an upstream query. A CLOSED set.
// `miss` is not a failure -- it is the answer that upstream does not know the hash.
type HashservUpstreamResult string

const (
	UpstreamHit   HashservUpstreamResult = "hit"
	UpstreamMiss  HashservUpstreamResult = "miss"
	UpstreamError HashservUpstreamResult = "error"
)

// The closed sets, in label order. A HashservRecorder resolves the full cross product
// eagerly, so these arrays ARE the cardinality of bakery_hashserv_* per project: it is
// fixed at compile time and does not grow with traffic.
var (
	allHashservRPCs = [...]HashservRPC{
		RPCPing, RPCAuth, RPCGet, RPCGetStream, RPCExistsStream, RPCReport,
		RPCReportEquiv, RPCGetOuthash, RPCRemove, RPCBackfillWait, RPCOther,
	}
	allHashservResults = [...]HashservResult{RPCOK, RPCError, RPCDenied}
	allHashservStreams = [...]HashservStream{StreamGet, StreamExists}
	allHashservDrops   = [...]HashservDropReason{
		DropReadOnly, DropInvalidUnihash, DropError,
	}
	allUpstreamOps = [...]HashservUpstreamOp{
		UpstreamGet, UpstreamGetOuthash, UpstreamExists, UpstreamBackfill,
	}
	allUpstreamResults = [...]HashservUpstreamResult{
		UpstreamHit, UpstreamMiss, UpstreamError,
	}
)

// The index functions fold an unrecognized value onto a terminal slot rather than
// panicking or minting a series. TestHashservIndexesMatchTheirTables pins each one to
// the table above, which is the drift these hand-written switches would otherwise
// invite.

func hashservRPCIndex(r HashservRPC) int {
	switch r {
	case RPCPing:
		return 0
	case RPCAuth:
		return 1
	case RPCGet:
		return 2
	case RPCGetStream:
		return 3
	case RPCExistsStream:
		return 4
	case RPCReport:
		return 5
	case RPCReportEquiv:
		return 6
	case RPCGetOuthash:
		return 7
	case RPCRemove:
		return 8
	case RPCBackfillWait:
		return 9
	case RPCOther:
		return 10
	}

	return 10
}

func hashservResultIndex(r HashservResult) int {
	switch r {
	case RPCOK:
		return 0
	case RPCError:
		return 1
	case RPCDenied:
		return 2
	}

	return 1
}

func hashservStreamIndex(s HashservStream) int {
	switch s {
	case StreamGet:
		return 0
	case StreamExists:
		return 1
	}

	return 0
}

func hashservDropIndex(d HashservDropReason) int {
	switch d {
	case DropReadOnly:
		return 0
	case DropInvalidUnihash:
		return 1
	case DropError:
		return 2
	}

	return 2
}

func upstreamOpIndex(o HashservUpstreamOp) int {
	switch o {
	case UpstreamGet:
		return 0
	case UpstreamGetOuthash:
		return 1
	case UpstreamExists:
		return 2
	case UpstreamBackfill:
		return 3
	}

	return 0
}

func upstreamResultIndex(r HashservUpstreamResult) int {
	switch r {
	case UpstreamHit:
		return 0
	case UpstreamMiss:
		return 1
	case UpstreamError:
		return 2
	}

	return 2
}

// hashservCollectors are the bakery_hashserv_* families.
//
// hashserv is the ONE backend that does not route through blob.Service (see the Deps
// doc comment in internal/cache/backend.go), so it gets none of the headline
// bakery_cache_* series for free and owns these instead.
//
// UNEXPORTED, unlike every other collector on Metrics, and that is the enforcement
// mechanism rather than a style choice. An exported *prometheus.CounterVec here would
// permit
//
//	m.HashservRPCs.WithLabelValues(org, project, req.Method, "ok")
//
// -- which compiles, reads plausibly, passes review, and labels on the
// client-controlled wire method. See HashservRPC. The only door to these collectors is
// HashservRecorder, whose arguments are typed closed sets.
type hashservCollectors struct {
	conns        *prometheus.GaugeVec
	rpcs         *prometheus.CounterVec
	streamLines  *prometheus.CounterVec
	equivalences *prometheus.CounterVec
	dropped      *prometheus.CounterVec
	upstream     *prometheus.CounterVec
	backfill     *prometheus.GaugeVec
}

func newHashservCollectors(f promauto.Factory) hashservCollectors {
	return hashservCollectors{
		conns: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_hashserv_connections",
			Help: "Live hashserv websocket connections.",
		}, []string{"org", "project"}),

		rpcs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_hashserv_rpcs_total",
			Help: "hashserv RPCs by name and outcome. `method` is the RPC NAME, " +
				"never the client-controlled SSTATE_HASHEQUIV_METHOD carried on the wire.",
		}, []string{"org", "project", "method", "result"}),

		streamLines: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_hashserv_stream_lines_total",
			Help: "Lines exchanged in streaming mode. The hot path's real volume: " +
				"one get-stream RPC carries the whole setscene graph.",
		}, []string{"org", "project", "stream"}),

		equivalences: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_hashserv_equivalences_total",
			Help: "Reports whose outhash matched an older taskhash, so the client's " +
				"unihash was remapped. The number that says whether hash equivalence " +
				"is earning its keep.",
		}, []string{"org", "project"}),

		dropped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_hashserv_reports_dropped_total",
			Help: "Reports that did not write, by reason. These are silent non-writes: " +
				"the client is answered normally and believes it reported.",
		}, []string{"org", "project", "reason"}),

		upstream: f.NewCounterVec(prometheus.CounterOpts{
			Name: "bakery_hashserv_upstream_total",
			Help: "Queries issued to the chained upstream hashserv. `miss` is an answer, " +
				"not a failure.",
		}, []string{"org", "project", "op", "result"}),

		backfill: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bakery_hashserv_backfill_queue",
			Help: "Unihashes learned from upstream on a get-stream hit and queued for " +
				"write-behind. backfill-wait drains this.",
		}, []string{"org", "project"}),
	}
}

// HashservRecorder is a PRE-RESOLVED view of every bakery_hashserv_* series for one
// (org, project), and the only way to touch them.
//
// It exists for the same two reasons Recorder does -- see recorder.go for the
// measurements -- and one more:
//
//  1. CARDINALITY. Its arguments are closed Go types, and an out-of-set value folds
//     onto a terminal slot instead of minting a series. Nothing that came off the wire
//     can reach a label, structurally, not by convention.
//  2. COST. WithLabelValues hashes the label values and locks the metric map on every
//     call (~95ns on a 4-label vec). StreamLine is called once per line of a
//     get-stream that carries the entire setscene graph; resolving the counter once
//     costs ~7ns per Inc.
//
// The gauges are VIEWS, not copies: two recorders for the same (org, project) share
// one underlying gauge, so ConnOpened/ConnClosed from different connections add up.
type HashservRecorder struct {
	conns        prometheus.Gauge
	backfill     prometheus.Gauge
	equivalences prometheus.Counter

	rpcs        [len(allHashservRPCs)][len(allHashservResults)]prometheus.Counter
	streamLines [len(allHashservStreams)]prometheus.Counter
	dropped     [len(allHashservDrops)]prometheus.Counter
	upstream    [len(allUpstreamOps)][len(allUpstreamResults)]prometheus.Counter
}

// Hashserv resolves the bakery_hashserv_* collectors for one project.
//
// Call it ONCE PER CONNECTION, at the upgrade, where the Route has already resolved
// the slugs from a DB row -- never per RPC, and never with a raw {org}/{project} path
// segment, which is attacker-controlled until the Route resolves it.
func (m *Metrics) Hashserv(org, project string) *HashservRecorder {
	tenant := prometheus.Labels{"org": org, "project": project}

	rpcs := m.hashserv.rpcs.MustCurryWith(tenant)
	lines := m.hashserv.streamLines.MustCurryWith(tenant)
	drops := m.hashserv.dropped.MustCurryWith(tenant)
	upstream := m.hashserv.upstream.MustCurryWith(tenant)

	r := &HashservRecorder{
		conns:        m.hashserv.conns.With(tenant),
		backfill:     m.hashserv.backfill.With(tenant),
		equivalences: m.hashserv.equivalences.With(tenant),

		rpcs:        [len(allHashservRPCs)][len(allHashservResults)]prometheus.Counter{},
		streamLines: [len(allHashservStreams)]prometheus.Counter{},
		dropped:     [len(allHashservDrops)]prometheus.Counter{},
		upstream:    [len(allUpstreamOps)][len(allUpstreamResults)]prometheus.Counter{},
	}

	for i, rpc := range allHashservRPCs {
		for j, res := range allHashservResults {
			r.rpcs[i][j] = rpcs.WithLabelValues(string(rpc), string(res))
		}
	}

	for i, s := range allHashservStreams {
		r.streamLines[i] = lines.WithLabelValues(string(s))
	}

	for i, d := range allHashservDrops {
		r.dropped[i] = drops.WithLabelValues(string(d))
	}

	for i, op := range allUpstreamOps {
		for j, res := range allUpstreamResults {
			r.upstream[i][j] = upstream.WithLabelValues(string(op), string(res))
		}
	}

	return r
}

// ConnOpened and ConnClosed bracket a connection's life. They must be paired --
// a leaked ConnOpened is a gauge that only ever climbs, and this gauge is how an
// operator sees a connection leak in the first place.
func (r *HashservRecorder) ConnOpened() { r.conns.Inc() }

// ConnClosed records a connection teardown. Defer it next to ConnOpened.
func (r *HashservRecorder) ConnClosed() { r.conns.Dec() }

// RPC records one completed RPC. `rpc` is the RPC NAME -- if the command was not
// recognized, pass RPCOther, and never the raw command string.
func (r *HashservRecorder) RPC(rpc HashservRPC, res HashservResult) {
	r.rpcs[hashservRPCIndex(rpc)][hashservResultIndex(res)].Inc()
}

// StreamLine records one line exchanged in streaming mode. This is the hot path: two
// array indexes and one atomic add.
func (r *HashservRecorder) StreamLine(s HashservStream) {
	r.streamLines[hashservStreamIndex(s)].Inc()
}

// Equivalence records that a report's outhash matched an OLDER taskhash's, so the
// client's unihash was remapped to the stored one. It is NOT a count of reports: a
// report that merely stores its own unihash did not establish an equivalence.
func (r *HashservRecorder) Equivalence() { r.equivalences.Inc() }

// ReportDropped records a report that was answered but not written.
func (r *HashservRecorder) ReportDropped(reason HashservDropReason) {
	r.dropped[hashservDropIndex(reason)].Inc()
}

// Upstream records one query against the chained upstream hashserv.
func (r *HashservRecorder) Upstream(op HashservUpstreamOp, res HashservUpstreamResult) {
	r.upstream[upstreamOpIndex(op)][upstreamResultIndex(res)].Inc()
}

// SetBackfillQueue publishes the current depth of the write-behind backfill queue.
//
// A Set, not an Inc/Dec pair: the queue's owner knows its own depth, and a gauge that
// two goroutines drive by delta drifts the moment one of them misses a decrement.
func (r *HashservRecorder) SetBackfillQueue(depth int) {
	r.backfill.Set(float64(depth))
}
