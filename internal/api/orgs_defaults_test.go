package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// TestEndToEndOrgDefaultsThreeStatePatch is B4, end to end: PATCH /orgs/{org}'s
// default_retention_window / default_quota_bytes carry the SAME three-state
// encoding UpdateBackendRequest's own fields already use -- absent keeps the
// current column, explicit null clears it, a value sets it.
func TestEndToEndOrgDefaultsThreeStatePatch(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug

	// Freshly created (dev-login's SeedDevLogin), both defaults start NULL.
	status, body := h.req(http.MethodGet, base, "", nil)
	if status != http.StatusOK {
		t.Fatalf("get org: status = %d, body %s", status, body)
	}

	var org Org
	if err := json.Unmarshal(body, &org); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if org.DefaultRetentionWindow != nil || org.DefaultQuotaBytes != nil {
		t.Fatalf("dev-org defaults = %+v, want both nil before this test sets them", org)
	}

	// SET both, alongside a rename.
	status, body = h.req(http.MethodPatch, base,
		`{"name":"Dev Org","default_retention_window":"720h","default_quota_bytes":5000000}`, nil)
	if status != http.StatusOK {
		t.Fatalf("patch (set): status = %d, body %s", status, body)
	}

	if err := json.Unmarshal(body, &org); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if org.DefaultRetentionWindow == nil || *org.DefaultRetentionWindow != "720h0m0s" {
		t.Errorf("default_retention_window = %v, want 720h0m0s", org.DefaultRetentionWindow)
	}

	if org.DefaultQuotaBytes == nil || *org.DefaultQuotaBytes != 5000000 {
		t.Errorf("default_quota_bytes = %v, want 5000000", org.DefaultQuotaBytes)
	}

	// ABSENT: a rename that never mentions the defaults must not clobber them.
	status, body = h.req(http.MethodPatch, base, `{"name":"Dev Org Renamed"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("patch (absent): status = %d, body %s", status, body)
	}

	if err := json.Unmarshal(body, &org); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if org.Name != "Dev Org Renamed" {
		t.Errorf("name = %q, want the rename to have applied", org.Name)
	}

	if org.DefaultRetentionWindow == nil || *org.DefaultRetentionWindow != "720h0m0s" {
		t.Errorf("default_retention_window = %v after an omitted patch; it must survive unchanged",
			org.DefaultRetentionWindow)
	}

	if org.DefaultQuotaBytes == nil || *org.DefaultQuotaBytes != 5000000 {
		t.Errorf("default_quota_bytes = %v after an omitted patch; it must survive unchanged",
			org.DefaultQuotaBytes)
	}

	// EXPLICIT NULL: clears both back to "fall back to the per-kind default".
	status, body = h.req(http.MethodPatch, base,
		`{"name":"Dev Org","default_retention_window":null,"default_quota_bytes":null}`, nil)
	if status != http.StatusOK {
		t.Fatalf("patch (clear): status = %d, body %s", status, body)
	}

	if err := json.Unmarshal(body, &org); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if org.DefaultRetentionWindow != nil {
		t.Errorf("default_retention_window = %v, want nil after an explicit null", org.DefaultRetentionWindow)
	}

	if org.DefaultQuotaBytes != nil {
		t.Errorf("default_quota_bytes = %v, want nil after an explicit null", org.DefaultQuotaBytes)
	}

	// A zero/negative window and quota are refused with the closed vocabulary,
	// not a 500 from the interval/bigint CHECKs.
	status, _ = h.req(http.MethodPatch, base, `{"name":"Dev Org","default_retention_window":"0h"}`, nil)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("default_retention_window=0h: status = %d, want 422", status)
	}

	status, _ = h.req(http.MethodPatch, base, `{"name":"Dev Org","default_quota_bytes":-1}`, nil)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("default_quota_bytes=-1: status = %d, want 422", status)
	}
}
