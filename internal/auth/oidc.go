package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Errors the caller branches on.
var (
	// ErrNoProvider means OIDC is not configured. Every OIDC-dependent route must
	// fail with this rather than pretend to work.
	ErrNoProvider = errors.New("auth: OIDC is not configured")

	// ErrTokenInvalid means the presented token failed verification: bad
	// signature, wrong issuer, wrong audience, or expired. It is deliberately
	// coarse -- the caller renders a 401 and must not leak which check failed.
	ErrTokenInvalid = errors.New("auth: the token is not valid")

	// ErrStateMismatch means the callback's state did not match the session's.
	// This is a CSRF attempt or a stale tab; either way, refuse.
	ErrStateMismatch = errors.New("auth: the authorization state does not match")

	// ErrNonceMismatch means the ID token's nonce did not match the one we sent.
	// go-oidc does NOT check this for us -- see verifyNonce.
	ErrNonceMismatch = errors.New("auth: the ID token nonce does not match")

	// ErrNoIDToken means the token response carried no id_token. An OAuth2
	// provider that is not an OIDC provider will do this; we cannot identify the
	// user from an access token alone, so it is fatal.
	ErrNoIDToken = errors.New("auth: the token response carried no id_token")

	// ErrNoDeviceEndpoint means the provider's discovery document omitted
	// device_authorization_endpoint and no override was configured, so the CLI's
	// device grant cannot work.
	ErrNoDeviceEndpoint = errors.New("auth: the provider advertises no device authorization endpoint")
)

// DefaultGroupsClaim is the ID-token claim the site and org roles are derived
// from. Providers differ (Keycloak and Okta need an explicit mapper; Auth0 wants
// a namespaced claim), so it is configurable.
const DefaultGroupsClaim = "groups"

// The OIDC Core "aggregated and distributed claims" members (§5.6.2). Azure AD
// uses them for the GROUPS OVERAGE: when a user is in more than ~200 groups, the
// `groups` claim is REPLACED by a `_claim_names` entry naming a `_claim_sources`
// endpoint on Microsoft Graph. The ID token then carries NO `groups` claim at
// all -- byte-for-byte identical, to a naive reader, to a user who is in no
// groups. That collision is the bug this package exists to refuse.
const (
	claimNamesClaim   = "_claim_names"
	claimSourcesClaim = "_claim_sources"
)

// OIDCConfig is what internal/config resolved for the identity provider.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string

	// GroupsClaim overrides the ID-token claim carrying the user's groups.
	// Empty means DefaultGroupsClaim.
	GroupsClaim string

	// DeviceAuthURL overrides the discovery document's
	// device_authorization_endpoint. That field is RFC 8414 / RFC 8628 metadata,
	// NOT core OIDC Discovery, so a conformant provider may legitimately omit it
	// -- and go-oidc will happily succeed with it absent, leaving the failure to
	// surface much later as "oauth2: endpoint missing DeviceAuthURL". This is the
	// escape hatch.
	DeviceAuthURL string
}

// Provider wraps the discovered OIDC provider and the OAuth2 client.
//
// Build ONE at boot and share it. Do not build one per request: the verifier
// holds the JWKS cache, and go-oidc's Provider.Verifier memoizes a single remote
// key set on the Provider under a mutex, whereas Provider.VerifierContext
// allocates a FRESH key set on every call -- per-request use of the latter means
// a cold JWKS cache and an HTTP round trip to the IdP on every single request.
type Provider struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config

	issuer      string
	clientID    string
	groupsClaim string

	deviceAuthURL string
	tokenURL      string
	authURL       string
	scopes        []string
}

// NewProvider performs OIDC discovery and builds the verifier and OAuth2 client.
//
// It is a boot-time call: a failure here must stop the process, not be retried
// per request.
func NewProvider(ctx context.Context, cfg OIDCConfig) (*Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, ErrNoProvider
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", cfg.Issuer, err)
	}

	endpoint := provider.Endpoint()

	// AuthStyleAutoDetect -- which is exactly what provider.Endpoint() returns --
	// probes AuthStyleInHeader first and RETRIES with AuthStyleInParams on any
	// error. Every `authorization_pending` poll of the device grant is an HTTP
	// 400, i.e. an error, and the probe result is cached only on SUCCESS, so the
	// autodetect never settles and EVERY poll is sent twice for the entire
	// duration of the device flow. Pinning the style is not a micro-optimization;
	// it halves the request rate against the IdP's token endpoint.
	endpoint.AuthStyle = oauth2.AuthStyleInParams

	deviceAuthURL := cfg.DeviceAuthURL
	if deviceAuthURL == "" {
		deviceAuthURL = endpoint.DeviceAuthURL
	}

	endpoint.DeviceAuthURL = deviceAuthURL

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email", DefaultGroupsClaim, oidc.ScopeOfflineAccess}
	}

	groupsClaim := cfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = DefaultGroupsClaim
	}

	return &Provider{
		provider: provider,
		// Verifier (not VerifierContext) so the JWKS cache is shared. The zero
		// values of oidc.Config's Skip* fields are what we want: issuer, audience,
		// expiry and SIGNATURE are all checked. Nothing here is skipped.
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}), //nolint:exhaustruct // every Skip* must stay false
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		issuer:        cfg.Issuer,
		clientID:      cfg.ClientID,
		groupsClaim:   groupsClaim,
		deviceAuthURL: deviceAuthURL,
		tokenURL:      endpoint.TokenURL,
		authURL:       endpoint.AuthURL,
		scopes:        scopes,
	}, nil
}

// AuthConfig is the /auth/config payload: what the SPA and the CLI need to
// configure themselves without redoing discovery.
//
// The API agent serves this; this package produces it.
type AuthConfig struct {
	Issuer                      string   `json:"issuer"`
	ClientID                    string   `json:"client_id"`
	Scopes                      []string `json:"scopes"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint               string   `json:"token_endpoint,omitempty"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint,omitempty"`

	// OIDCEnabled is false when the server booted with no issuer configured. The
	// console renders the dev-login affordance instead of an OIDC button.
	OIDCEnabled bool `json:"oidc_enabled"`

	// DevLoginEnabled mirrors DEV_LOGIN_ENABLED. It is REPORTED here, never SET
	// here: this struct is serialized outward and never parsed inward.
	DevLoginEnabled bool `json:"dev_login_enabled"`
}

// AuthConfig renders the provider half of the /auth/config document.
func (p *Provider) AuthConfig() AuthConfig {
	return AuthConfig{
		Issuer:                      p.issuer,
		ClientID:                    p.clientID,
		Scopes:                      slices.Clone(p.scopes),
		AuthorizationEndpoint:       p.authURL,
		TokenEndpoint:               p.tokenURL,
		DeviceAuthorizationEndpoint: p.deviceAuthURL,
		OIDCEnabled:                 true,
		DevLoginEnabled:             false,
	}
}

// HasDeviceGrant reports whether the CLI's device flow can work against this
// provider. Boot should log loudly when it cannot.
func (p *Provider) HasDeviceGrant() bool { return p.deviceAuthURL != "" }

// Identity is a VERIFIED set of claims from an ID token. It is not a Principal:
// it is what the IdP asserts, before Bakery has decided what it means. The
// reconciler turns one into the other.
type Identity struct {
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Groups      []string

	// GroupsPresent says whether Groups is the IdP's ANSWER or merely our
	// FAILURE TO READ ONE. It is the whole point of this struct.
	//
	//   true,  len(Groups) == 0 -> the IdP asserts this user is in no groups.
	//                              Admissible: they hold local memberships only.
	//   false                   -> we could not read the claim. NOT "no groups".
	//                              The reconciler must refuse the login and write
	//                              nothing.
	//
	// A plain []string cannot hold that distinction -- nil and empty are the same
	// answer to "how many groups" and opposite answers to "did you read it" -- and
	// that inability IS the bug. Treating an unreadable claim as zero groups NULLs
	// every oidc_role, deletes every membership with no local grant, and cascades
	// the user's project roles and API keys away. Irreversibly, on a login that
	// looked entirely successful.
	GroupsPresent bool

	// IssuedAt drives re-reconciliation on the Bearer path: the CLI has no
	// server-side login callback, so a token issued AFTER the user's recorded
	// last_login_at is our signal that the IdP re-asserted their groups and we
	// should reconcile again.
	IssuedAt time.Time

	// RefreshToken is present only on the authorization-code exchange, and only
	// when offline_access was granted.
	RefreshToken string
}

// idTokenClaims is the shape we unmarshal out of the ID token's raw payload.
// The groups claim is read by name because providers disagree about it.
type idTokenClaims struct {
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// AuthRequest is one browser authorization attempt. State, Nonce and Verifier
// are secrets: they go into the user's session, never into a log or a URL we
// keep.
type AuthRequest struct {
	URL      string
	State    string
	Nonce    string
	Verifier string
}

// AuthCodeURL begins the browser authorization-code flow with state, nonce and
// PKCE (S256).
//
// All three are load-bearing and none is optional:
//   - state binds the callback to this browser session (CSRF).
//   - nonce binds the ID TOKEN to this request (replay). go-oidc does not check
//     it; we do, in verifyNonce.
//   - PKCE binds the code to this client, so an intercepted code is useless.
func (p *Provider) AuthCodeURL() (AuthRequest, error) {
	state, err := randomToken()
	if err != nil {
		return AuthRequest{}, fmt.Errorf("generate state: %w", err)
	}

	nonce, err := randomToken()
	if err != nil {
		return AuthRequest{}, fmt.Errorf("generate nonce: %w", err)
	}

	verifier := oauth2.GenerateVerifier()

	url := p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOffline,
	)

	return AuthRequest{URL: url, State: state, Nonce: nonce, Verifier: verifier}, nil
}

// Exchange completes the authorization-code flow: it swaps the code for tokens,
// verifies the ID token's SIGNATURE against the provider's JWKS along with its
// issuer, audience and expiry, and then checks the nonce itself.
func (p *Provider) Exchange(ctx context.Context, code, verifier, wantNonce string) (Identity, error) {
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: exchange authorization code: %w", ErrTokenInvalid, err)
	}

	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, ErrNoIDToken
	}

	id, err := p.verify(ctx, raw, wantNonce)
	if err != nil {
		return Identity{}, err
	}

	id.RefreshToken = token.RefreshToken

	return id, nil
}

// VerifyIDToken verifies a raw ID token presented as a Bearer credential.
//
// The device grant carries no nonce (there is no browser redirect to bind), so
// none is expected here. Signature, issuer, audience and expiry are all still
// checked -- this is the CLI's entire authentication, on every request.
func (p *Provider) VerifyIDToken(ctx context.Context, raw string) (Identity, error) {
	return p.verify(ctx, raw, "")
}

// verify is the one place an ID token is turned into claims we will act on.
func (p *Provider) verify(ctx context.Context, raw, wantNonce string) (Identity, error) {
	// This checks the SIGNATURE against the provider's JWKS (fetching and caching
	// it, refetching only when the token's kid is unknown), plus iss, aud and exp.
	// A verifier that decodes a JWT without checking its signature accepts a token
	// anyone can mint; there is no shortcut here worth taking.
	tok, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}

	if err := verifyNonce(tok.Nonce, wantNonce); err != nil {
		return Identity{}, err
	}

	var claims idTokenClaims
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: decode id_token claims: %w", ErrTokenInvalid, err)
	}

	groups, present, err := p.groups(tok)
	if err != nil {
		return Identity{}, err
	}

	return Identity{
		Issuer:        tok.Issuer,
		Subject:       tok.Subject,
		Email:         strings.TrimSpace(claims.Email),
		DisplayName:   displayName(claims),
		Groups:        groups,
		GroupsPresent: present,
		IssuedAt:      tok.IssuedAt,
		RefreshToken:  "",
	}, nil
}

// verifyNonce compares the ID token's nonce against the one we sent.
//
// go-oidc's Verify() does NOT do this. It returns success on a token carrying an
// attacker-chosen nonce; oidc.Nonce(n) only SENDS the nonce on the authorization
// request. Verified by executing it: a token with an unexpected nonce verifies
// clean. So the check has to live here, and it is not optional -- the nonce is
// what stops an ID token captured from one login being replayed into another.
//
// The comparison is constant-time. The nonce is a secret we generated and are
// comparing against attacker-supplied bytes, which is exactly the shape that
// leaks through a short-circuiting bytes.Equal.
func verifyNonce(got, want string) error {
	if want == "" {
		// The device grant has no nonce. There is nothing to bind and nothing to
		// compare; the token's audience and signature carry the whole burden.
		return nil
	}

	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return ErrNonceMismatch
	}

	return nil
}

// groups pulls the group claim out by its configured name, and reports whether
// it could be READ at all.
//
// This is the boundary where "the IdP says zero groups" and "we could not read
// the groups" are separated, and they must never be conflated again downstream.
// It does NOT decide what to do about an unreadable claim -- that is the
// reconciler's, which fails the login closed. What it decides is that the caller
// is TOLD, rather than handed an empty slice that lies by omission.
//
// Unreadable (present == false), fail-closed, in every one of these cases:
//
//   - the claim is ABSENT. Azure AD's groups overage does exactly this to real,
//     correctly-configured users, and so does a provider whose group mapper was
//     never configured.
//   - `_claim_names` names the groups claim (OIDC Core §5.6.2 distributed
//     claims). The authoritative set lives on a `_claim_sources` endpoint we do
//     not fetch, so whatever else the token says about groups is not the answer.
//     Checked FIRST and independently of the claim's presence: if the IdP told us
//     the real list is elsewhere, we do not get to pretend the token holds it.
//   - the claim is present but is not a JSON array (null, a string, an object).
//     We cannot read a list out of it, so we have not read the list.
//
// Readable (present == true) is only the one case: the claim is there and it is
// an array. An EMPTY array is a genuine, admissible answer -- it means the user
// has no claim-derived memberships, and under the hybrid role model that is an
// ordinary user with local grants only.
func (p *Provider) groups(tok *oidc.IDToken) ([]string, bool, error) {
	raw := map[string]any{}
	if err := tok.Claims(&raw); err != nil {
		return nil, false, fmt.Errorf("%w: decode id_token claims: %w", ErrTokenInvalid, err)
	}

	if isDistributedClaim(raw, p.groupsClaim) {
		return nil, false, nil
	}

	value, ok := raw[p.groupsClaim]
	if !ok {
		return nil, false, nil
	}

	list, ok := value.([]any)
	if !ok {
		return nil, false, nil
	}

	groups := make([]string, 0, len(list))

	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			groups = append(groups, s)
		}
	}

	return groups, true, nil
}

// isDistributedClaim reports whether the token says the named claim's real value
// lives somewhere else: `_claim_names` maps a claim name to a key in
// `_claim_sources`, which carries the endpoint (and possibly an access token)
// to fetch it from.
//
// We do not follow the source -- that is a Graph round trip with its own
// credential, and it is not what this milestone buys. We only need to know that
// the token is NOT the authority on this claim, which is precisely the fact a
// naive `raw["groups"]` lookup throws away.
//
// `_claim_sources` is not required to reach this verdict: a `_claim_names` entry
// alone already says the token is not carrying the value. A malformed overage
// (names without sources) is, if anything, more reason to refuse.
func isDistributedClaim(raw map[string]any, claim string) bool {
	names, ok := raw[claimNamesClaim].(map[string]any)
	if !ok {
		return false
	}

	source, ok := names[claim]
	if !ok {
		return false
	}

	// The value is the key into _claim_sources. Anything non-empty there means the
	// IdP has redirected us; an empty string is not a usable redirect, and we do
	// not want a stray `"_claim_names": {"groups": ""}` to lock every login out.
	ref, ok := source.(string)

	return ok && ref != ""
}

func displayName(c idTokenClaims) string {
	for _, candidate := range []string{c.Name, c.PreferredUsername, c.Email} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}

	return ""
}

// randomToken returns 256 bits of CSPRNG output, base64url-encoded. crypto/rand,
// never math/rand: a predictable state or nonce defeats the thing it exists for.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// bearerToken extracts a Bearer credential from the Authorization header.
// The scheme match is case-insensitive per RFC 7235.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)

	return token, token != ""
}
