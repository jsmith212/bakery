package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// TestEndToEndOrgUsageIsHonestAboutStaleness is B2, end to end. cache_backend_usage
// is written by exactly one thing in production (gc.Engine's UpsertBackendUsage),
// so a backend with no row is NOT the same fact as a backend measured at zero, and
// this asserts the wire distinguishes them: measured_at nil vs present.
func TestEndToEndOrgUsageIsHonestAboutStaleness(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	for _, kind := range []string{"sstate", "downloads"} {
		status, body := h.req(http.MethodPost, base+"/backends", `{"kind":"`+kind+`"}`, nil)
		if status != http.StatusCreated {
			t.Fatalf("create %s backend: status = %d, body %s", kind, status, body)
		}
	}

	// BEFORE any measurement: the org-level rollup shows the project at zero,
	// with NO measured_at -- not a live gauge, an honest "never measured".
	status, body := h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/usage", "", nil)
	if status != http.StatusOK {
		t.Fatalf("org usage: status = %d, body %s", status, body)
	}

	var orgUsage ListResponse[OrgProjectUsage]
	if err := json.Unmarshal(body, &orgUsage); err != nil {
		t.Fatalf("decode: %v", err)
	}

	row := findOrgUsage(t, orgUsage.Items, auth.DevProjectSlug)

	if row.MeasuredAt != nil {
		t.Errorf("measured_at = %v, want nil: nothing has reported usage for this project yet", row.MeasuredAt)
	}

	if row.ObjectsCount != 0 || row.LogicalBytes != 0 {
		t.Errorf("usage = %+v, want zero before anything is measured", row)
	}

	// The GC engine reports usage for the sstate backend ONLY.
	sstateID := h.backendID(auth.DevOrgSlug, auth.DevProjectSlug, "sstate")
	h.setUsage(sstateID, 42, 4096)

	status, body = h.req(http.MethodGet, Prefix+"/orgs/"+auth.DevOrgSlug+"/usage", "", nil)
	if status != http.StatusOK {
		t.Fatalf("org usage: status = %d, body %s", status, body)
	}

	if err := json.Unmarshal(body, &orgUsage); err != nil {
		t.Fatalf("decode: %v", err)
	}

	row = findOrgUsage(t, orgUsage.Items, auth.DevProjectSlug)

	if row.MeasuredAt == nil {
		t.Fatal("measured_at is nil after a backend reported usage")
	}

	if row.ObjectsCount != 42 || row.LogicalBytes != 4096 {
		t.Errorf("aggregated usage = %+v, want objects:42 bytes:4096", row)
	}

	// The PER-BACKEND endpoint tells sstate and downloads apart: one measured,
	// one never touched.
	status, body = h.req(http.MethodGet, base+"/usage", "", nil)
	if status != http.StatusOK {
		t.Fatalf("project usage: status = %d, body %s", status, body)
	}

	var projectUsage ListResponse[ProjectBackendUsage]
	if err := json.Unmarshal(body, &projectUsage); err != nil {
		t.Fatalf("decode: %v", err)
	}

	seen := map[string]bool{}

	for _, b := range projectUsage.Items {
		seen[b.Kind] = true

		switch b.Kind {
		case "sstate":
			if b.ObjectsCount == nil || *b.ObjectsCount != 42 {
				t.Errorf("sstate objects_count = %v, want 42", b.ObjectsCount)
			}

			if b.LogicalBytes == nil || *b.LogicalBytes != 4096 {
				t.Errorf("sstate logical_bytes = %v, want 4096", b.LogicalBytes)
			}

			if b.MeasuredAt == nil {
				t.Error("sstate measured_at is nil after it was measured")
			}
		case "downloads":
			if b.ObjectsCount != nil {
				t.Errorf("downloads objects_count = %v, want nil: it was never measured", b.ObjectsCount)
			}

			if b.MeasuredAt != nil {
				t.Errorf("downloads measured_at = %v, want nil: it was never measured", b.MeasuredAt)
			}
		}
	}

	if !seen["sstate"] || !seen["downloads"] {
		t.Fatalf("project usage did not report both configured backends: %+v", projectUsage.Items)
	}
}

func findOrgUsage(t *testing.T, items []OrgProjectUsage, projectSlug string) OrgProjectUsage {
	t.Helper()

	for _, it := range items {
		if it.ProjectSlug == projectSlug {
			return it
		}
	}

	t.Fatalf("no usage row for project %q in %+v", projectSlug, items)

	return OrgProjectUsage{}
}
