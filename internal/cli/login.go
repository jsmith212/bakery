package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/jsmith212/bakery/internal/auth"
)

// The RFC 8628 §3.5 poll errors we must handle rather than surface.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errAccessDenied         = "access_denied"
	errExpiredToken         = "expired_token"
)

// deviceGrantType is the device-code grant's URN (RFC 8628 §3.4).
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// defaultInterval is the poll interval when the IdP omits one (RFC 8628 §3.5).
const defaultInterval = 5 * time.Second

// slowDownIncrement is what a `slow_down` adds to the interval. RFC 8628 §3.5
// says the client MUST increase by 5 seconds; it is not a suggestion, and an IdP
// is entitled to start rejecting a client that ignores it.
const slowDownIncrement = 5 * time.Second

// oauthError is an OAuth2 error response from the token endpoint.
//
// x/oauth2 has *RetrieveError, but constructing one requires the *http.Response
// it came from, and we would still be matching on the same string codes. A local
// type keeps the poll loop's switch honest and keeps errors.As at the boundary.
type oauthError struct {
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}

	return e.Code
}

// Errors the device flow surfaces to the user, each with the next action in it.
var (
	errDeviceDenied  = errors.New("the sign-in request was denied")
	errDeviceExpired = errors.New("the code expired before it was approved: run bakery login again")
)

// Login runs the OIDC device grant (RFC 8628) and caches the resulting tokens.
//
// The device grant, and not a local callback listener: a `bakery login` is
// routinely run over ssh on a build host, where there is no browser to redirect
// to and no way to reach a loopback port on the machine the user is sitting at.
// The device grant is the flow designed for exactly that, and it means the CLI
// never handles the user's password or sees the authorization code.
func Login(ctx context.Context, c *Client, out io.Writer) error {
	cfg, err := c.AuthConfig(ctx)
	if err != nil {
		return err
	}

	if !cfg.OIDCEnabled {
		return errors.New("this server has no identity provider configured, so there is nothing to sign in to")
	}

	if cfg.DeviceAuthorizationEndpoint == "" {
		// device_authorization_endpoint is RFC 8414 metadata, not core OIDC
		// discovery, so a conformant IdP may legitimately omit it. Say which knob
		// fixes it rather than reporting an empty URL.
		return errors.New(
			"the identity provider advertises no device authorization endpoint; " +
				"the server must set OIDC_DEVICE_AUTH_URL")
	}

	oc := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: "", // The CLI is a PUBLIC client. It ships no secret, by design.
		Scopes:       cfg.Scopes,
		RedirectURL:  "",
		Endpoint: oauth2.Endpoint{
			AuthURL:       cfg.AuthorizationEndpoint,
			TokenURL:      cfg.TokenEndpoint,
			DeviceAuthURL: cfg.DeviceAuthorizationEndpoint,
			// AuthStyleAutoDetect probes AuthStyleInHeader first and RETRIES on any
			// error -- and every authorization_pending poll IS an error (HTTP 400).
			// The probe result is cached only on success, so autodetect never
			// settles and the whole device flow is sent twice. Pin it.
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}

	// x/oauth2 reads its HTTP client off the context.
	octx := context.WithValue(ctx, oauth2.HTTPClient, c.http)

	da, err := oc.DeviceAuth(octx)
	if err != nil {
		return fmt.Errorf("start the device authorization: %w", err)
	}

	printDeviceCode(out, da)

	tok, err := c.pollDevice(ctx, cfg, da)
	if err != nil {
		return err
	}

	if err := c.tokens.Put(c.server, tok); err != nil {
		return err
	}

	me, err := c.Me(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "signed in as %s (%s)\n", me.Email, me.SiteRole)

	return nil
}

// printDeviceCode is the user's half of the flow.
//
// The user code goes on its own line, unadorned, because the next thing that
// happens is that a human retypes it into a browser on another machine.
func printDeviceCode(out io.Writer, da *oauth2.DeviceAuthResponse) {
	fmt.Fprintf(out, "\nopen this page in a browser:\n\n  %s\n\nand enter the code:\n\n  %s\n\n",
		da.VerificationURI, da.UserCode)

	if da.VerificationURIComplete != "" {
		fmt.Fprintf(out, "or open this, which carries the code for you:\n\n  %s\n\n",
			da.VerificationURIComplete)
	}

	fmt.Fprint(out, "waiting for approval\n")
}

// pollDevice polls the token endpoint until the user approves, denies, or the
// code expires.
//
// This loop is written out rather than delegated to oauth2.DeviceAccessToken for
// one reason: DeviceAccessToken owns its own clock, so the backoff -- the part of
// this that an IdP will rate-limit us for getting wrong -- could only be tested
// by sitting through it in real time. Here the sleep is injected, so the test
// asserts the exact interval sequence in microseconds. The semantics are the
// RFC's and match x/oauth2's: wait BEFORE the first poll, keep polling on
// authorization_pending, add five seconds on slow_down, stop on anything else.
func (c *Client) pollDevice(
	ctx context.Context, cfg auth.AuthConfig, da *oauth2.DeviceAuthResponse,
) (Token, error) {
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}

	// DeviceAuthResponse.Expiry is an ABSOLUTE time despite its `expires_in` tag:
	// x/oauth2's UnmarshalJSON converts the seconds to a wall clock on arrival.
	deadline := da.Expiry

	for {
		if !deadline.IsZero() && !c.now().Before(deadline) {
			return Token{}, errDeviceExpired
		}

		if err := c.sleep(ctx, interval); err != nil {
			return Token{}, fmt.Errorf("wait for approval: %w", err)
		}

		tok, err := c.tokenExchange(ctx, cfg.TokenEndpoint, url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {da.DeviceCode},
			"client_id":   {cfg.ClientID},
		})

		var oe *oauthError

		if errors.As(err, &oe) {
			switch oe.Code {
			case errAuthorizationPending:
				continue

			case errSlowDown:
				interval += slowDownIncrement

				continue

			case errAccessDenied:
				return Token{}, errDeviceDenied

			case errExpiredToken:
				return Token{}, errDeviceExpired
			}
		}

		if err != nil {
			return Token{}, err
		}

		return tok, nil
	}
}

// tokenResponse is the token endpoint's success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// errorResponse is the token endpoint's failure body (RFC 6749 §5.2).
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// tokenExchange makes ONE request to the token endpoint.
//
// It is the single-shot primitive x/oauth2 does not export: DeviceAccessToken
// only offers the whole blocking loop, and TokenSource only offers refresh. Both
// the poll and the refresh need one request, so both come through here.
func (c *Client) tokenExchange(ctx context.Context, endpoint string, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("build the token request: %w", err)
	}

	// A public client: the client id rides in the body, and there is no secret to
	// put in an Authorization header even if we wanted to.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("call the token endpoint: %w", err)
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Token{}, fmt.Errorf("read the token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var body errorResponse

		if err := json.Unmarshal(raw, &body); err != nil || body.Error == "" {
			return Token{}, fmt.Errorf("the token endpoint returned %s",
				strings.ToLower(http.StatusText(resp.StatusCode)))
		}

		return Token{}, &oauthError{Code: body.Error, Description: body.ErrorDescription}
	}

	var body tokenResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return Token{}, fmt.Errorf("parse the token response: %w", err)
	}

	// No id_token means the provider is an OAuth2 provider and not an OIDC one.
	// The ID token is the ONLY credential Bakery accepts from the CLI -- the access
	// token identifies nobody -- so this is fatal rather than degraded.
	if body.IDToken == "" {
		return Token{}, errors.New("the identity provider returned no id_token")
	}

	expiry, ok := idTokenExpiry(body.IDToken)
	if !ok {
		// Fall back to the access token's lifetime. It is the wrong clock (we
		// present the ID token, not this one), but it is bounded, and a too-early
		// refresh is harmless where a never-refresh is a hard 401.
		expiry = c.now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}

	return Token{
		IDToken:      body.IDToken,
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		Expiry:       expiry,
	}, nil
}

// Logout clears this server's cached tokens.
//
// It is deliberately local-only. Ending the IdP session is the IdP's business,
// and a CLI that silently signed you out of your browser SSO because you cleaned
// up a token cache would be a surprise nobody asked for.
func Logout(c *Client, out io.Writer) error {
	had, err := c.tokens.Delete(c.server)
	if err != nil {
		return err
	}

	if !had {
		fmt.Fprintf(out, "not signed in to %s\n", c.server)

		return nil
	}

	fmt.Fprintf(out, "signed out of %s; cleared %s\n", c.server, c.tokens.Path())

	return nil
}
