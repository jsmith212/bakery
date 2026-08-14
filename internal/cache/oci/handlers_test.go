package oci

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The two route families, as a client would spell them. Every behavioural test runs
// against both: they are two mounts into one handler set, and a divergence between
// them is a silent half-broken proxy (containerd works, BuildKit does not).
const (
	tenantPrefix = "/cache/acme/widget/docker/v2/"
	buildkitPfx  = "/v2/acme/widget/"
)

func bothFamilies() []struct{ name, prefix string } {
	return []struct{ name, prefix string }{
		{name: "containerd+docker-engine", prefix: tenantPrefix},
		{name: "buildkit+podman", prefix: buildkitPfx},
	}
}

// TestPingCarriesChallengeOnA200 is the highest-stakes assertion in this package.
//
// containers/image (podman, skopeo, CRI-O) harvests authentication challenges from the
// /v2/ PING RESPONSE AND NOWHERE ELSE, and it harvests them from a 200 as readily as
// from a 401. A ping that answers a bare 200 with no WWW-Authenticate therefore tells
// every one of those clients that this registry needs no authentication: they never
// attempt auth, every later request 401s, the client silently falls back to the real
// registry, the hit rate is 0%, and every build is green. There is no client-side
// signal that anything is wrong.
func TestPingCarriesChallengeOnA200(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	tests := []struct {
		name      string
		target    string
		wantRealm string
	}{
		{
			// The tenant-less host-root ping. containers/image pings the HOST ROOT, not
			// the mirror path, and hard-errors on anything but 200 or 401.
			name: "global", target: "/v2/",
			wantRealm: "https://bakery.example.com/v2/token",
		},
		{
			// Docker Engine's PingV2Registry includes the mirror's path.
			name: "tenant", target: tenantPrefix,
			wantRealm: "https://bakery.example.com/cache/acme/widget/docker/v2/token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := f.do(http.MethodGet, tt.target, nil)

			if w.Code != http.StatusOK {
				t.Fatalf("ping %s = %d, want 200 -- containers/image hard-errors on anything else",
					tt.target, w.Code)
			}

			if got := w.Header().Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
				t.Errorf("Docker-Distribution-API-Version = %q, want registry/2.0", got)
			}

			challenge := w.Header().Get("WWW-Authenticate")
			if challenge == "" {
				t.Fatal("ping answered 200 with NO WWW-Authenticate: every containers/image " +
					"client will now skip authentication forever and silently bypass this cache")
			}

			if !strings.HasPrefix(challenge, "Bearer ") {
				t.Errorf("challenge = %q, want a Bearer challenge -- a bare Basic challenge makes "+
					"credential-less BuildKit skip the mirror entirely", challenge)
			}

			if !strings.Contains(challenge, `realm="`+tt.wantRealm+`"`) {
				t.Errorf("challenge = %q, want realm %q (an ABSOLUTE url)", challenge, tt.wantRealm)
			}

			if !strings.Contains(challenge, `service="bakery"`) {
				t.Errorf("challenge = %q, want a service parameter", challenge)
			}
		})
	}
}

// TestRealmFallsBackToTheRequestHost covers the unset-ExternalURL path. It is correct
// for a direct connection and documented as wrong behind a TLS-terminating proxy,
// which is the entire reason Config.ExternalURL exists.
func TestRealmFallsBackToTheRequestHost(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.backend.cfg.ExternalURL = ""

	w := f.do(http.MethodGet, "/v2/", nil)

	// The fallback FAILS CLOSED: a non-loopback host gets an https realm even though
	// the request arrived over plaintext, because behind a TLS-terminating ingress
	// (r.TLS == nil, the normal deployment) an http:// realm would make every client
	// POST the bkry_ token over cleartext. A wrong https realm is a loud client error;
	// a wrong http realm is a silent credential disclosure.
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, `realm="https://example.com/v2/token"`) {
		t.Errorf("challenge = %q, want an https realm derived from the request host (fail-closed)", got)
	}
}

// TestTokenEndpoint covers both verbs on both families.
//
// POST IS NOT OPTIONAL. containerd's authorizer sends an OAuth2 form POST first
// whenever it holds a secret and falls back to GET only on 405/404/401/400 -- and its
// 405 branch additionally requires a non-empty username, so an `identitytoken`
// credential (which has none) HARD-FAILS against a GET-only endpoint that a mux
// auto-405s. The response field is `access_token` for the POST flow and `token` for
// the GET flow, so both are always emitted.
func TestTokenEndpoint(t *testing.T) {
	t.Parallel()

	const key = "bkry_abcdefghijklmnop"

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("bkry:"+key))

	tests := []struct {
		name      string
		method    string
		target    string
		headers   map[string]string
		body      url.Values
		wantToken string
	}{
		{
			name: "GET anonymous", method: http.MethodGet, target: "/v2/token",
			headers: nil, body: nil, wantToken: anonymousToken,
		},
		{
			name: "GET with a key in the basic password", method: http.MethodGet, target: "/v2/token",
			headers: map[string]string{"Authorization": basic}, body: nil, wantToken: key,
		},
		{
			name: "GET on the tenant family", method: http.MethodGet, target: tenantPrefix + "token",
			headers: map[string]string{"Authorization": basic}, body: nil, wantToken: key,
		},
		{
			// containerd's password grant.
			name: "POST password grant", method: http.MethodPost, target: "/v2/token",
			headers:   nil,
			body:      url.Values{"grant_type": {"password"}, "username": {"bkry"}, "password": {key}},
			wantToken: key,
		},
		{
			// containerd's identitytoken grant: NO username at all.
			name:   "POST refresh_token grant with an empty username",
			method: http.MethodPost, target: "/v2/token", headers: nil,
			body: url.Values{
				"grant_type": {"refresh_token"}, "username": {""}, "refresh_token": {key},
			},
			wantToken: key,
		},
		{
			name: "POST anonymous", method: http.MethodPost, target: tenantPrefix + "token",
			headers: nil, body: url.Values{"grant_type": {"password"}}, wantToken: anonymousToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)

			var w = f.doForm(tt.method, tt.target, tt.headers, tt.body)

			if w.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", tt.method, tt.target, w.Code)
			}

			var got tokenResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode token response: %v (body %q)", err, w.Body.String())
			}

			if got.Token != tt.wantToken {
				t.Errorf("token = %q, want %q", got.Token, tt.wantToken)
			}

			// BOTH keys, always: the GET flow reads `token` and the OAuth2 POST flow
			// reads `access_token`, and each client reads only its own spelling.
			if got.AccessToken != tt.wantToken {
				t.Errorf("access_token = %q, want %q", got.AccessToken, tt.wantToken)
			}
		})
	}
}

// TestAnonymousMissIs404AndNeverFetches is the open-relay gate at the HTTP boundary.
//
// No principal means no upstream, structurally: an anonymous caller is served from
// cache and gets a clean 404 otherwise, so the operator's registry credentials and
// rate limit are never spent on an unauthenticated request. The client falls back to
// the real registry, which is the verified behaviour of all four.
func TestAnonymousMissIs404AndNeverFetches(t *testing.T) {
	t.Parallel()

	for _, fam := range bothFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)

			w := f.do(http.MethodGet, fam.prefix+"library/alpine/manifests/3.20", nil)

			if w.Code != http.StatusNotFound {
				t.Fatalf("anonymous miss = %d, want 404", w.Code)
			}

			assertOCIError(t, w.Body.Bytes(), codeManifestUnknown)

			if resolves, manifests, _, _ := f.upstream.counts(); resolves+manifests != 0 {
				t.Errorf("anonymous miss contacted the upstream (%d resolves, %d manifests) -- "+
					"that is an open relay on the operator's credentials", resolves, manifests)
			}

			if f.upstream.sawNilPrincipal {
				t.Error("a nil principal reached the Fetcher; the gate is not structural")
			}
		})
	}
}

// TestAnonymousHitIsServed: an open mirror serves cache HITS to anonymous callers. The
// 404-on-miss rule is about the UPSTREAM leg, not about reads.
func TestAnonymousHitIsServed(t *testing.T) {
	t.Parallel()

	for _, fam := range bothFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			raw := testIndex(t)
			f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

			w := f.do(http.MethodGet, fam.prefix+"library/alpine/manifests/3.20", nil)

			if w.Code != http.StatusOK {
				t.Fatalf("anonymous hit = %d, want 200 (body %q)", w.Code, w.Body.String())
			}

			if got := w.Body.String(); got != string(raw) {
				t.Error("served manifest bytes differ from the stored bytes")
			}

			if _, manifests, _, _ := f.upstream.counts(); manifests != 0 {
				t.Errorf("a cache HIT contacted the upstream %d times; it must contact nobody", manifests)
			}
		})
	}
}

// TestManifestResponseHeaders pins the three headers containerd's HEAD fast path needs
// TOGETHER: Docker-Content-Digest, a real Content-Length, and the STORED Content-Type
// (which containerd assigns verbatim into the descriptor's MediaType and dispatches
// on). Missing any of them forces a full GET-and-hash, silently doubling the traffic
// the HEAD existed to avoid.
func TestManifestResponseHeaders(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			raw := testIndex(t)
			f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

			w := f.do(method, tenantPrefix+"library/alpine/manifests/3.20", nil)

			if w.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", method, w.Code)
			}

			if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:"+testIndexDigest {
				t.Errorf("Docker-Content-Digest = %q, want sha256:%s", got, testIndexDigest)
			}

			if got := w.Header().Get("Content-Type"); got != testIndexType {
				t.Errorf("Content-Type = %q, want the STORED media type %q", got, testIndexType)
			}

			want := strconv.Itoa(len(raw))
			if got := w.Header().Get("Content-Length"); got != want {
				t.Errorf("Content-Length = %q, want %q", got, want)
			}
		})
	}
}

// TestUnknownNamespaceIs404 is the SSRF gate. ?ns= is attacker-controlled input naming
// a host Bakery will dial; without the allowlist this proxy is an SSRF primitive into
// whatever network it is deployed in.
func TestUnknownNamespaceIs404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.authn.principal = fakePrincipal{canRead: true, canWrite: true}
	f.authn.err = nil

	raw := testIndex(t)
	f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

	target := tenantPrefix + "library/alpine/manifests/3.20?ns=" + url.QueryEscape("vault.internal.corp")

	w := f.do(http.MethodGet, target, map[string]string{"Authorization": "Bearer bkry_key"})

	if w.Code != http.StatusNotFound {
		t.Fatalf("unlisted ?ns= = %d, want 404", w.Code)
	}

	if resolves, manifests, stats, gets := f.upstream.counts(); resolves+manifests+stats+gets != 0 {
		t.Error("an unlisted ?ns= reached the Fetcher; the allowlist is not the gate it claims to be")
	}
}

// TestAllowlistedNamespaceIsServed proves the gate is not simply "deny everything":
// a listed alternate upstream resolves, and the tag row it reads is keyed on that
// upstream rather than on the default.
func TestAllowlistedNamespaceIsServed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	raw := testIndex(t)
	f.seedTag(t, "ghcr.io", "acme/app", "v1", raw)

	w := f.do(http.MethodGet, tenantPrefix+"acme/app/manifests/v1?ns=ghcr.io", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("allowlisted ?ns= = %d, want 200", w.Code)
	}
}

// TestUnconfiguredBackendIs404 -- never a 500, never a 403, and identical to a missing
// image so a scanner cannot enumerate projects.
func TestUnconfiguredBackendIs404(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		found bool
		setup func(f *fixture)
	}{
		{name: "no such backend", found: false, setup: func(*fixture) {}},
		{
			name: "backend disabled", found: true,
			setup: func(f *fixture) { f.resolver.route.Enabled = false },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			f.resolver.found = tt.found
			tt.setup(f)

			w := f.do(http.MethodGet, tenantPrefix+"library/alpine/manifests/3.20", nil)

			if w.Code != http.StatusNotFound {
				t.Fatalf("unconfigured backend = %d, want 404", w.Code)
			}

			assertOCIError(t, w.Body.Bytes(), codeNameUnknown)
		})
	}
}

// TestForeignCredentialsAreAnonymousAndUnlogged is the Docker Engine regression test.
//
// Docker Engine forwards the user's REAL DOCKER HUB CREDENTIALS to its mirror on every
// pull -- it has no per-mirror credential scoping. Two things must hold, and the second
// is the one a naive implementation gets wrong:
//
//  1. An unrecognized credential on a read behaves as ANONYMOUS, never a 401. A strict
//     401 here breaks every docker-login'd engine on earth while looking, in review,
//     exactly like correct security.
//  2. The credential is never validated and never logged. It is shape-gated inside this
//     package, so the Authenticator is never even called -- there is no DB probe, no
//     auth-failure metric, and nothing that could write it to a log.
func TestForeignCredentialsAreAnonymousAndUnlogged(t *testing.T) {
	t.Parallel()

	const hubPAT = "dckr_pat_S3CRET_VALUE_xyz"

	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "docker engine forwards a hub PAT as basic",
			header: "Basic " + base64.StdEncoding.EncodeToString([]byte("hubuser:"+hubPAT)),
		},
		{
			// The sentinel every client replays after an anonymous token dance. Left to
			// fall through it reaches the OIDC verifier on EVERY manifest and blob
			// request.
			name: "the Bearer anonymous sentinel", header: "Bearer " + anonymousToken,
		},
		{
			name: "an unrelated bearer token", header: "Bearer " + hubPAT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			raw := testIndex(t)
			f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

			w := f.do(http.MethodGet, tenantPrefix+"library/alpine/manifests/3.20",
				map[string]string{"Authorization": tt.header})

			if w.Code != http.StatusOK {
				t.Fatalf("= %d, want 200: an unrecognized credential on a read is ANONYMOUS, "+
					"not a rejection", w.Code)
			}

			if seen := f.authn.tokens(); len(seen) != 0 {
				t.Errorf("the authenticator was called with %d token(s); a foreign credential "+
					"must never reach credential validation", len(seen))
			}

			if strings.Contains(f.logs.String(), hubPAT) {
				t.Error("a forwarded credential appeared in the log")
			}

			if strings.Contains(w.Body.String(), hubPAT) {
				t.Error("a forwarded credential was echoed in the response body")
			}
		})
	}
}

// TestReadAuthRequiredAnswers401WithAChallenge. Never 403: a 403 is a
// project-existence oracle and is also the code that makes some clients stop retrying
// instead of falling back.
func TestReadAuthRequiredAnswers401WithAChallenge(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.resolver.route.ReadAuthRequired = true

	raw := testIndex(t)
	f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

	w := f.do(http.MethodGet, tenantPrefix+"library/alpine/manifests/3.20", nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", w.Code)
	}

	challenge := w.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Errorf("challenge = %q, want Bearer", challenge)
	}

	// The httpblob mounts answer `Basic realm="bakery"`, which is not a valid Docker
	// challenge and makes credential-less BuildKit skip the mirror outright. It must
	// never leak onto an OCI route.
	if strings.Contains(challenge, "Basic") {
		t.Errorf("challenge = %q: the Basic realm must never appear on an OCI route", challenge)
	}
}

// TestReadAuthRequiredAdmitsAValidKey -- the other half of the gate.
func TestReadAuthRequiredAdmitsAValidKey(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.resolver.route.ReadAuthRequired = true
	f.authn.principal = fakePrincipal{canRead: true, canWrite: true}
	f.authn.err = nil

	raw := testIndex(t)
	f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)

	w := f.do(http.MethodGet, tenantPrefix+"library/alpine/manifests/3.20",
		map[string]string{"Authorization": "Bearer bkry_valid_key"})

	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}

	if seen := f.authn.tokens(); len(seen) != 1 || seen[0] != "bkry_valid_key" {
		t.Errorf("authenticator saw %v, want exactly the bkry_ token", seen)
	}
}

// TestPushIsUnsupported. There is no push API; the honest answer is UNSUPPORTED rather
// than a miss the client would retry.
func TestPushIsUnsupported(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	w := f.do(http.MethodGet, tenantPrefix+"library/alpine/blobs/uploads/abc", nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", w.Code)
	}

	assertOCIError(t, w.Body.Bytes(), codeUnsupported)
}

// TestBlobHitServesRangesAndDigest. Registry clients resume interrupted layer pulls
// with Range, and layers are the largest objects this system moves.
func TestBlobHitServesRangesAndDigest(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	body := []byte("a layer's worth of bytes, near enough for a range request")
	digest := f.seedBlob(t, body)

	target := tenantPrefix + "library/alpine/blobs/sha256:" + digest.String()

	w := f.do(http.MethodGet, target, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("blob GET = %d, want 200", w.Code)
	}

	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:"+digest.String() {
		t.Errorf("Docker-Content-Digest = %q", got)
	}

	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}

	ranged := f.do(http.MethodGet, target, map[string]string{"Range": "bytes=0-4"})
	if ranged.Code != http.StatusPartialContent {
		t.Fatalf("ranged blob GET = %d, want 206", ranged.Code)
	}

	if got := ranged.Body.String(); got != string(body[:5]) {
		t.Errorf("ranged body = %q, want %q", got, body[:5])
	}
}

// TestBlobHeadOnUncachedNeverDownloads. distribution's own proxy downloads the whole
// blob to answer a HEAD; containerd issues one HEAD per layer per pull, so that turns
// a cheap existence probe into hundreds of megabytes of upstream traffic.
func TestBlobHeadOnUncachedNeverDownloads(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.authn.principal = fakePrincipal{canRead: true, canWrite: true}
	f.authn.err = nil

	body := []byte("an uncached layer")
	digest := "sha256:" + hashOf(body)
	f.upstream.blobs[digest] = body

	w := f.do(http.MethodHead, tenantPrefix+"library/alpine/blobs/"+digest,
		map[string]string{"Authorization": "Bearer bkry_key"})

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD on an uncached blob = %d, want 200", w.Code)
	}

	if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}

	if w.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", w.Body.Len())
	}

	_, _, stats, gets := f.upstream.counts()
	if stats != 1 || gets != 0 {
		t.Errorf("upstream stat=%d get=%d, want stat=1 get=0 -- a HEAD must never pull a body",
			stats, gets)
	}
}

// TestRegisterTwicePanics guards the GLOBAL routes. There is one OCI backend value for
// the whole server; a future wiring change that constructs two must fail as a
// duplicate mount here rather than as a panic inside net/http.
func TestRegisterTwicePanics(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	defer func() {
		if recover() == nil {
			t.Error("Register called twice did not panic; the global /v2/ ping would " +
				"panic inside net/http instead")
		}
	}()

	f.backend.Register(http.NewServeMux())
}

// assertOCIError checks the spec's error envelope and its code.
func assertOCIError(t *testing.T, body []byte, wantCode string) {
	t.Helper()

	var got errorBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("error body is not the OCI envelope: %v (body %q)", err, body)
	}

	if len(got.Errors) != 1 || got.Errors[0].Code != wantCode {
		t.Errorf("error body = %+v, want one error with code %s", got.Errors, wantCode)
	}
}

// TestRealmFallbackIsHTTPOnlyForLoopback pins the one case where the ExternalURL-unset
// fallback may derive an http:// realm: a plainly local host, where cleartext is not a
// disclosure. Everything else fails closed to https (see TestRealmFallsBackToTheRequestHost).
func TestRealmFallbackIsHTTPOnlyForLoopback(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.backend.cfg.ExternalURL = ""

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = "127.0.0.1:8080"

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)

	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, `realm="http://127.0.0.1:8080/v2/token"`) {
		t.Errorf("challenge = %q, want an http realm for a loopback host", got)
	}
}

// TestOpenBackendDowngradesAForeignPrincipalToAnonymous pins the open-relay gate the
// adversarial review caught: on an open backend (ReadAuthRequired=false), a VALID
// bkry_ credential minted for a DIFFERENT project must be treated as anonymous, not
// passed through. The nil-Principal rule is this package's entire open-relay defence
// (no principal -> no upstream fetch), so a foreign-but-valid principal reaching the
// miss path would let any authenticated tenant spend the operator's upstream rate
// limit on every open backend in the installation -- invisibly, until Docker Hub 429s
// the whole deployment.
func TestOpenBackendDowngradesAForeignPrincipalToAnonymous(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.resolver.route.ReadAuthRequired = false
	// A key that AUTHENTICATES (it is valid somewhere) but does not admit THIS route.
	f.authn.principal = fakePrincipal{canRead: false, canWrite: false}
	f.authn.err = nil

	req := httptest.NewRequest(http.MethodGet, tenantPrefix+"library/alpine/manifests/3.20", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer bkry_foreignprojectkey")

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)

	// Anonymous-miss semantics: 404, and NOT ONE upstream call.
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign principal on an open backend: status = %d, want 404 (anonymous miss)", w.Code)
	}

	if r, m, sb, bg := f.upstream.counts(); r+m+sb+bg != 0 {
		t.Errorf("foreign principal triggered %d upstream call(s), want 0 -- the open-relay gate is broken",
			r+m+sb+bg)
	}
}

// TestTagsList drives the spec's tag-listing endpoint, which exists because a stock
// `skopeo inspect` (no --no-tags) lists a repository's tags on every inspect and
// treats a 404 as fatal -- the failure that broke CI the first time the real binary
// ran against this backend.
//
// The listing is CACHED TAGS ONLY: it is answered from the `tags` namespace with no
// upstream contact, anonymously on an open backend, and its scope assertions are the
// interesting half -- a sibling repository and the same repository at a DIFFERENT
// upstream both share the tags namespace and must not leak in.
func TestTagsList(t *testing.T) {
	t.Parallel()

	for _, fam := range bothFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			raw := testIndex(t)

			// Seeded out of order; the listing must come back sorted.
			f.seedTag(t, "docker.io", "library/alpine", "edge", raw)
			f.seedTag(t, "docker.io", "library/alpine", "3.20", raw)
			f.seedTag(t, "docker.io", "library/alpine", "3.19", raw)

			// The two adjacency traps: a sibling repo sharing the name as a prefix,
			// and the same repo mirrored from a different upstream host.
			f.seedTag(t, "docker.io", "library/alpine2", "9.9", raw)
			f.seedTag(t, "ghcr.io", "library/alpine", "foreign", raw)

			w := f.do(http.MethodGet, fam.prefix+"library/alpine/tags/list", nil)

			if w.Code != http.StatusOK {
				t.Fatalf("tags/list = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var got struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
			}

			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode tags/list body: %v (body %s)", err, w.Body.String())
			}

			if got.Name != "library/alpine" {
				t.Errorf("name = %q, want library/alpine", got.Name)
			}

			want := []string{"3.19", "3.20", "edge"}
			if len(got.Tags) != len(want) {
				t.Fatalf("tags = %v, want %v", got.Tags, want)
			}

			for i, tag := range want {
				if got.Tags[i] != tag {
					t.Fatalf("tags = %v, want %v (sorted, scoped to repo AND upstream)", got.Tags, want)
				}
			}

			if r, m, sb, bg := f.upstream.counts(); r+m+sb+bg != 0 {
				t.Errorf("tags/list made %d upstream call(s), want 0 -- the listing is cache-only", r+m+sb+bg)
			}
		})
	}
}

// TestTagsListHead: the GET pattern also matches HEAD, and a HEAD must answer headers
// only -- httptest's recorder would happily show us a body that net/http would strip,
// so the assertion is on our own write path.
func TestTagsListHead(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.seedTag(t, "docker.io", "library/alpine", "3.20", testIndex(t))

	w := f.do(http.MethodHead, tenantPrefix+"library/alpine/tags/list", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD tags/list = %d, want 200", w.Code)
	}

	if w.Body.Len() != 0 {
		t.Errorf("HEAD tags/list wrote a %d-byte body, want none", w.Body.Len())
	}

	if cl := w.Header().Get("Content-Length"); cl == "" || cl == "0" {
		t.Errorf("HEAD tags/list Content-Length = %q, want the GET body's length", cl)
	}
}

// TestTagsListEmptyIs404 pins the miss shape: a repository with no cached tags answers
// NAME_UNKNOWN, exactly as distribution's own tag store does, so the client falls back
// to a registry that knows more rather than being handed an empty list that reads as
// "this repository has no tags anywhere".
func TestTagsListEmptyIs404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	w := f.do(http.MethodGet, tenantPrefix+"library/alpine/tags/list", nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("empty tags/list = %d, want 404", w.Code)
	}

	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if len(body.Errors) != 1 || body.Errors[0].Code != codeNameUnknown {
		t.Errorf("error body = %s, want one NAME_UNKNOWN", w.Body.String())
	}
}
