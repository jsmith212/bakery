package ociconf

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPingsCarryTheBearerChallengeOnA200 is the single highest-stakes assertion in this
// package, and it is an assertion about a 200, not about a 401.
//
// containers/image (podman, skopeo, CRI-O) harvests authentication challenges from the
// `GET /v2/` PING RESPONSE AND NOWHERE ELSE -- its own comment says it uses "the
// challenges from the /v2/ ping response and not the one from the destination URL" --
// and detectPropertiesHelper harvests them from a 200 as readily as from a 401. So a
// ping that answers a bare 200 with no challenge header tells every one of those clients
// that this registry needs no authentication: they never attempt auth at all, every
// subsequent request 401s, and the client falls back to the real registry in silence.
// The cache's hit rate is 0%, every build is green, and nothing anywhere reports it.
//
// Docker Engine's PingV2Registry reaches the MIRROR-PATH ping instead of the host root,
// which is why both are asserted.
func TestPingsCarryTheBearerChallengeOnA200(t *testing.T) {
	e := newEnv(t)

	pings := []struct {
		name      string
		url       string
		wantRealm string
	}{
		{
			// The bare host root, OUTSIDE any tenant prefix. podman/skopeo/CRI-O ping
			// exactly this and hard-error on anything but 200 or 401, so without it they
			// cannot use Bakery at all.
			name:      "containers/image host-root ping",
			url:       e.httpBase + "/v2/",
			wantRealm: e.httpBase + "/v2/token",
		},
		{
			name:      "docker engine / containerd mirror-path ping",
			url:       e.httpBase + e.containerdPath(projMain) + "/",
			wantRealm: e.httpBase + e.containerdPath(projMain) + "/token",
		},
		{
			name:      "buildkit / podman tenant ping",
			url:       e.family2(projMain, ""),
			wantRealm: e.httpBase + "/cache/" + envOrg + "/" + projMain + "/docker/v2/token",
		},
	}

	for _, p := range pings {
		t.Run(p.name, func(t *testing.T) {
			res := request(t, http.MethodGet, p.url, nil)

			if res.status != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 (containers/image hard-errors on anything but 200/401)",
					p.url, res.status)
			}

			challenge := res.header.Get("WWW-Authenticate")
			if challenge == "" {
				t.Fatalf("GET %s answered 200 with NO WWW-Authenticate header. Every containers/image "+
					"client harvests challenges from the ping and nowhere else, so it will now "+
					"conclude this registry needs no auth and silently stop using the mirror.", p.url)
			}

			if !strings.HasPrefix(challenge, "Bearer ") {
				t.Errorf("challenge = %q, want a Bearer challenge. BuildKit installs its Basic handler "+
					"only when it holds BOTH a username and a secret, so a bare Basic challenge "+
					"makes a credential-less BuildKit skip the mirror entirely.", challenge)
			}

			realm := challengeParam(t, challenge, "realm")

			// An ABSOLUTE URL. A relative realm, or one derived from r.TLS behind a
			// TLS-terminating ingress, works in every direct-connection test and breaks in
			// every real deployment -- which is why oci.Config.ExternalURL exists.
			u, err := url.Parse(realm)
			if err != nil || !u.IsAbs() {
				t.Fatalf("realm = %q, want an absolute URL", realm)
			}

			if realm != p.wantRealm {
				t.Errorf("realm = %q, want %q", realm, p.wantRealm)
			}

			if svc := challengeParam(t, challenge, "service"); svc != "bakery" {
				t.Errorf("service = %q, want \"bakery\"", svc)
			}

			// The realm must actually answer. go-containerregistry validates it before
			// dialling and every client follows it blindly; a realm that 404s is an auth
			// loop nobody sees.
			if tok := request(t, http.MethodGet, realm, nil); tok.status != http.StatusOK {
				t.Errorf("GET %s (the advertised realm) = %d, want 200", realm, tok.status)
			}
		})
	}
}

// TestTokenEndpointAnswersEveryClientShape covers the three ways the four clients ask
// for a token, and asserts BOTH response keys every time.
//
//	GET  + Basic                       BuildKit, Docker Engine, containers/image
//	POST + grant_type=password         containerd with a username and a secret
//	POST + grant_type=refresh_token    containerd with an identitytoken (NO username)
//
// THE POST HALF IS NOT OPTIONAL. containerd sends the OAuth2 form POST FIRST whenever it
// holds a secret and falls back to GET only on 405/404/401/400 -- and the 405 branch is
// additionally gated on a non-empty username. A Bakery credential has no username half,
// so against a GET-only endpoint (where Go's ServeMux auto-405s the POST) containerd
// never reaches the fallback: it hard-fails, the mirror is skipped, and the pull goes to
// the real registry in silence.
//
// AND BOTH KEYS ARE NOT REDUNDANCY. The classic endpoint answers `token`; the OAuth2
// endpoint answers `access_token`; containerd's POST path reads only the second and
// BuildKit's GET path reads only the first.
func TestTokenEndpointAnswersEveryClientShape(t *testing.T) {
	e := newEnv(t)
	key := e.key(projMain)

	for _, endpoint := range []string{
		e.httpBase + "/v2/token",
		e.httpBase + e.containerdPath(projMain) + "/token",
	} {
		t.Run(endpoint, func(t *testing.T) {
			basic := base64.StdEncoding.EncodeToString([]byte("bakery:" + key))

			got := decodeToken(t, request(t, http.MethodGet, endpoint,
				map[string]string{"Authorization": "Basic " + basic}))
			assertTokenPair(t, "GET+Basic", got, key)

			// The token in the USERNAME field instead. A Bakery credential is one opaque
			// token, not an id:secret pair, so a client that puts the whole thing in either
			// Basic field must authenticate.
			userOnly := base64.StdEncoding.EncodeToString([]byte(key + ":"))

			got = decodeToken(t, request(t, http.MethodGet, endpoint,
				map[string]string{"Authorization": "Basic " + userOnly}))
			assertTokenPair(t, "GET+Basic (token in the username field)", got, key)

			// containerd, with a username: grant_type=password.
			got = decodeToken(t, postForm(t, endpoint, url.Values{
				"grant_type": {"password"},
				"username":   {"bakery"},
				"password":   {key},
				"service":    {"bakery"},
				"client_id":  {"containerd-client"},
			}))
			assertTokenPair(t, "POST grant_type=password", got, key)

			// containerd, with an identitytoken: grant_type=refresh_token and NO username
			// anywhere in the exchange. This is the shape a Bakery credential actually
			// takes in a hosts.toml.
			got = decodeToken(t, postForm(t, endpoint, url.Values{
				"grant_type":    {"refresh_token"},
				"refresh_token": {key},
				"service":       {"bakery"},
				"client_id":     {"containerd-client"},
			}))
			assertTokenPair(t, "POST grant_type=refresh_token", got, key)

			// No credential at all. Every registry client expects the literal "anonymous"
			// from a public registry's token endpoint, and replays it as a Bearer header on
			// every request thereafter -- which is why the backend short-circuits that
			// sentinel by name rather than sending it to the OIDC verifier.
			got = decodeToken(t, request(t, http.MethodGet, endpoint, nil))
			assertTokenPair(t, "GET anonymous", got, "anonymous")
		})
	}
}

// tokenBody is the token endpoint's response, read for BOTH spellings.
type tokenBody struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func decodeToken(t *testing.T, res httpResult) tokenBody {
	t.Helper()

	if res.status != http.StatusOK {
		t.Fatalf("token endpoint = %d, body %s", res.status, res.body)
	}

	var out tokenBody
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatalf("decode the token response: %v (body %s)", err, res.body)
	}

	return out
}

func assertTokenPair(t *testing.T, shape string, got tokenBody, want string) {
	t.Helper()

	if got.Token != want {
		t.Errorf("%s: token = %q, want %q (BuildKit and containers/image read this key)",
			shape, got.Token, want)
	}

	if got.AccessToken != want {
		t.Errorf("%s: access_token = %q, want %q (containerd's OAuth2 POST reads THIS key, and "+
			"only this key)", shape, got.AccessToken, want)
	}
}

func postForm(t *testing.T, endpoint string, form url.Values) httpResult {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build POST %s: %v", endpoint, err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST %s: %v", endpoint, err)
	}

	return httpResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// challengeParam pulls one key="value" parameter out of a WWW-Authenticate header.
func challengeParam(t *testing.T, challenge, key string) string {
	t.Helper()

	for _, part := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k != key {
			continue
		}

		return strings.Trim(v, `"`)
	}

	t.Fatalf("challenge %q has no %s parameter", challenge, key)

	return ""
}
