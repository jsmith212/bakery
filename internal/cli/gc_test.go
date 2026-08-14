package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/config"
)

// TestGCRunWithoutWaitPrintsTheID: the server answers as soon as it has a run id
// (spec §9.10), and by default `bakery gc run` does not block on the sweep --
// it prints the id it was handed and returns, exactly one request made.
func TestGCRunWithoutWaitPrintsTheID(t *testing.T) {

	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == api.Prefix+"/gc/run" {
			writeJSONResp(w, http.StatusAccepted, api.TriggerGCRunResponse{ID: 7, Status: "running"})

			return true
		}

		return false
	}

	c := f.client(t)

	var out bytes.Buffer

	if err := gcRun(t.Context(), c, renderer{out: &out, json: false}, config.GCRunCmd{}); err != nil {
		t.Fatalf("gcRun() error = %v", err)
	}

	if got := out.String(); !strings.Contains(got, "gc run 7") || !strings.Contains(got, "started") {
		t.Errorf("output = %q, want it to name run 7 as started", got)
	}

	if strings.Contains(out.String(), "dry run") {
		t.Errorf("output = %q, a REAL run must not claim to be a dry run", out.String())
	}

	if len(f.requests) != 1 {
		t.Fatalf("%d requests made, want exactly 1 -- gc run without --wait must not poll", len(f.requests))
	}
}

// TestGCRunDryRunSaysSo: --dry-run must be visible in BOTH the request (asserted
// by TestClientVerbs' "gc trigger" case) and the human-readable output -- an
// operator staring at a scrollback must not mistake a dry run for a real one.
func TestGCRunDryRunSaysSo(t *testing.T) {

	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSONResp(w, http.StatusAccepted, api.TriggerGCRunResponse{ID: 9, Status: "running"})

		return true
	}

	c := f.client(t)

	var out bytes.Buffer

	cmd := config.GCRunCmd{DryRun: true, Wait: false}
	if err := gcRun(t.Context(), c, renderer{out: &out, json: false}, cmd); err != nil {
		t.Fatalf("gcRun() error = %v", err)
	}

	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("output = %q, want it to say this was a dry run", out.String())
	}
}

// TestGCRunWaitsForATerminalStatus drives --wait through two polls: the run is
// 'running' on the first GET and 'succeeded' on the second, and the command must
// not return until the second. c.sleep is INJECTED so this test spends no real
// time asleep, matching login_test.go's device-grant pattern -- and the recorded
// sleeps prove the poll actually paused between requests rather than busy-looping.
func TestGCRunWaitsForATerminalStatus(t *testing.T) {

	f := newFakeAPI(t)

	var gets int

	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == api.Prefix+"/gc/run":
			writeJSONResp(w, http.StatusAccepted, api.TriggerGCRunResponse{ID: 3, Status: "running"})

			return true

		case r.Method == http.MethodGet && r.URL.Path == api.Prefix+"/gc/runs/3":
			gets++

			status := "running"
			if gets > 1 {
				status = "succeeded"
			}

			writeJSONResp(w, http.StatusOK, api.GCRun{
				ID: 3, Status: status, Trigger: "api", DryRun: false,
				StartedAt:      time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
				ObjectsDeleted: 100, BytesReclaimed: 2048,
			})

			return true
		}

		return false
	}

	c := f.client(t)

	var slept []time.Duration

	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)

		return nil
	}

	var out bytes.Buffer

	cmd := config.GCRunCmd{DryRun: false, Wait: true}
	if err := gcRun(t.Context(), c, renderer{out: &out, json: false}, cmd); err != nil {
		t.Fatalf("gcRun() error = %v", err)
	}

	if gets != 2 {
		t.Errorf("GET /gc/runs/3 called %d times, want 2 (running, then succeeded)", gets)
	}

	if len(slept) != 1 || slept[0] != gcPollInterval {
		t.Errorf("slept %v, want exactly one wait of %v between the two polls", slept, gcPollInterval)
	}

	if !strings.Contains(out.String(), "succeeded") {
		t.Errorf("output = %q, want the terminal status", out.String())
	}

	if !strings.Contains(out.String(), "100") {
		t.Errorf("output = %q, want objects_deleted in the detail view", out.String())
	}
}

// TestGCList asserts the request AND the rendered table -- both the shape sent
// (status and limit as query parameters, covered again here rather than only in
// TestClientVerbs, because this is the path a real user's flags travel) and the
// shape printed.
func TestGCList(t *testing.T) {

	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != api.Prefix+"/gc/runs" {
			return false
		}

		id := int64(41)
		writeJSONResp(w, http.StatusOK, api.GCRunList{
			Items: []api.GCRun{
				{
					ID: 42, Status: "succeeded", Trigger: "interval", DryRun: false,
					StartedAt:      time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC),
					ObjectsDeleted: 12, BytesReclaimed: 4096,
				},
			},
			NextCursor: &id,
		})

		return true
	}

	c := f.client(t)

	var out bytes.Buffer

	cmd := config.GCListCmd{Status: "succeeded", Limit: 10}
	if err := gcList(t.Context(), c, renderer{out: &out, json: false}, cmd); err != nil {
		t.Fatalf("gcList() error = %v", err)
	}

	got := f.last()

	if got.method != http.MethodGet || got.path != "/gc/runs" {
		t.Errorf("request = %s %s, want GET /gc/runs", got.method, got.path)
	}

	if got.rawQuery != "limit=10&status=succeeded" {
		t.Errorf("query = %q, want limit=10&status=succeeded", got.rawQuery)
	}

	tableOut := out.String()
	if !strings.Contains(tableOut, "42") || !strings.Contains(tableOut, "succeeded") {
		t.Errorf("table output = %q, want run 42's row", tableOut)
	}

	// --json must carry NextCursor through: r.list's {"items": ...} shape would
	// silently drop it, which is exactly why gcList uses r.value instead.
	var jsonOut bytes.Buffer

	if err := gcList(t.Context(), c, renderer{out: &jsonOut, json: true}, cmd); err != nil {
		t.Fatalf("gcList() (json) error = %v", err)
	}

	var decoded api.GCRunList
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("decode --json output: %v", err)
	}

	if decoded.NextCursor == nil || *decoded.NextCursor != 41 {
		t.Errorf("--json next_cursor = %v, want 41", decoded.NextCursor)
	}
}

// TestGCListEmpty: no runs is a plain sentence, not an empty table with headers
// and nothing under them.
func TestGCListEmpty(t *testing.T) {

	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSONResp(w, http.StatusOK, api.GCRunList{Items: nil, NextCursor: nil})

		return true
	}

	c := f.client(t)

	var out bytes.Buffer

	if err := gcList(t.Context(), c, renderer{out: &out, json: false}, config.GCListCmd{}); err != nil {
		t.Fatalf("gcList() error = %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "no gc runs" {
		t.Errorf("output = %q, want %q", got, "no gc runs")
	}
}
