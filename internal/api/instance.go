package api

import (
	"net/http"
)

// ---------------------------------------------------------------------------
// GET /instance (B6, spec docs/design/specs/2026-08-15-spa-api-wiring.md).
// SiteAdmin: a boot-time config echo an operator uses to confirm a deployment
// is wired the way they think it is (EXTERNAL_URL set, the gRPC listener
// enabled, dev-login off in production).
//
// IT NEVER TOUCHES PROMETHEUS. /metrics is --metrics-addr-only by hard
// invariant (CLAUDE.md), and this endpoint reports what the process was BOOTED
// WITH -- a static snapshot resolved once in server.Boot and handed to
// api.New, never re-derived per request, and never a proxy or summary of the
// live counters that invariant keeps off the public listener.
// ---------------------------------------------------------------------------

// InstanceInfo is GET /instance's body AND api.Config's carrier for it: the
// same struct is filled in once at boot (server.Boot) and served back
// verbatim, so there is exactly one place that ever computes these values.
type InstanceInfo struct {
	Version         string `json:"version"`
	StorageDriver   string `json:"storage_driver"`
	PublicAddr      string `json:"public_addr"`
	MetricsAddr     string `json:"metrics_addr"`
	GRPCAddr        string `json:"grpc_addr"`
	ExternalURL     string `json:"external_url"`
	OIDCIssuer      string `json:"oidc_issuer"`
	DevLoginEnabled bool   `json:"dev_login_enabled"`

	// GRPCExternalEndpoint is --grpc-external-endpoint (B1). Empty while GRPCAddr
	// is set means the snippet generator DERIVES the endpoint from the request host
	// and the GRPCAddr port -- a guess about the ingress that is right for a
	// single-host deployment and wrong for any other. This field is how an operator
	// sees that they are relying on the guess.
	GRPCExternalEndpoint string `json:"grpc_external_endpoint"`

	AllowSelfServeOrgs   bool `json:"allow_self_serve_orgs"`
	AllowLocalSiteAdmins bool `json:"allow_local_site_admins"`
	AllowMultiInstance   bool `json:"allow_multi_instance"`

	GCEnabled       bool   `json:"gc_enabled"`
	GCInterval      string `json:"gc_interval"`
	GCUsageInterval string `json:"gc_usage_interval"`
	GCGracePeriod   string `json:"gc_grace_period"`
}

// handleGetInstance is B6. SiteAdmin.
func (a *API) handleGetInstance(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, a.instance)

	return nil
}
