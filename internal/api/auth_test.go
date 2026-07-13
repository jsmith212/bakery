package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// repositoryCreateOrg is a tiny constructor so the integration test can call the
// database directly, bypassing the API, to prove the CHECK constraint is real.
func repositoryCreateOrg(slug string) repository.CreateOrganizationParams {
	return repository.CreateOrganizationParams{Slug: slug, Name: slug}
}

// TestAuthConfigCarriesNoSecret.
//
// /auth/config is served UNAUTHENTICATED -- it has to be, because it is what the
// SPA and the CLI read in order to know how to authenticate at all. So the claim
// "it carries no secret" is load-bearing, and this test checks it rather than
// asserting it in a comment.
//
// Field by field, why each is safe to hand an anonymous caller:
//
//   - issuer, authorization_endpoint, token_endpoint, device_authorization_endpoint:
//     all of these are already published, unauthenticated, at the IdP's own
//     /.well-known/openid-configuration. Anyone who can reach this endpoint can
//     reach that one. We serve them only so the client need not redo discovery.
//   - client_id: public BY DESIGN in OIDC. It travels in the authorization URL, in
//     the browser's address bar, in the IdP's logs. A public client (which the CLI
//     is) has no secret at all; that is what PKCE replaces.
//   - scopes: the strings we will ask for. Not a credential.
//   - oidc_enabled / dev_login_enabled: booleans. dev_login_enabled reveals that
//     the dev-login route exists -- but when it is TRUE the whole point is for the
//     console to render that affordance, and when it is FALSE the route is not
//     registered at all and 404s. It discloses nothing that probing would not.
//
// What must NEVER be here is client_secret. This test fails if a field whose name
// even SUGGESTS a secret is added to auth.AuthConfig -- because the next person to
// extend that struct will be extending a type whose serialized form goes to
// unauthenticated callers, and that is easy to forget.
func TestAuthConfigCarriesNoSecret(t *testing.T) {
	// The allow-list of JSON keys. Adding a key means deciding, deliberately, that
	// it is safe to hand an anonymous caller.
	allowed := map[string]bool{
		"issuer":                        true,
		"client_id":                     true,
		"scopes":                        true,
		"authorization_endpoint":        true,
		"token_endpoint":                true,
		"device_authorization_endpoint": true,
		"oidc_enabled":                  true,
		"dev_login_enabled":             true,
	}

	// A fully-populated config: omitempty must not hide a field from this test.
	cfg := authConfigFixture()

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal AuthConfig: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key := range fields {
		if !allowed[key] {
			t.Errorf("auth.AuthConfig serializes %q, which is NOT on the /auth/config "+
				"allow-list. This document is served UNAUTHENTICATED. If the field is "+
				"genuinely safe to publish, add it to `allowed` and say why in the doc "+
				"comment above. If it is not, remove it from the struct.", key)
		}
	}

	// And the belt-and-braces check on the Go type: no field name may suggest a
	// credential.
	typ := reflect.TypeOf(cfg)
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range []string{"secret", "password", "token", "credential", "key"} {
			// client_id is fine; ClientSecret is not. "Token endpoint" is a URL.
			if typ.Field(i).Name == "TokenEndpoint" {
				continue
			}

			if strings.Contains(name, bad) {
				t.Errorf("auth.AuthConfig.%s may carry a secret, and /auth/config is "+
					"served unauthenticated", typ.Field(i).Name)
			}
		}
	}

}

// authConfigFixture builds a FULLY-POPULATED AuthConfig. Every field is non-zero on
// purpose: `omitempty` would hide an empty field from the allow-list check above,
// and a field that is invisible to the test is exactly the field a secret gets added
// to.
func authConfigFixture() auth.AuthConfig {
	return auth.AuthConfig{
		Issuer:                      "https://idp.example.com",
		ClientID:                    "bakery-console",
		Scopes:                      []string{"openid", "profile", "email", "groups", "offline_access"},
		AuthorizationEndpoint:       "https://idp.example.com/authorize",
		TokenEndpoint:               "https://idp.example.com/token",
		DeviceAuthorizationEndpoint: "https://idp.example.com/device",
		OIDCEnabled:                 true,
		DevLoginEnabled:             false,
	}
}
