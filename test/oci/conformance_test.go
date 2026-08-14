// Package ociconf proves that Bakery's M5 Docker/OCI pull-through proxy serves the
// REAL registry clients. It is M5's CI gate, and DESIGN.md calls the conformance gates
// non-negotiable.
//
// # Why this gate exists at all, and why it is shaped the way it is
//
// EVERY REGISTRY CLIENT SILENTLY FALLS BACK TO THE REAL REGISTRY when a mirror fails --
// on a 404, a 500, a bad challenge, a timeout, a missing header. So a completely broken
// Bakery produces green builds, no complaints and a hit rate of zero, and no client-side
// signal exists anywhere. "The pull worked" therefore proves NOTHING on its own. Every
// success assertion in this package is paired with an assertion about the FAKE
// UPSTREAM'S REQUEST LOG: it is the only way to tell "Bakery served it" from "Bakery was
// bypassed and the client quietly fetched it itself".
//
// # The clients
//
//	containerd's own resolver (containerd/v2 core/remotes/docker, a TEST-ONLY dep). No
//	daemon, no root, no containers -- the library that does the actual pulling. It is the
//	only client that exercises ?ns=, the HEAD digest fast path, and the OAuth2 POST token
//	endpoint an identitytoken credential forces.
//
//	go-containerregistry (crane's engine), by tag AND by digest, through BOTH route
//	families, plus a full validate.Image over manifest + config + every layer.
//
//	skopeo, if installed. The containers/image stack is the only client that hard-
//	requires the bare-root GET /v2/ ping, and the only one that harvests auth challenges
//	from the ping response and nowhere else.
//
//	Docker Engine, by SHAPE. The daemon cannot run in CI, so its URL construction is
//	replicated exactly -- ValidateMirror's trailing-slash normalization followed by
//	ResolveReference -- and driven at Bakery as ordinary HTTP. That chain is the reason
//	Docker Engine can be a fourth supported client at all, and dropping one slash from it
//	silently drops a path segment and sends every pull back to Docker Hub.
//
// It lives OUTSIDE internal/ on purpose. `just race`/`just coverage` glob ./..., and
// this package is compiled and run there; the skopeo test therefore calls requireBinary
// FIRST, before dbtest.New and before the server boots, so a skip on a runner without it
// costs nothing. `just oci-conformance` is its home, and there a skip FAILS the job.
package ociconf

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// TestMain gives the suite its own ephemeral Postgres. dbtest.Main is lazy -- the
// container (or the TEST_DB_URL template clone) is created by the first dbtest.New and
// by nothing else -- so a run that skips every test pays nothing for it.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// acceptManifests is the Accept list every real client sends on a manifest request. It
// is spelled out here because Bakery's content negotiation answers a miss when the
// stored media type is not admitted, and a helper that sent no Accept at all would
// silently take the lenient path and prove less than it looks like it does.
const acceptManifests = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// httpResult is one raw exchange with Bakery: everything an assertion might need,
// already drained.
type httpResult struct {
	status int
	header http.Header
	body   []byte
}

// request issues one request at Bakery with no client library in the way. It is what
// the ping, token, negative and Docker-Engine-shape tests use: the point of those is the
// exact bytes on the wire, and a client library would normalize them.
func request(t *testing.T, method, url string, headers map[string]string) httpResult {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}

	return httpResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// bearer is the credential header a client replays after the token dance. A Bakery
// credential is ONE opaque token, so it goes in verbatim.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "Accept": acceptManifests}
}

// anonymous is a request with no credential at all.
func anonymous() map[string]string {
	return map[string]string{"Accept": acceptManifests}
}

// manifestPath is the tail of a manifest URL: <name>/manifests/<tag-or-digest>.
func manifestPath(name, ref string) string { return name + "/manifests/" + ref }

// blobPath is the tail of a blob URL.
func blobPath(name, digest string) string { return name + "/blobs/" + digest }

// isManifestRequest matches a recorded request against the manifests route, ignoring
// which tenant or repository it named.
func isManifestRequest(r recordedReq) bool { return strings.Contains(r.path, "/manifests/") }

// prefixTransport prepends a mirror path prefix to every /v2/... request, which is
// exactly what containerd's RegistryHost.Path and Docker Engine's mirror base URL do.
//
// It is how go-containerregistry -- which has no concept of a registry with a path,
// because a registry name is an authority -- is driven through the FIRST route family.
// The rewrite is deliberately conditional on the /v2/ prefix: the Bearer realm Bakery
// advertises on that family ALREADY contains the mirror path, so prefixing it a second
// time would produce a doubled path and a 404 in the token dance.
type prefixTransport struct {
	base   http.RoundTripper
	prefix string
}

func (t prefixTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.URL.Path, "/v2/") {
		clone := r.Clone(r.Context())
		clone.URL.Path = t.prefix + r.URL.Path

		//nolint:wrapcheck // a pass-through RoundTripper must not decorate the error.
		return t.base.RoundTrip(clone)
	}

	//nolint:wrapcheck // a pass-through RoundTripper must not decorate the error.
	return t.base.RoundTrip(r)
}
