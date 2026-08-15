package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestEndToEndInstanceEchoesBootConfig is B6, end to end: a site admin (dev-login
// always seeds one) reads back the boot-time config the test harness was built
// with. TestGuardAuthorizationMatrix already proves AccessSiteAdmin's 404/403
// ladder generically; this proves the wiring -- that api.Config.Instance
// actually reaches the response, unmodified, and that the endpoint is a STATIC
// echo rather than a live re-derivation.
func TestEndToEndInstanceEchoesBootConfig(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	status, body := h.req(http.MethodGet, Prefix+"/instance", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /instance: status = %d, body %s", status, body)
	}

	var info InstanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The harness's own newHarness does not set api.Config.Instance at all, so
	// this pins the ZERO VALUE reaching the wire untouched -- proof that
	// handleGetInstance serves exactly a.instance and computes nothing itself.
	if info.StorageDriver != "" {
		t.Errorf("storage_driver = %q, want the zero value: this harness never set Config.Instance", info.StorageDriver)
	}

	if info.DevLoginEnabled {
		t.Error("dev_login_enabled = true, but Config.Instance was never populated by this harness")
	}
}

// TestEndToEndInstanceServesWhateverConfigCarries proves the other half: when
// api.Config.Instance IS populated (as server.Boot always populates it), the
// endpoint serves exactly that value.
func TestEndToEndInstanceServesWhateverConfigCarries(t *testing.T) {
	h := newHarness(t)

	want := InstanceInfo{
		Version: "test-build", StorageDriver: "local",
		PublicAddr: "0.0.0.0:8080", MetricsAddr: "127.0.0.1:9090", GRPCAddr: "127.0.0.1:9092",
		ExternalURL: "https://bakery.example.com", OIDCIssuer: "", DevLoginEnabled: true,
		AllowSelfServeOrgs: true, AllowLocalSiteAdmins: true, AllowMultiInstance: false,
		GCEnabled: true, GCInterval: "6h0m0s", GCUsageInterval: "6h0m0s", GCGracePeriod: "24h0m0s",
	}

	h.api.instance = want

	h.devLogin()

	status, body := h.req(http.MethodGet, Prefix+"/instance", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /instance: status = %d, body %s", status, body)
	}

	var got InstanceInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got != want {
		t.Errorf("GET /instance = %+v, want %+v", got, want)
	}

	// NEVER a Prometheus counter or gauge on this response -- it is a config
	// echo, not a metrics proxy (CLAUDE.md: /metrics is --metrics-addr-only).
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	for key := range raw {
		if key == "prometheus" || key == "metrics" {
			t.Errorf("GET /instance carries a %q field; it must never touch Prometheus", key)
		}
	}
}
