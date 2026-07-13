package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
)

// ---------------------------------------------------------------------------
// A fake IdP + a fake Bakery, wired together exactly as the real pair are: the
// server publishes the IdP's endpoints at /auth/config, and the CLI configures
// itself from that and talks to the IdP directly.
// ---------------------------------------------------------------------------

type fakeIDP struct {
	t *testing.T

	server *httptest.Server

	mu sync.Mutex

	// tokenReplies is consumed one per poll. Each is a status + body.
	tokenReplies []idpReply

	// tokenRequests records every form the CLI POSTed to the token endpoint.
	tokenRequests []map[string]string

	interval int64
}

type idpReply struct {
	status int
	body   string
}

func newFakeIDP(t *testing.T, replies []idpReply, interval int64) *fakeIDP {
	t.Helper()

	idp := &fakeIDP{
		t: t, server: nil, mu: sync.Mutex{},
		tokenReplies: replies, tokenRequests: nil, interval: interval,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"device_code":               "dev-code-1",
			"user_code":                 "WDJB-MJHT",
			"verification_uri":          idp.server.URL + "/activate",
			"verification_uri_complete": idp.server.URL + "/activate?user_code=WDJB-MJHT",
			"expires_in":                600,
			"interval":                  idp.interval,
		})
	})

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse the token form: %v", err)
		}

		form := make(map[string]string, len(r.PostForm))
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}

		idp.mu.Lock()
		idp.tokenRequests = append(idp.tokenRequests, form)

		var reply idpReply

		if len(idp.tokenReplies) > 0 {
			reply = idp.tokenReplies[0]
			idp.tokenReplies = idp.tokenReplies[1:]
		} else {
			reply = idpReply{status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`}
		}

		idp.mu.Unlock()

		// A Basic auth header here would mean AuthStyle autodetect is still on and
		// every failing poll is being sent twice. This is a PUBLIC client: the id
		// goes in the body, and there is no secret.
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("the token request carried HTTP Basic auth; the CLI is a public client " +
				"and AuthStyle must be pinned to AuthStyleInParams")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

func (i *fakeIDP) forms() []map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()

	out := make([]map[string]string, len(i.tokenRequests))
	copy(out, i.tokenRequests)

	return out
}

// loginHarness is a Bakery whose /auth/config points at the fake IdP, plus a
// /me that answers once the CLI has a token.
type loginHarness struct {
	idp    *fakeIDP
	api    *fakeAPI
	client *Client
	store  *TokenStore

	// slept records every interval the poll loop waited, in order. This is the
	// assertion that matters: it is the difference between honouring slow_down and
	// hammering the IdP's token endpoint until it rate-limits us.
	slept []time.Duration
}

func newLoginHarness(t *testing.T, replies []idpReply, interval int64) *loginHarness {
	t.Helper()

	idp := newFakeIDP(t, replies, interval)
	f := newFakeAPI(t)

	h := &loginHarness{idp: idp, api: f, client: nil, store: nil, slept: nil}

	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		switch strings.TrimPrefix(r.URL.Path, api.Prefix) {
		case "/auth/config":
			writeJSONResp(w, http.StatusOK, auth.AuthConfig{
				Issuer:                      idp.server.URL,
				ClientID:                    "bakery-cli",
				Scopes:                      []string{"openid", "profile", "email", "groups", "offline_access"},
				AuthorizationEndpoint:       idp.server.URL + "/authorize",
				TokenEndpoint:               idp.server.URL + "/token",
				DeviceAuthorizationEndpoint: idp.server.URL + "/device",
				OIDCEnabled:                 true,
				DevLoginEnabled:             false,
			})

			return true

		case "/me":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				writeErr(w, http.StatusUnauthorized, api.CodeUnauthorized, "no credential")

				return true
			}

			writeJSONResp(w, http.StatusOK, api.Me{
				UserID: "u1", Email: "dev@bakery.local", DisplayName: "Dev",
				Method: "bearer", SiteRole: "admin", IsSiteAdmin: true,
				Orgs: nil, Projects: nil, APIKey: nil,
			})

			return true
		}

		return false
	}

	store, _ := tempStore(t)

	c, err := NewClient(f.server.URL, store)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// The injected clock. The poll loop's REAL sleeps would make this test take
	// eight seconds to prove a property that is exactly about durations, so record
	// them instead of serving them.
	c.sleep = func(_ context.Context, d time.Duration) error {
		h.slept = append(h.slept, d)

		return nil
	}

	h.client = c
	h.store = store

	return h
}

// TestDeviceGrantBacksOff is the load-bearing test in this file.
//
// The IdP answers authorization_pending, then slow_down, then success. RFC 8628
// §3.5 says a client that is told to slow down MUST add five seconds to its poll
// interval -- and an IdP is entitled to start rejecting a client that does not.
// A device flow that ignored slow_down would still pass an end-to-end "did I get
// a token" test, because the token arrives either way. What it would do is hammer
// the token endpoint. So the assertion is on the SLEEPS, not on the outcome:
//
//	poll 1: wait 1s (the IdP's interval)  -> authorization_pending
//	poll 2: wait 1s                       -> slow_down
//	poll 3: wait 6s (1 + 5)               -> success
func TestDeviceGrantBacksOff(t *testing.T) {
	idToken := fakeJWT(t, time.Now().Add(time.Hour))

	h := newLoginHarness(t, []idpReply{
		{status: http.StatusBadRequest, body: `{"error":"authorization_pending"}`},
		{status: http.StatusBadRequest, body: `{"error":"slow_down"}`},
		{status: http.StatusOK, body: `{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"id_token":      "` + idToken + `",
			"token_type":    "Bearer",
			"expires_in":    3600
		}`},
	}, 1)

	out := new(strings.Builder)

	if err := Login(t.Context(), h.client, out); err != nil {
		t.Fatalf("Login: %v", err)
	}

	want := []time.Duration{1 * time.Second, 1 * time.Second, 6 * time.Second}

	if len(h.slept) != len(want) {
		t.Fatalf("slept %v, want %v", h.slept, want)
	}

	for i, d := range want {
		if h.slept[i] != d {
			t.Errorf("sleep %d = %v, want %v (slept %v)", i+1, h.slept[i], d, h.slept)
		}
	}

	// Three polls, not six: AuthStyle autodetect would have doubled every failing
	// one, because every authorization_pending IS an HTTP 400 and the probe result
	// is cached only on success.
	forms := h.idp.forms()
	if len(forms) != 3 {
		t.Fatalf("made %d token requests, want 3", len(forms))
	}

	for i, f := range forms {
		if f["grant_type"] != deviceGrantType {
			t.Errorf("poll %d grant_type = %q, want %q", i+1, f["grant_type"], deviceGrantType)
		}

		if f["device_code"] != "dev-code-1" {
			t.Errorf("poll %d device_code = %q", i+1, f["device_code"])
		}

		if f["client_id"] != "bakery-cli" {
			t.Errorf("poll %d client_id = %q, want the id from /auth/config", i+1, f["client_id"])
		}
	}

	// The user must be told the code and where to type it.
	printed := out.String()
	for _, want := range []string{"WDJB-MJHT", "/activate", "signed in as dev@bakery.local"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the login output does not mention %q:\n%s", want, printed)
		}
	}
}

// TestLoginCachesTokensAt0600: the credential written at the END of a real login
// must be locked down, not just the one written by a unit test that called Put
// directly.
func TestLoginCachesTokensAt0600(t *testing.T) {
	idToken := fakeJWT(t, time.Now().Add(time.Hour))

	h := newLoginHarness(t, []idpReply{{
		status: http.StatusOK,
		body: `{"access_token":"at-1","refresh_token":"rt-1","id_token":"` + idToken +
			`","token_type":"Bearer","expires_in":3600}`,
	}}, 1)

	if err := Login(t.Context(), h.client, new(strings.Builder)); err != nil {
		t.Fatalf("Login: %v", err)
	}

	fi, err := os.Stat(h.store.Path())
	if err != nil {
		t.Fatalf("stat the token cache: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("token cache mode = %04o, want 0600", got)
	}

	tok, ok := h.store.Get(h.client.Server())
	if !ok {
		t.Fatal("no token was cached")
	}

	if tok.IDToken != idToken {
		t.Error("the cached ID token is not the one the IdP issued")
	}

	if tok.RefreshToken != "rt-1" {
		t.Errorf("refresh token = %q, want rt-1 -- without it the user is bounced to a browser hourly",
			tok.RefreshToken)
	}

	// The expiry must come from the ID TOKEN's exp -- that is the credential we
	// present -- not from the access token's expires_in.
	if tok.Expiry.IsZero() {
		t.Error("no expiry was recorded, so every request will pay for a refresh")
	}
}

// TestDeviceGrantOutcomes covers the terminal errors. Each has a distinct next
// action, so each gets a distinct sentence.
func TestDeviceGrantOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		replies []idpReply
		wantErr error
		wantMsg string
	}{
		{
			name: "denied",
			replies: []idpReply{
				{status: http.StatusBadRequest, body: `{"error":"access_denied"}`},
			},
			wantErr: errDeviceDenied,
			wantMsg: "",
		},
		{
			name: "expired",
			replies: []idpReply{
				{status: http.StatusBadRequest, body: `{"error":"expired_token"}`},
			},
			wantErr: errDeviceExpired,
			wantMsg: "",
		},
		{
			name: "an unexpected oauth error is surfaced, not swallowed",
			replies: []idpReply{
				{
					status: http.StatusBadRequest,
					body:   `{"error":"invalid_client","error_description":"unknown client"}`,
				},
			},
			wantErr: nil,
			wantMsg: "invalid_client: unknown client",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoginHarness(t, tc.replies, 1)

			err := Login(t.Context(), h.client, new(strings.Builder))
			if err == nil {
				t.Fatal("Login succeeded, want an error")
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}

			if _, ok := h.store.Get(h.client.Server()); ok {
				t.Error("a failed login cached a token")
			}
		})
	}
}

// TestLoginRefusesAServerWithNoDeviceEndpoint. device_authorization_endpoint is
// RFC 8414 metadata, not core OIDC discovery, so an IdP may legitimately omit it.
// The failure must name the knob that fixes it, not report an empty URL.
func TestLoginRefusesAServerWithNoDeviceEndpoint(t *testing.T) {
	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.TrimPrefix(r.URL.Path, api.Prefix) != "/auth/config" {
			return false
		}

		writeJSONResp(w, http.StatusOK, auth.AuthConfig{
			Issuer: "https://idp.example.com", ClientID: "bakery-cli",
			Scopes: []string{"openid"}, AuthorizationEndpoint: "https://idp.example.com/authorize",
			TokenEndpoint: "https://idp.example.com/token", DeviceAuthorizationEndpoint: "",
			OIDCEnabled: true, DevLoginEnabled: false,
		})

		return true
	}

	store, _ := tempStore(t)

	c, err := NewClient(f.server.URL, store)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	err = Login(t.Context(), c, new(strings.Builder))
	if err == nil {
		t.Fatal("Login succeeded against a provider with no device endpoint")
	}

	if !strings.Contains(err.Error(), "OIDC_DEVICE_AUTH_URL") {
		t.Errorf("err = %q, want it to name the override", err.Error())
	}
}

// TestStaleTokenIsRefreshedTransparently is the offline_access requirement: an
// hour after logging in, `bakery org list` must return an org list, not a browser.
func TestStaleTokenIsRefreshedTransparently(t *testing.T) {
	fresh := fakeJWT(t, time.Now().Add(time.Hour))

	h := newLoginHarness(t, []idpReply{{
		status: http.StatusOK,
		body: `{"access_token":"at-2","id_token":"` + fresh +
			`","token_type":"Bearer","expires_in":3600}`,
	}}, 1)

	// Also answer /orgs, so we can drive an ordinary command through the refresh.
	inner := h.api.handler
	h.api.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.TrimPrefix(r.URL.Path, api.Prefix) == "/orgs" {
			if r.Header.Get("Authorization") != "Bearer "+fresh {
				writeErr(w, http.StatusUnauthorized, api.CodeUnauthorized, "stale token")

				return true
			}

			writeJSONResp(w, http.StatusOK, api.ListResponse[api.Org]{Items: []api.Org{}})

			return true
		}

		return inner(w, r)
	}

	// An expired ID token, with a refresh token beside it.
	expired := fakeJWT(t, time.Now().Add(-time.Hour))

	if err := h.store.Put(h.client.Server(), Token{
		IDToken: expired, AccessToken: "at-1", RefreshToken: "rt-1",
		Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := h.client.ListOrgs(t.Context()); err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}

	forms := h.idp.forms()
	if len(forms) != 1 {
		t.Fatalf("made %d token requests, want 1 refresh", len(forms))
	}

	if forms[0]["grant_type"] != "refresh_token" || forms[0]["refresh_token"] != "rt-1" {
		t.Errorf("refresh form = %v", forms[0])
	}

	tok, ok := h.store.Get(h.client.Server())
	if !ok {
		t.Fatal("the token cache was emptied")
	}

	if tok.IDToken != fresh {
		t.Error("the refreshed ID token was not written back to the cache")
	}

	// The IdP rotated no new refresh token, which RFC 6749 permits. Dropping the
	// old one would sign the user out at the next refresh, an hour later, for no
	// reason.
	if tok.RefreshToken != "rt-1" {
		t.Errorf("refresh token = %q, want the original rt-1 to be retained", tok.RefreshToken)
	}
}

// TestARevokedRefreshTokenSaysRunBakeryLogin: a refresh that the IdP refuses is
// not a transient fault. There is no retry that helps, and the user needs the
// next command to type -- not an OAuth error code.
func TestARevokedRefreshTokenSaysRunBakeryLogin(t *testing.T) {
	h := newLoginHarness(t, []idpReply{{
		status: http.StatusBadRequest,
		body:   `{"error":"invalid_grant","error_description":"token revoked"}`,
	}}, 1)

	expired := fakeJWT(t, time.Now().Add(-time.Hour))

	if err := h.store.Put(h.client.Server(), Token{
		IDToken: expired, RefreshToken: "rt-revoked", Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err := h.client.ListOrgs(t.Context())
	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("err = %v, want ErrNeedsLogin", err)
	}
}

// TestLogout clears the cache and says so.
func TestLogout(t *testing.T) {
	f := newFakeAPI(t)
	store, _ := tempStore(t)

	c, err := NewClient(f.server.URL, store)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := store.Put(c.Server(), Token{IDToken: "id"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	out := new(strings.Builder)
	if err := Logout(c, out); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, ok := store.Get(c.Server()); ok {
		t.Error("the token survived logout")
	}

	if !strings.Contains(out.String(), "signed out") {
		t.Errorf("logout said %q", out.String())
	}

	// Logging out twice is not an error.
	out.Reset()

	if err := Logout(c, out); err != nil {
		t.Fatalf("second Logout: %v", err)
	}

	if !strings.Contains(out.String(), "not signed in") {
		t.Errorf("the second logout said %q", out.String())
	}
}
