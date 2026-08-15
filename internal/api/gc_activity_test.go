package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// TestEndToEndOrgGCActivityIsScopedToTheOrg is B7, end to end: one gc_runs row
// spans backends in TWO different orgs (a real run's ordinary shape -- retention
// is instance-wide), and GET /orgs/{org}/gc/activity for ONE of them must show
// only that org's own backend rows. The dev user is the local owner of BOTH
// orgs created here (self-serve grants the creator ownership), so this proves
// the QUERY's own scoping (the org_id join), not merely a permission denial --
// the caller CAN see both orgs and must still never see the other's activity
// through this endpoint.
func TestEndToEndOrgGCActivityIsScopedToTheOrg(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	devBase := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	status, body := h.req(http.MethodPost, devBase+"/backends", `{"kind":"sstate"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create dev-org backend: status = %d, body %s", status, body)
	}

	status, body = h.req(http.MethodPost, Prefix+"/orgs", `{"slug":"other-co","name":"Other Co"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create other-co: status = %d, body %s", status, body)
	}

	status, body = h.req(http.MethodPost, Prefix+"/orgs/other-co/projects", `{"slug":"widget","name":"Widget"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create other-co/widget: status = %d, body %s", status, body)
	}

	status, body = h.req(http.MethodPost,
		Prefix+"/orgs/other-co/projects/widget/backends", `{"kind":"sstate"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create other-co backend: status = %d, body %s", status, body)
	}

	devBackendID := h.backendID(auth.DevOrgSlug, auth.DevProjectSlug, "sstate")
	otherBackendID := h.backendID("other-co", "widget", "sstate")

	// ONE run, two backends, two orgs -- exactly the shape a real instance-wide
	// sweep produces.
	run := h.startGCRun()
	h.recordGCRunBackend(run.ID, devBackendID, 3, 300)
	h.recordGCRunBackend(run.ID, otherBackendID, 7, 700)

	status, body = h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/gc/activity", "", nil)
	if status != http.StatusOK {
		t.Fatalf("gc/activity: status = %d, body %s", status, body)
	}

	var activity GCActivityList
	if err := json.Unmarshal(body, &activity); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(activity.Items) != 1 {
		t.Fatalf("items = %d, want exactly the one run (%+v)", len(activity.Items), activity.Items)
	}

	if activity.Items[0].RunID != run.ID {
		t.Errorf("run_id = %d, want %d", activity.Items[0].RunID, run.ID)
	}

	if len(activity.Items[0].Backends) != 1 {
		t.Fatalf("backends = %+v, want exactly ONE row: dev-org's own", activity.Items[0].Backends)
	}

	got := activity.Items[0].Backends[0]
	if got.ProjectSlug != auth.DevProjectSlug || got.Kind != "sstate" {
		t.Errorf("backend row = %+v, want project:%s kind:sstate", got, auth.DevProjectSlug)
	}

	if got.ObjectsDeleted != 3 || got.BytesFreed != 300 {
		t.Errorf("backend row = %+v, want objects_deleted:3 bytes_freed:300", got)
	}

	for _, b := range activity.Items[0].Backends {
		if b.ProjectSlug == "widget" {
			t.Error("other-co's backend row leaked into dev-org's activity")
		}
	}

	// And the OTHER org's own view shows exactly its own row, symmetrically.
	status, body = h.req(http.MethodGet, Prefix+"/orgs/other-co/gc/activity", "", nil)
	if status != http.StatusOK {
		t.Fatalf("gc/activity (other-co): status = %d, body %s", status, body)
	}

	if err := json.Unmarshal(body, &activity); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(activity.Items) != 1 || len(activity.Items[0].Backends) != 1 {
		t.Fatalf("other-co activity = %+v, want exactly one run, one backend row", activity.Items)
	}

	got = activity.Items[0].Backends[0]
	if got.ProjectSlug != "widget" || got.ObjectsDeleted != 7 || got.BytesFreed != 700 {
		t.Errorf("other-co's own row = %+v, want project:widget objects_deleted:7 bytes_freed:700", got)
	}
}

// TestEndToEndOrgGCActivityClampsAnOverLargeLimit reuses gcRunLimit (already
// unit-tested by TestListGCRunsPagination's own suite); this just proves the
// wiring: this endpoint's ?limit= goes through the same clamp, never a 422.
func TestEndToEndOrgGCActivityClampsAnOverLargeLimit(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	status, body := h.req(http.MethodGet,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/gc/activity?limit=999999", "", nil)
	if status != http.StatusOK {
		t.Errorf("an over-large ?limit=: status = %d, want 200 (body %s)", status, body)
	}

	status, _ = h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/gc/activity?limit=0", "", nil)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("?limit=0: status = %d, want 422", status)
	}
}

// TestEndToEndOrgGCActivityHasNoRowsWhenNothingWasSwept proves the absence
// case: a project with a configured backend but no gc_run_backends row at all
// (nothing has swept it yet) reports an EMPTY activity list, never a
// fabricated zero entry.
func TestEndToEndOrgGCActivityHasNoRowsWhenNothingWasSwept(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	status, body := h.req(http.MethodPost,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/projects/"+auth.DevProjectSlug+"/backends",
		`{"kind":"sstate"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create backend: status = %d, body %s", status, body)
	}

	status, body = h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/gc/activity", "", nil)
	if status != http.StatusOK {
		t.Fatalf("gc/activity: status = %d, body %s", status, body)
	}

	var activity GCActivityList
	if err := json.Unmarshal(body, &activity); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(activity.Items) != 0 {
		t.Errorf("items = %+v, want empty: no gc_run_backends row exists yet", activity.Items)
	}
}
