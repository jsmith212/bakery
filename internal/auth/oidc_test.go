package auth

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// TestVerifyIDToken is the load-bearing test in this package.
//
// A "verifier" that base64-decodes a JWT and trusts the payload accepts a token
// that anyone can mint, which is a complete authentication bypass wearing the
// costume of working code. The wrong_key case below is the one that catches it:
// the token is structurally perfect, carries the right issuer, audience and
// expiry, and names the IdP's real key id in its header -- it is signed with a key
// the IdP never published. A decoder says yes. A verifier says no.
func TestVerifyIDToken(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.provider(t)

	tests := []struct {
		name    string
		token   func(t *testing.T) string
		wantErr bool
		errText string
	}{
		{
			name:    "a token signed by the provider verifies",
			token:   func(t *testing.T) string { return idp.signIDToken(t, defaultClaims(idp)) },
			wantErr: false,
		},
		{
			name: "a token signed by the WRONG KEY is rejected",
			token: func(t *testing.T) string {
				// Right issuer, right audience, unexpired, and it even claims the
				// provider's real kid. Only the signature is wrong.
				return idp.signWithWrongKey(t, defaultClaims(idp))
			},
			wantErr: true,
			errText: "signature",
		},
		{
			name: "a token for another audience is rejected",
			token: func(t *testing.T) string {
				c := defaultClaims(idp)
				c.aud = "some-other-client"

				return idp.signIDToken(t, c)
			},
			wantErr: true,
			errText: "audience",
		},
		{
			name: "an expired token is rejected",
			token: func(t *testing.T) string {
				c := defaultClaims(idp)
				c.issuedAt = time.Now().Add(-2 * time.Hour)
				c.expires = time.Now().Add(-time.Hour)

				return idp.signIDToken(t, c)
			},
			wantErr: true,
			errText: "expired",
		},
		{
			name:    "a garbage token is rejected",
			token:   func(_ *testing.T) string { return "not.a.jwt" },
			wantErr: true,
		},
		{
			name:    "an empty token is rejected",
			token:   func(_ *testing.T) string { return "" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := provider.VerifyIDToken(t.Context(), tt.token(t))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("VerifyIDToken() succeeded for subject %q, want a rejection", id.Subject)
				}

				if !errors.Is(err, ErrTokenInvalid) {
					t.Errorf("VerifyIDToken() error = %v, want it to wrap ErrTokenInvalid", err)
				}

				if tt.errText != "" && !errContains(err, tt.errText) {
					t.Errorf("VerifyIDToken() error = %q, want it to mention %q", err, tt.errText)
				}

				return
			}

			if err != nil {
				t.Fatalf("VerifyIDToken() error = %v, want success", err)
			}

			if id.Subject != "subject-1" {
				t.Errorf("Subject = %q, want %q", id.Subject, "subject-1")
			}

			if id.Email != "jackson@example.com" {
				t.Errorf("Email = %q, want %q", id.Email, "jackson@example.com")
			}

			if want := []string{"platform", "yocto-admins"}; !reflect.DeepEqual(id.Groups, want) {
				t.Errorf("Groups = %v, want %v", id.Groups, want)
			}
		})
	}
}

// TestVerifyRejectsAnUnknownKeyID covers the other half of the JWKS story: a
// token whose kid is not in the key set at all. go-oidc refetches the JWKS when it
// sees an unknown kid (that is the OIDC key-rotation strategy), so this also
// proves the refetch does not turn into an accept.
func TestVerifyRejectsAnUnknownKeyID(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.provider(t)

	token := idp.sign(t, defaultClaims(idp), idp.evilKey, "a-key-that-was-never-published")

	if _, err := provider.VerifyIDToken(t.Context(), token); err == nil {
		t.Fatal("VerifyIDToken() accepted a token whose signing key is not in the JWKS")
	}
}

// TestNonceIsVerifiedByUs documents a sharp edge in go-oidc: Verify() does NOT
// check the nonce. oidc.Nonce(n) only SENDS it. If we did not compare it
// ourselves, an ID token captured from one login could be replayed into another.
func TestNonceIsVerifiedByUs(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.provider(t)

	c := defaultClaims(idp)
	c.nonce = "the-attackers-nonce"
	token := idp.signIDToken(t, c)

	// The library alone accepts it: no nonce expectation, no complaint.
	if _, err := provider.verify(t.Context(), token, ""); err != nil {
		t.Fatalf("verify() with no expected nonce = %v, want success (proving go-oidc does not check it)", err)
	}

	// With an expectation, our own check is what refuses it.
	_, err := provider.verify(t.Context(), token, "the-nonce-we-actually-sent")
	if !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("verify() with a mismatched nonce = %v, want ErrNonceMismatch", err)
	}

	// And the matching nonce passes.
	if _, err := provider.verify(t.Context(), token, "the-attackers-nonce"); err != nil {
		t.Fatalf("verify() with the matching nonce = %v, want success", err)
	}
}

// TestAuthCodeURLCarriesStateNonceAndPKCE asserts on the WIRE, not on intent.
func TestAuthCodeURLCarriesStateNonceAndPKCE(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.provider(t)

	req, err := provider.AuthCodeURL()
	if err != nil {
		t.Fatalf("AuthCodeURL() error = %v", err)
	}

	parsed, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}

	q := parsed.Query()

	if q.Get("state") != req.State || req.State == "" {
		t.Errorf("state on the wire = %q, want the generated %q", q.Get("state"), req.State)
	}

	if q.Get("nonce") != req.Nonce || req.Nonce == "" {
		t.Errorf("nonce on the wire = %q, want the generated %q", q.Get("nonce"), req.Nonce)
	}

	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256 (a plain challenge is not PKCE)", got)
	}

	if want := oauth2.S256ChallengeFromVerifier(req.Verifier); q.Get("code_challenge") != want {
		t.Errorf("code_challenge = %q, want S256(verifier) = %q", q.Get("code_challenge"), want)
	}

	// Two requests must not share a state or a nonce, or neither binds anything.
	second, err := provider.AuthCodeURL()
	if err != nil {
		t.Fatalf("AuthCodeURL() error = %v", err)
	}

	if second.State == req.State || second.Nonce == req.Nonce || second.Verifier == req.Verifier {
		t.Error("two authorization requests reused a state, nonce or verifier; they must be per-request")
	}
}

// TestExchangeSendsTheVerifierAndChecksTheNonce drives a full code exchange
// against the fake IdP and asserts the PKCE verifier actually reached the token
// endpoint.
func TestExchangeSendsTheVerifierAndChecksTheNonce(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.provider(t)

	req, err := provider.AuthCodeURL()
	if err != nil {
		t.Fatalf("AuthCodeURL() error = %v", err)
	}

	c := defaultClaims(idp)
	c.nonce = req.Nonce
	idp.issueCode("good-code", idp.signIDToken(t, c))

	id, err := provider.Exchange(t.Context(), "good-code", req.Verifier, req.Nonce)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	if id.Subject != "subject-1" {
		t.Errorf("Subject = %q, want subject-1", id.Subject)
	}

	if id.RefreshToken != "refresh-token" {
		t.Errorf("RefreshToken = %q, want the provider's refresh token (offline_access)", id.RefreshToken)
	}

	if got := idp.lastTokenForm["code_verifier"]; got != req.Verifier {
		t.Errorf("code_verifier at the token endpoint = %q, want %q", got, req.Verifier)
	}

	// A token whose nonce is not the one we sent must be refused even though its
	// signature is perfectly good.
	c.nonce = "someone-elses-nonce"
	idp.issueCode("replayed-code", idp.signIDToken(t, c))

	if _, err := provider.Exchange(t.Context(), "replayed-code", req.Verifier, req.Nonce); !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("Exchange() with a replayed token = %v, want ErrNonceMismatch", err)
	}
}

// TestDeviceAuthorizationEndpointDiscovery covers the CLI's device grant: the
// endpoint comes out of discovery, and it may legitimately be ABSENT (it is
// RFC 8414 metadata, not core OIDC Discovery). Absent must be detectable at boot,
// not a mystery at first use.
func TestDeviceAuthorizationEndpointDiscovery(t *testing.T) {
	t.Run("advertised", func(t *testing.T) {
		idp := newFakeIDP(t)
		provider := idp.provider(t)

		if !provider.HasDeviceGrant() {
			t.Fatal("HasDeviceGrant() = false, want true when discovery advertises the endpoint")
		}

		cfg := provider.AuthConfig()
		if want := idp.issuer() + "/device"; cfg.DeviceAuthorizationEndpoint != want {
			t.Errorf("DeviceAuthorizationEndpoint = %q, want %q", cfg.DeviceAuthorizationEndpoint, want)
		}

		// The oauth2.Config the device grant will use must carry it too, or
		// DeviceAuth fails with "endpoint missing DeviceAuthURL".
		if provider.oauth.Endpoint.DeviceAuthURL == "" {
			t.Error("oauth2.Config.Endpoint.DeviceAuthURL is empty; DeviceAuth would fail")
		}

		// AuthStyleAutoDetect doubles every failing device poll, because each
		// authorization_pending is an HTTP 400 and the probe only caches on success.
		if provider.oauth.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
			t.Errorf("AuthStyle = %v, want AuthStyleInParams", provider.oauth.Endpoint.AuthStyle)
		}
	})

	t.Run("absent, and overridable", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.withDeviceEndpoint = false

		provider, err := NewProvider(t.Context(), OIDCConfig{
			Issuer: idp.issuer(), ClientID: idp.clientID, ClientSecret: "secret",
			RedirectURL: "https://bakery.example.com/cb",
			Scopes:      nil, GroupsClaim: "", DeviceAuthURL: "",
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}

		if provider.HasDeviceGrant() {
			t.Fatal("HasDeviceGrant() = true, but discovery advertised no device endpoint")
		}

		// The config override is the escape hatch for exactly this provider.
		overridden, err := NewProvider(t.Context(), OIDCConfig{
			Issuer: idp.issuer(), ClientID: idp.clientID, ClientSecret: "secret",
			RedirectURL: "https://bakery.example.com/cb",
			Scopes:      nil, GroupsClaim: "",
			DeviceAuthURL: "https://idp.example.com/device",
		})
		if err != nil {
			t.Fatalf("NewProvider with an override: %v", err)
		}

		if !overridden.HasDeviceGrant() {
			t.Error("the DeviceAuthURL override did not take effect")
		}
	})
}

// TestGroupsClaimIsConfigurable: providers disagree about the claim name (Auth0
// namespaces it), so reading it by a hardcoded key would silently yield "no
// groups" -- which the reconciler must then refuse as a login.
func TestGroupsClaimIsConfigurable(t *testing.T) {
	idp := newFakeIDP(t)

	provider, err := NewProvider(t.Context(), OIDCConfig{
		Issuer: idp.issuer(), ClientID: idp.clientID, ClientSecret: "secret",
		RedirectURL: "https://bakery.example.com/cb", Scopes: nil,
		GroupsClaim:   "https://bakery.example.com/groups",
		DeviceAuthURL: "",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	// The token carries `groups`, not the namespaced claim we configured.
	id, err := provider.VerifyIDToken(t.Context(), idp.signIDToken(t, defaultClaims(idp)))
	if err != nil {
		t.Fatalf("VerifyIDToken() error = %v", err)
	}

	if len(id.Groups) != 0 {
		t.Errorf("Groups = %v, want none: the configured claim name is absent from the token", id.Groups)
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{name: "a bearer token", header: "Bearer abc123", want: "abc123", wantOK: true},
		{name: "the scheme is case-insensitive", header: "bearer abc123", want: "abc123", wantOK: true},
		{name: "no header", header: "", want: "", wantOK: false},
		{name: "another scheme", header: "Basic abc123", want: "", wantOK: false},
		{name: "no token", header: "Bearer", want: "", wantOK: false},
		{name: "an empty token", header: "Bearer   ", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, "GET", "/", tt.header)

			got, ok := bearerToken(r)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tt.header, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestNewProviderWithoutAnIssuer(t *testing.T) {
	_, err := NewProvider(t.Context(), OIDCConfig{
		Issuer: "", ClientID: "", ClientSecret: "", RedirectURL: "",
		Scopes: nil, GroupsClaim: "", DeviceAuthURL: "",
	})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("NewProvider() with no issuer = %v, want ErrNoProvider", err)
	}
}

func TestRandomTokenIsUnpredictable(t *testing.T) {
	seen := make(map[string]struct{}, 100)

	for range 100 {
		tok, err := randomToken()
		if err != nil {
			t.Fatalf("randomToken() error = %v", err)
		}

		if len(tok) < 40 {
			t.Fatalf("randomToken() = %q, want at least 256 bits of entropy", tok)
		}

		if _, dup := seen[tok]; dup {
			t.Fatalf("randomToken() repeated %q", tok)
		}

		seen[tok] = struct{}{}
	}

	// A base64url token must be URL- and header-safe.
	tok, _ := randomToken()
	if strings.ContainsAny(tok, "+/= ") {
		t.Errorf("randomToken() = %q, want base64url with no padding", tok)
	}
}
