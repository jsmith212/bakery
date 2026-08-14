package oci

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestNormalizeUpstream is the "one upstream is one row" gate. Docker Hub answers to
// four names and different clients use different ones -- containerd sends
// ns=docker.io, the registry answering is registry-1.docker.io, Docker's own config
// says index.docker.io. Unnormalized, one tag becomes several rows with independent
// TTLs that can disagree about which digest :latest is.
func TestNormalizeUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "docker.io", in: "docker.io", want: "docker.io"},
		{name: "index.docker.io", in: "index.docker.io", want: "docker.io"},
		{name: "registry-1.docker.io", in: "registry-1.docker.io", want: "docker.io"},
		{name: "registry.hub.docker.com", in: "registry.hub.docker.com", want: "docker.io"},
		{name: "uppercase", in: "Docker.IO", want: "docker.io"},
		{name: "with scheme", in: "https://registry-1.docker.io", want: "docker.io"},
		{name: "trailing slash", in: "index.docker.io/", want: "docker.io"},
		{name: "surrounding space", in: "  docker.io ", want: "docker.io"},
		{name: "other registries are untouched", in: "ghcr.io", want: "ghcr.io"},
		{name: "quay", in: "quay.io", want: "quay.io"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := NormalizeUpstream(tt.in); got != tt.want {
				t.Errorf("NormalizeUpstream(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestParsePolicy pins the fail-CLOSED property: a broken config narrows the
// allowlist, it never widens it, and it never takes the mirror down.
func TestParsePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantDefault string
		wantTTL     time.Duration
		wantAllowed []string
		wantDenied  []string
		wantErr     bool
	}{
		{
			name: "empty config uses the defaults", raw: "",
			wantDefault: "docker.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"docker.io"}, wantDenied: []string{"ghcr.io"}, wantErr: false,
		},
		{
			name: "explicit allowlist", raw: `{"upstreams":["docker.io","ghcr.io"],"tag_ttl":"1m"}`,
			wantDefault: "docker.io", wantTTL: time.Minute,
			wantAllowed: []string{"docker.io", "ghcr.io"}, wantDenied: []string{"quay.io"}, wantErr: false,
		},
		{
			name: "the default upstream is always allowed even if unlisted",
			raw:  `{"default_upstream":"ghcr.io","upstreams":["quay.io"]}`,
			// A config naming a default it may not dial could serve nothing, silently.
			wantDefault: "ghcr.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"ghcr.io", "quay.io"}, wantDenied: []string{"docker.io"}, wantErr: false,
		},
		{
			name: "allowlist entries are normalized",
			raw:  `{"upstreams":["registry-1.docker.io"]}`,
			// containerd will send ns=docker.io for the very host spelled
			// registry-1.docker.io here; without normalization the allowlist would
			// reject it.
			wantDefault: "docker.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"docker.io"}, wantDenied: []string{"ghcr.io"}, wantErr: false,
		},
		{
			name: "malformed json fails closed", raw: `{not json`,
			wantDefault: "docker.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"docker.io"}, wantDenied: []string{"ghcr.io"}, wantErr: true,
		},
		{
			name: "bad ttl keeps the default", raw: `{"tag_ttl":"ten minutes"}`,
			wantDefault: "docker.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"docker.io"}, wantDenied: nil, wantErr: true,
		},
		{
			name: "negative ttl keeps the default", raw: `{"tag_ttl":"-5m"}`,
			wantDefault: "docker.io", wantTTL: defaultTagTTL,
			wantAllowed: []string{"docker.io"}, wantDenied: nil, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pol, err := parsePolicy([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePolicy error = %v, wantErr %v", err, tt.wantErr)
			}

			if pol.defaultUpstream != tt.wantDefault {
				t.Errorf("defaultUpstream = %q, want %q", pol.defaultUpstream, tt.wantDefault)
			}

			if pol.tagTTL != tt.wantTTL {
				t.Errorf("tagTTL = %v, want %v", pol.tagTTL, tt.wantTTL)
			}

			for _, host := range tt.wantAllowed {
				if _, ok := pol.resolveUpstream(host); !ok {
					t.Errorf("resolveUpstream(%q) denied, want allowed", host)
				}
			}

			for _, host := range tt.wantDenied {
				if _, ok := pol.resolveUpstream(host); ok {
					t.Errorf("resolveUpstream(%q) allowed, want denied -- this is the SSRF gate", host)
				}
			}

			// An absent ?ns= must resolve to the default, not fail: podman, skopeo and
			// Docker Engine never send one.
			if host, ok := pol.resolveUpstream(""); !ok || host != tt.wantDefault {
				t.Errorf("resolveUpstream(\"\") = (%q, %v), want (%q, true)", host, ok, tt.wantDefault)
			}
		})
	}
}

// TestParseUpstreamAuth covers the one parsing subtlety: a PAT may itself contain a
// colon, so only the FIRST colon separates the user from the secret.
func TestParseUpstreamAuth(t *testing.T) {
	t.Parallel()

	got, err := ParseUpstreamAuth([]string{
		"index.docker.io=alice:pat:with:colons",
		"ghcr.io=bob:ghp_x",
	})
	if err != nil {
		t.Fatalf("ParseUpstreamAuth: %v", err)
	}

	// The host is normalized, so index.docker.io configures docker.io -- otherwise the
	// credential would be silently unreachable from a ns=docker.io request.
	hub, ok := got["docker.io"]
	if !ok {
		t.Fatalf("docker.io absent; keys are %v", got)
	}

	if hub.Username != "alice" || hub.Password != "pat:with:colons" {
		t.Errorf("docker.io credential username = %q, password split wrongly", hub.Username)
	}

	if _, ok := got["ghcr.io"]; !ok {
		t.Errorf("ghcr.io absent")
	}

	for _, bad := range []string{"nohost", "docker.io=nocolon", "=alice:pw", "docker.io=:pw"} {
		if _, err := ParseUpstreamAuth([]string{bad}); err == nil {
			t.Errorf("ParseUpstreamAuth(%q) = nil error, want a parse error", bad)
		}
	}
}

// TestCredentialNeverRenders is the log-hygiene gate. An upstream credential must be
// incapable of appearing in a log line or a %v, so the type itself refuses -- there is
// no discipline to forget.
func TestCredentialNeverRenders(t *testing.T) {
	t.Parallel()

	c := Credential{Username: "alice", Password: "s3cr3t-pat"}

	if rendered := c.String(); strings.Contains(rendered, "s3cr3t") {
		t.Errorf("Credential.String() leaked the secret: %q", rendered)
	}

	if v := c.LogValue(); strings.Contains(v.String(), "s3cr3t") {
		t.Errorf("Credential.LogValue() leaked the secret: %q", v.String())
	}

	// And through slog, which is how it would actually escape.
	if strings.Contains(slog.AnyValue(c).String(), "s3cr3t") {
		t.Errorf("slog rendering leaked the secret")
	}
}
