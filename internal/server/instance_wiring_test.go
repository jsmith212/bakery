package server

import (
	"testing"

	"github.com/jsmith212/bakery/internal/config"
)

// TestInstanceInfoEchoesTheB1AddressingFlags pins the boot-time echo of the two
// facts a Bakery process cannot derive about itself: its public base URL and the
// public gRPC endpoint (B1, spec 2026-08-15).
//
// Both are pure operator statements about an ingress, both are silently wrong when
// unset behind a proxy, and both are baked verbatim into every generated client
// config. GET /instance exists so an operator can see which of them this process is
// actually running with -- in particular that an EMPTY grpc_external_endpoint
// alongside a non-empty grpc_addr means the snippet generator is DERIVING the
// endpoint, i.e. guessing about their ingress.
func TestInstanceInfoEchoesTheB1AddressingFlags(t *testing.T) {
	tests := []struct {
		name                 string
		cmd                  config.ServeCmd
		wantExternalURL      string
		wantGRPCAddr         string
		wantGRPCExternalEndp string
	}{
		{
			name:                 "all three set",
			cmd:                  config.ServeCmd{ExternalURL: "https://bakery.example.com", GRPCAddr: "0.0.0.0:9092", GRPCExternalEndpoint: "grpcs://reapi.example.com:443"},
			wantExternalURL:      "https://bakery.example.com",
			wantGRPCAddr:         "0.0.0.0:9092",
			wantGRPCExternalEndp: "grpcs://reapi.example.com:443",
		},
		{
			// The deriving case: the operator is relying on a guess, and this is
			// the field that says so.
			name:                 "grpc listener on, no public endpoint stated",
			cmd:                  config.ServeCmd{GRPCAddr: "127.0.0.1:9092"},
			wantExternalURL:      "",
			wantGRPCAddr:         "127.0.0.1:9092",
			wantGRPCExternalEndp: "",
		},
		{
			// REAPI switched off entirely: a bazel/moon snippet is a 409 here.
			name:                 "grpc listener off",
			cmd:                  config.ServeCmd{},
			wantExternalURL:      "",
			wantGRPCAddr:         "",
			wantGRPCExternalEndp: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := instanceInfo(tt.cmd, "v-test")

			if got.ExternalURL != tt.wantExternalURL {
				t.Errorf("external_url = %q, want %q", got.ExternalURL, tt.wantExternalURL)
			}

			if got.GRPCAddr != tt.wantGRPCAddr {
				t.Errorf("grpc_addr = %q, want %q", got.GRPCAddr, tt.wantGRPCAddr)
			}

			if got.GRPCExternalEndpoint != tt.wantGRPCExternalEndp {
				t.Errorf("grpc_external_endpoint = %q, want %q",
					got.GRPCExternalEndpoint, tt.wantGRPCExternalEndp)
			}

			if got.Version != "v-test" {
				t.Errorf("version = %q, want v-test", got.Version)
			}
		})
	}
}
