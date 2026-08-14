package oci

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/cache"
)

// tokenTTL is what the token response ADVERTISES, not a lifetime we enforce. The
// "token" we hand back is the caller's own bkry_ API key, whose real lifetime is the
// key's -- revocable in the database, checked on every request. Advertising a finite
// expiry only makes well-behaved clients re-run the (free, local) token dance
// periodically instead of caching one string forever.
const tokenTTL = 300

// tokenResponse is the Docker token-endpoint body.
//
// IT CARRIES THE VALUE UNDER BOTH KEYS, AND THAT IS NOT REDUNDANCY. The two auth
// flows spell the field differently and each client reads only its own spelling:
// the classic token endpoint (GET) answers `token`, while the OAuth2 endpoint (POST)
// answers `access_token`. containerd's authorizer POSTs first when it has a secret and
// reads `access_token`; BuildKit GETs first and reads `token`; Docker Engine and
// containers/image read `token`. Emitting both in both responses makes the endpoint
// correct for every client with no negotiation and no branching.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// serveToken answers the Docker Bearer token endpoint, on GET AND POST.
//
// POST IS NOT OPTIONAL, and getting this wrong is a silent client break rather than a
// visible error. containerd's authorizer sends an OAuth2 form POST FIRST whenever it
// holds a secret, and falls back to GET only on a 405/404/401/400 response -- and the
// 405 branch is additionally gated on a non-empty username. A hosts.toml credential
// configured as an `identitytoken` has NO username, so against a GET-only endpoint
// (where Go's ServeMux auto-405s the POST) containerd never reaches the fallback: it
// hard-fails, the mirror is skipped, and the pull silently goes to the real registry.
//
// THE TOKEN WE ISSUE IS THE CALLER'S OWN API KEY, echoed back. There is no signing
// key, no JWT, no expiry to track and no new secret material in the system. What makes
// that sound rather than lazy: the returned string is validated on every subsequent
// request by the same constant-time, zero-join index probe every other Bakery
// credential goes through, so revocation is immediate and correct for free. A signed
// token would need its own revocation story, which is strictly more machinery for
// strictly less correctness.
//
// A caller with no credential gets the literal "anonymous", which is what every
// registry client expects from a public registry's token endpoint. It is the sentinel
// credentialToken short-circuits on the way back in.
func (b *Backend) serveToken(w http.ResponseWriter, r *http.Request) {
	token := anonymousToken

	if t, ok := credentialToken(r); ok {
		token = t
	} else if t, ok := formToken(r); ok {
		token = t
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A token response is per-credential and must never be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")

	_ = json.NewEncoder(w).Encode(tokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   tokenTTL,
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
	})
}

// formToken reads the credential out of an OAuth2 POST body.
//
// containerd sends one of two grants and the credential lands in a different field in
// each: `grant_type=password` puts it in `password`, while an identitytoken credential
// sends `grant_type=refresh_token` with the secret in `refresh_token` and NO username
// at all. Both are checked, and both are shape-gated by isBakeryToken so a forwarded
// Docker Hub secret in a form body is discarded exactly as one in a header is.
//
// ParseForm is bounded by Go's default 10 MB form limit and only ever runs on the
// token route, which carries no body worth reading in the first place.
func formToken(r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		return "", false
	}

	if err := r.ParseForm(); err != nil {
		return "", false
	}

	for _, field := range []string{"password", "refresh_token", "access_token"} {
		if v := r.PostForm.Get(field); isBakeryToken(v) {
			return v, true
		}
	}

	return "", false
}

// challenge writes the WWW-Authenticate Bearer challenge.
//
// IT IS ALSO WRITTEN ON A 200 PING, and that is the single most consequential line in
// this package. containers/image (podman, skopeo, CRI-O) harvests authentication
// challenges from the `GET /v2/` PING RESPONSE AND NOWHERE ELSE -- its own comment
// says it uses "the challenges from the /v2/ ping response and not the one from the
// destination URL" -- and it harvests them from a 200 as readily as from a 401. A ping
// that answers a bare 200 with no challenge therefore tells every one of those clients
// that this registry needs no authentication: they never attempt auth, every
// subsequent request 401s, and the client silently falls back to the real registry.
// The cache's hit rate is 0%, every build is green, and nothing anywhere reports a
// problem.
//
// The realm MUST be an absolute URL -- see Config.ExternalURL for why deriving it from
// the request is production-only-wrong.
func (b *Backend) challenge(w http.ResponseWriter, r *http.Request, route cache.Route) {
	w.Header().Set("WWW-Authenticate",
		`Bearer realm="`+b.tokenURL(r, route)+`",service="bakery"`)
}

// tokenURL builds the absolute realm URL.
//
// A TENANT ping advertises that tenant's token endpoint and the global ping advertises
// the global one; both endpoints accept the same credentials and answer identically,
// so the distinction is cosmetic -- but a client that harvested a challenge from the
// tenant ping and later has to re-auth against a tenant path stays inside one path
// prefix, which is what an operator reading an access log expects.
func (b *Backend) tokenURL(r *http.Request, route cache.Route) string {
	base := strings.TrimSuffix(b.cfg.ExternalURL, "/")
	if base == "" {
		// Direct-connection fallback -- and it FAILS CLOSED. Behind a TLS-terminating
		// ingress (the normal deployment) r.TLS is nil, so deriving "http" here would
		// advertise an http:// realm and every client would then POST the bkry_ token
		// over cleartext -- silently, on every token dance, with nothing logging it.
		// So the fallback assumes https unless the host is plainly local: a wrong
		// https:// realm is an unreachable realm (a loud, debuggable client error), a
		// wrong http:// realm is a credential disclosure. Config.ExternalURL exists so
		// operators never rely on this guess; warn once when they do.
		scheme := "https"
		if r.TLS == nil && isLoopbackHost(r.Host) {
			scheme = "http"
		}

		b.warnRealmOnce.Do(func() {
			b.deps.Logger.Warn("oci: EXTERNAL_URL is unset; deriving the token realm from the request",
				slog.String("derived", scheme+"://"+r.Host),
				slog.String("fix", "set --external-url / EXTERNAL_URL to the public base URL"))
		})

		base = scheme + "://" + r.Host
	}

	if route.Org == "" {
		return base + "/v2/token"
	}

	return base + "/cache/" + route.Org + "/" + route.Project + "/docker/v2/token"
}

// isLoopbackHost reports whether the request host is plainly local -- the one case
// where an http:// realm fallback is not a credential disclosure.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
