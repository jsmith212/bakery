package metrics

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestHashservMethodLabelIsNeverTheWireMethod is the cardinality invariant, and it
// drives the exact mistake it exists to prevent.
//
// hashserv's wire protocol carries its own "method" field: SSTATE_HASHEQUIV_METHOD,
// an opaque client-controlled string on every get and report. A handler that passes it
// where the RPC NAME belongs compiles and reads plausibly. Here it is passed 2000
// distinct times -- and it must mint ZERO series, folding onto method="other".
func TestHashservMethodLabelIsNeverTheWireMethod(t *testing.T) {
	m := New()
	rec := m.Hashserv("acme", "widget")

	const requests = 2000

	for i := range requests {
		wire := fmt.Sprintf("oe.sstatesig.OEOuthashBasic.%064x", i)

		rec.RPC(HashservRPC(wire), RPCOK)
	}

	series := seriesFor(t, m, "bakery_hashserv_rpcs_total")

	want := len(allHashservRPCs) * len(allHashservResults)
	if len(series) != want {
		t.Fatalf("%d distinct wire methods minted %d series, want %d -- "+
			"an unbounded value reached the `method` label", requests, len(series), want)
	}

	got := counterValue(t, m, "bakery_hashserv_rpcs_total", map[string]string{
		"org": "acme", "project": "widget", "method": string(RPCOther), "result": string(RPCOK),
	})
	if got != requests {
		t.Errorf("method=other counter = %v, want %v -- the fold did not happen", got, requests)
	}
}

// TestHashservRecorderIsBounded. Every label dimension is a closed set resolved
// eagerly, so ONE project is a fixed number of series -- a number that is decided at
// compile time and does not move when traffic does.
func TestHashservRecorderIsBounded(t *testing.T) {
	m := New()
	_ = m.Hashserv("acme", "widget")

	tests := []struct {
		name   string
		family string
		want   int
	}{
		{name: "rpcs", family: "bakery_hashserv_rpcs_total", want: 33},
		{name: "stream lines", family: "bakery_hashserv_stream_lines_total", want: 2},
		{name: "reports dropped", family: "bakery_hashserv_reports_dropped_total", want: 3},
		{name: "upstream", family: "bakery_hashserv_upstream_total", want: 12},
		{name: "equivalences", family: "bakery_hashserv_equivalences_total", want: 1},
		{name: "connections", family: "bakery_hashserv_connections", want: 1},
		{name: "backfill queue", family: "bakery_hashserv_backfill_queue", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(seriesFor(t, m, tt.family)); got != tt.want {
				t.Errorf("%s: one recorder minted %d series, want %d", tt.family, got, tt.want)
			}
		})
	}
}

// TestHashservIndexesMatchTheirTables pins the hand-written switches to the arrays
// they index. Get one number wrong and every ok lands in the error column -- silently,
// forever, and no other test would catch it.
func TestHashservIndexesMatchTheirTables(t *testing.T) {
	for _, v := range allHashservRPCs {
		if got := allHashservRPCs[hashservRPCIndex(v)]; got != v {
			t.Errorf("hashservRPCIndex(%q) indexes %q", v, got)
		}
	}

	for _, v := range allHashservResults {
		if got := allHashservResults[hashservResultIndex(v)]; got != v {
			t.Errorf("hashservResultIndex(%q) indexes %q", v, got)
		}
	}

	for _, v := range allHashservStreams {
		if got := allHashservStreams[hashservStreamIndex(v)]; got != v {
			t.Errorf("hashservStreamIndex(%q) indexes %q", v, got)
		}
	}

	for _, v := range allHashservDrops {
		if got := allHashservDrops[hashservDropIndex(v)]; got != v {
			t.Errorf("hashservDropIndex(%q) indexes %q", v, got)
		}
	}

	for _, v := range allUpstreamOps {
		if got := allUpstreamOps[upstreamOpIndex(v)]; got != v {
			t.Errorf("upstreamOpIndex(%q) indexes %q", v, got)
		}
	}

	for _, v := range allUpstreamResults {
		if got := allUpstreamResults[upstreamResultIndex(v)]; got != v {
			t.Errorf("upstreamResultIndex(%q) indexes %q", v, got)
		}
	}
}

// TestHashservFoldsUnknownValues. An RPC name arrives off the wire, so an out-of-set
// value must land somewhere bounded rather than panic or mint a series -- a metrics
// call may never take down a connection.
func TestHashservFoldsUnknownValues(t *testing.T) {
	m := New()
	rec := m.Hashserv("acme", "widget")

	rec.RPC(HashservRPC("gc-mark"), HashservResult("weird"))
	rec.StreamLine(HashservStream("nonsense"))
	rec.ReportDropped(HashservDropReason("nonsense"))
	rec.Upstream(HashservUpstreamOp("nonsense"), HashservUpstreamResult("nonsense"))

	tests := []struct {
		name   string
		family string
		labels map[string]string
	}{
		{
			name:   "an unimplemented RPC folds onto other, and a bad result onto error",
			family: "bakery_hashserv_rpcs_total",
			labels: map[string]string{"method": "other", "result": "error"},
		},
		{
			name:   "an unknown stream folds onto get-stream",
			family: "bakery_hashserv_stream_lines_total",
			labels: map[string]string{"stream": "get-stream"},
		},
		{
			name:   "an unknown drop reason folds onto error",
			family: "bakery_hashserv_reports_dropped_total",
			labels: map[string]string{"reason": "error"},
		},
		{
			name:   "an unknown upstream op and result fold onto get and error",
			family: "bakery_hashserv_upstream_total",
			labels: map[string]string{"op": "get", "result": "error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := map[string]string{"org": "acme", "project": "widget"}
			for k, v := range tt.labels {
				labels[k] = v
			}

			if got := counterValue(t, m, tt.family, labels); got != 1 {
				t.Errorf("%s%v = %v, want 1", tt.family, labels, got)
			}
		})
	}
}

// TestHashservRecorderRecords: the values land in the series they name.
func TestHashservRecorderRecords(t *testing.T) {
	m := New()
	rec := m.Hashserv("acme", "widget")

	for range 3 {
		rec.StreamLine(StreamGet)
	}

	rec.RPC(RPCGetStream, RPCOK)
	rec.RPC(RPCReport, RPCDenied)
	rec.Equivalence()
	rec.Equivalence()
	rec.ReportDropped(DropReadOnly)
	rec.Upstream(UpstreamGetOuthash, UpstreamMiss)
	rec.SetBackfillQueue(7)

	tenant := func(extra map[string]string) map[string]string {
		labels := map[string]string{"org": "acme", "project": "widget"}
		for k, v := range extra {
			labels[k] = v
		}

		return labels
	}

	counters := []struct {
		name   string
		family string
		labels map[string]string
		want   float64
	}{
		{
			name:   "stream lines",
			family: "bakery_hashserv_stream_lines_total",
			labels: tenant(map[string]string{"stream": "get-stream"}),
			want:   3,
		},
		{
			name:   "one get-stream RPC carried all three lines",
			family: "bakery_hashserv_rpcs_total",
			labels: tenant(map[string]string{"method": "get-stream", "result": "ok"}),
			want:   1,
		},
		{
			name:   "a denied report",
			family: "bakery_hashserv_rpcs_total",
			labels: tenant(map[string]string{"method": "report", "result": "denied"}),
			want:   1,
		},
		{
			name:   "equivalences",
			family: "bakery_hashserv_equivalences_total",
			labels: tenant(nil),
			want:   2,
		},
		{
			name:   "reports dropped read-only",
			family: "bakery_hashserv_reports_dropped_total",
			labels: tenant(map[string]string{"reason": "read_only"}),
			want:   1,
		},
		{
			name:   "upstream miss",
			family: "bakery_hashserv_upstream_total",
			labels: tenant(map[string]string{"op": "get-outhash", "result": "miss"}),
			want:   1,
		},
	}

	for _, tt := range counters {
		t.Run(tt.name, func(t *testing.T) {
			if got := counterValue(t, m, tt.family, tt.labels); got != tt.want {
				t.Errorf("%s%v = %v, want %v", tt.family, tt.labels, got, tt.want)
			}
		})
	}

	if got := gaugeValue(t, m, "bakery_hashserv_backfill_queue", tenant(nil)); got != 7 {
		t.Errorf("backfill queue gauge = %v, want 7", got)
	}
}

// TestHashservConnectionsGaugeIsShared. Every connection resolves its own recorder, so
// the gauge behind them must be one gauge per project and not one per recorder -- else
// bakery_hashserv_connections reads 1 forever while a thousand connections are live.
func TestHashservConnectionsGaugeIsShared(t *testing.T) {
	m := New()

	first := m.Hashserv("acme", "widget")
	second := m.Hashserv("acme", "widget")
	other := m.Hashserv("acme", "gadget")

	first.ConnOpened()
	second.ConnOpened()
	other.ConnOpened()
	second.ConnClosed()

	widget := map[string]string{"org": "acme", "project": "widget"}
	if got := gaugeValue(t, m, "bakery_hashserv_connections", widget); got != 1 {
		t.Errorf("acme/widget connections = %v, want 1 -- two recorders for one project "+
			"must share one gauge", got)
	}

	gadget := map[string]string{"org": "acme", "project": "gadget"}
	if got := gaugeValue(t, m, "bakery_hashserv_connections", gadget); got != 1 {
		t.Errorf("acme/gadget connections = %v, want 1", got)
	}
}

// TestHashservLeaksNoUnihash is the audit. It drives every recorder method with the
// values a real connection actually holds -- unihashes, taskhashes, outhashes, the
// client's method string -- and then scans EVERY label value in the bakery_hashserv_*
// families. Nothing that came off the wire may survive into a label.
func TestHashservLeaksNoUnihash(t *testing.T) {
	m := New()
	rec := m.Hashserv("acme", "widget")

	for i := range 500 {
		hash := fmt.Sprintf("%064x", i)

		rec.RPC(HashservRPC(hash), RPCOK)
		rec.StreamLine(HashservStream(hash))
		rec.ReportDropped(HashservDropReason(hash))
		rec.Upstream(HashservUpstreamOp(hash), HashservUpstreamResult(hash))
		rec.Equivalence()
	}

	longHex := regexp.MustCompile(`[0-9a-f]{16,}`)

	for _, fam := range gather(t, m) {
		if !strings.HasPrefix(fam.GetName(), "bakery_hashserv_") {
			continue
		}

		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if longHex.MatchString(lp.GetValue()) {
					t.Errorf("%s{%s=%q} leaks a hash into a label",
						fam.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

// gaugeValue reads one gauge out of the exposition, or 0 if the series does not exist.
func gaugeValue(t *testing.T, m *Metrics, name string, want map[string]string) float64 {
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
				return met.GetGauge().GetValue()
			}
		}
	}

	return 0
}
