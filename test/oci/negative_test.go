package ociconf

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestNoPrincipalNoUpstream is the open-relay fence, asserted from the outside.
//
// An OCI backend with read_auth_required=false serves anyone. If an anonymous MISS were
// allowed to trigger an upstream fetch, Bakery would be relaying Docker Hub to the
// internet on the OPERATOR'S credentials and rate limit -- and it would look exactly
// like a busy, healthy cache while doing it. So the product decision is: no principal,
// no upstream, structurally. The client falls back to the real registry, which is the
// verified behaviour of all four of them.
//
// The assertion that matters is the SECOND one. A 404 alone proves nothing: it is also
// what a working relay returns when the upstream happens not to have the image. Zero
// upstream requests is the proof.
func TestNoPrincipalNoUpstream(t *testing.T) {
	e := newEnv(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no credential at all", anonymous()},
		{
			// Every client that completes the anonymous token dance replays this literal
			// string on every manifest and blob request thereafter. It must be recognized
			// as anonymous by name -- not sent to the OIDC verifier, which is where a
			// non-bkry_ Bearer token otherwise goes, once per blob, on the hot path.
			name:    "the anonymous sentinel replayed as Bearer",
			headers: map[string]string{"Authorization": "Bearer anonymous", "Accept": acceptManifests},
		},
		{
			// Docker Engine forwards the user's REAL Docker Hub credentials to whatever it
			// has configured as a mirror, unchanged, on every pull -- it has no per-mirror
			// credential scoping. Rejecting these outright would break every docker-login'd
			// engine on earth while looking, in review, exactly like correct security. They
			// are treated as anonymous instead: no principal, so no upstream, so a clean
			// 404 and a silent fallback to Hub, which is where those credentials belong.
			name: "a forwarded Docker Hub credential",
			headers: map[string]string{
				"Authorization": "Basic " + base64.StdEncoding.EncodeToString(
					[]byte("dockeruser:dckr_pat_notabakerytoken")),
				"Accept": acceptManifests,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.up.reset()

			url := e.family2(projOpen, manifestPath(repoIndex, tagIndex))

			res := request(t, http.MethodGet, url, tc.headers)
			if res.status != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404; body %s", url, res.status, res.body)
			}

			assertOCIError(t, res.body, "MANIFEST_UNKNOWN")

			if n := e.up.count(); n != 0 {
				t.Errorf("an anonymous miss made %d upstream request(s): %v.\nThat is an open relay: "+
					"an unauthenticated caller just spent the operator's registry credentials "+
					"and rate limit.", n, e.up.requests())
			}
		})
	}
}

// TestAnonymousReadsServeCacheHits is the other half of the rule above, and it is what
// makes an open mirror worth having: anonymous callers are served everything Bakery
// already holds. Only the MISS is refused.
func TestAnonymousReadsServeCacheHits(t *testing.T) {
	e := newEnv(t)

	url := e.family2(projOpen, manifestPath(repoIndex, tagIndex))

	// Warm it with a real credential -- the ingest half needs a principal.
	if res := request(t, http.MethodGet, url, bearer(e.key(projOpen))); res.status != http.StatusOK {
		t.Fatalf("warming GET %s = %d; body %s", url, res.status, res.body)
	}

	e.up.reset()

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"anonymous", anonymous()},
		{"an unrecognized credential, treated as anonymous", map[string]string{
			"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("who:whatever")),
			"Accept":        acceptManifests,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := request(t, http.MethodGet, url, tc.headers)
			if res.status != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body %s", url, res.status, res.body)
			}

			if digestOf(res.body) != indexDigest {
				t.Errorf("served body hashes to %s, want %s", digestOf(res.body), indexDigest)
			}
		})
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("serving cached content made %d upstream request(s): %v", n, e.up.requests())
	}
}

// TestUnknownNamespaceIsRefusedWithoutDialling is the SSRF gate.
//
// ?ns= is attacker-controlled input naming a host Bakery will make an outbound
// connection to. Without an allowlist the proxy is an SSRF primitive into whatever
// network it is deployed in, reachable by anyone who can reach the cache --
// go-containerregistry's own SSRF hardening covers auth realms and redirects, NOT the
// registry host, because that is normally the caller's own choice. Here it is not.
//
// The answer is 404 and not 403 for a reason: 404 is indistinguishable from "no such
// image", so a scanner sweeping ?ns= across internal hostnames learns nothing about
// which ones exist.
func TestUnknownNamespaceIsRefusedWithoutDialling(t *testing.T) {
	e := newEnv(t)
	e.up.reset()

	url := e.family1(projMain, manifestPath(repoIndex, tagIndex)) + "?ns=evil.internal.example"

	res := request(t, http.MethodGet, url, bearer(e.key(projMain)))
	if res.status != http.StatusNotFound {
		t.Fatalf("GET %s = %d, want 404; body %s", url, res.status, res.body)
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("an unlisted ?ns= made %d upstream request(s): %v", n, e.up.requests())
	}
}

// TestReferenceParsingEdges pins the reference parser against the shapes that break a
// naive one. Each row is a real client request, and each failure is silent.
func TestReferenceParsingEdges(t *testing.T) {
	e := newEnv(t)
	key := e.key(projMain)

	t.Run("a repository name containing a literal manifests segment", func(t *testing.T) {
		// THE PATHOLOGICAL NAME. `acme/manifests/alpine` is a legal repository name, and
		// its manifest URL therefore contains "/manifests/" TWICE. A parser that splits on
		// the FIRST marker reads the repository as "acme" and the reference as
		// "alpine/manifests/1.0", which resolves to nothing and 404s a real image forever,
		// silently, for exactly one customer. Same class as REAPI's ByteStream
		// instance_name, same fix: scan from the right.
		url := e.family1(projMain, manifestPath(repoManifestsInName, tagManifestsInName))

		res := request(t, http.MethodGet, url, bearer(key))
		if res.status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body %s", url, res.status, res.body)
		}

		if digestOf(res.body) != indexDigest {
			t.Errorf("served body hashes to %s, want %s", digestOf(res.body), indexDigest)
		}
	})

	t.Run("a legal non-sha256 digest is a clean miss", func(t *testing.T) {
		// sha512 is a legal OCI digest that this proxy cannot serve --
		// go-containerregistry is sha256-only, so it could not be fetched upstream either.
		// A clean 404 sends the client to a registry that can serve it; a 400 or a 500
		// would make some clients retry rather than fall back.
		url := e.family1(projMain, manifestPath(repoIndex, "sha512:"+strings.Repeat("a", 128)))

		if res := request(t, http.MethodGet, url, bearer(key)); res.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", url, res.status)
		}
	})

	t.Run("the push API is refused honestly", func(t *testing.T) {
		// There is no push API and there will not be one in M5. A 404 with UNSUPPORTED
		// tells a client the truth; a plain miss would invite it to retry the upload.
		url := e.family1(projMain, repoIndex+"/blobs/uploads/")

		res := request(t, http.MethodGet, url, bearer(key))
		if res.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", url, res.status)
		}

		assertOCIError(t, res.body, "UNSUPPORTED")
	})

	t.Run("an unconfigured project is 404, never 401", func(t *testing.T) {
		// A project with no oci backend row has no mount to serve and must not say so --
		// a 401 would confirm the project exists and tell a scanner what to come back for.
		// The route resolves before any credential is looked at, which is why this is 404
		// even with a perfectly good key from another project.
		url := e.httpBase + "/v2/" + envOrg + "/no-such-project/" + manifestPath(repoIndex, tagIndex)

		if res := request(t, http.MethodGet, url, bearer(key)); res.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", url, res.status)
		}
	})
}

// TestPrivateBackendChallengesRatherThanForbids pins the 401-not-403 rule on a
// read_auth_required backend.
//
// 403 is wrong twice: it is a project-existence oracle, and it is the code that makes
// some clients stop retrying instead of falling back. And the 401 must carry the Bearer
// challenge, or the client has nothing to authenticate against.
func TestPrivateBackendChallengesRatherThanForbids(t *testing.T) {
	e := newEnv(t)

	url := e.family1(projMain, manifestPath(repoIndex, tagIndex))

	res := request(t, http.MethodGet, url, anonymous())
	if res.status != http.StatusUnauthorized {
		t.Fatalf("GET %s with no credential = %d, want 401; body %s", url, res.status, res.body)
	}

	if ch := res.header.Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Bearer ") {
		t.Errorf("401 challenge = %q, want a Bearer challenge", ch)
	}

	assertOCIError(t, res.body, "UNAUTHORIZED")
}

// TestHeadOnAnUncachedBlobDoesNotDownloadIt pins a deliberate divergence from the
// reference implementation.
//
// distribution's own pull-through proxy answers a HEAD for a blob it does not hold by
// DOWNLOADING THE ENTIRE BLOB and then discarding the body. containerd issues one
// existence probe per layer per pull, so that turns a client's cheapest request into
// hundreds of megabytes of upstream traffic and disk writes for content nobody has
// asked to read. Bakery answers from an upstream HEAD and ingests nothing; if the client
// then GETs, the GET path ingests.
//
// The proof is on the UPSTREAM's request log, because the two behaviours are
// indistinguishable from the client's side -- both return the same headers.
func TestHeadOnAnUncachedBlobDoesNotDownloadIt(t *testing.T) {
	e := newEnv(t)

	digest, size := e.up.aBlobOf(t, repoProbe)
	url := e.family2(projMain, blobPath(repoProbe, digest))
	auth := bearer(e.key(projMain))

	e.up.reset()

	head := request(t, http.MethodHead, url, auth)
	if head.status != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200", url, head.status)
	}

	// Docker-Content-Digest and Content-Length TOGETHER: containerd builds the descriptor
	// straight off a HEAD response and needs both, and falls back to a full GET-and-hash
	// if either is missing -- silently doubling the traffic the HEAD existed to avoid.
	if got := head.header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}

	if got := head.header.Get("Content-Length"); got != itoa(size) {
		t.Errorf("Content-Length = %q, want %q", got, itoa(size))
	}

	for _, req := range e.up.requests() {
		if strings.HasPrefix(req, http.MethodGet+" ") && strings.Contains(req, "/blobs/") {
			t.Errorf("a downstream HEAD caused an upstream blob GET (%s): the whole layer was "+
				"downloaded to answer an existence probe", req)
		}
	}

	// The GET does ingest, and a second GET is served locally.
	if res := request(t, http.MethodGet, url, auth); res.status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, res.status)
	}

	e.up.reset()

	if res := request(t, http.MethodGet, url, auth); res.status != http.StatusOK || len(res.body) != size {
		t.Fatalf("warm GET %s = %d with %d bytes, want 200 with %d", url, res.status, len(res.body), size)
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("a cached blob read made %d upstream request(s): %v", n, e.up.requests())
	}
}

// itoa keeps the Content-Length comparison a string comparison, which is what the header
// actually is.
func itoa(n int) string { return strconv.Itoa(n) }

// assertOCIError checks the spec's error envelope. The STATUS is the part every client
// acts on and the body is the part none of them parse, but a wrong code here is a sign
// the wrong branch produced the response.
func assertOCIError(t *testing.T, body []byte, wantCode string) {
	t.Helper()

	var envelope struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the error body is not the OCI envelope: %v (body %s)", err, body)
	}

	if len(envelope.Errors) == 0 || envelope.Errors[0].Code != wantCode {
		t.Errorf("error code = %+v, want %s", envelope.Errors, wantCode)
	}
}
