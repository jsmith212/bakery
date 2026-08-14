package ociconf

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestContainerdResolverPullsMultiArchIndex drives containerd's OWN resolver -- the
// library nerdctl, ctr and every Kubernetes node pull with -- against Bakery's mirror
// route, and proves the four things that break silently.
//
//  1. ?ns= IS SENT AND HONORED. containerd appends ?ns=<the registry the image names>
//     to every request whose mirror host differs from the image host. Bakery resolves it
//     against the backend's allowlist; get that wrong and either the SSRF gate is open
//     or the mirror serves nothing.
//
//  2. THE HEAD DIGEST FAST PATH WORKS. containerd takes the descriptor straight off a
//     HEAD response, but ONLY when Docker-Content-Digest AND a Content-Length that is
//     not -1 are present on the SAME response (resolver.go: `if dgstHeader != "" && size
//     != -1`). Missing either makes it fall back to a full GET and hash the body itself
//     -- correct, invisible, and exactly twice the traffic the HEAD existed to avoid.
//     The assertion is on the REQUEST COUNT: with a working fast path Resolve issues no
//     GET at all, so the whole exchange contains exactly one manifest GET, the Fetch.
//
//  3. CONTENT-TYPE IS CARRIED VERBATIM. containerd assigns the manifest response's
//     Content-Type straight into ocispec.Descriptor.MediaType and dispatches on it.
//
//  4. THE BYTES ARE THE BYTES. The fetched body is compared to the captured file, byte
//     for byte, and its digest to the value the real registry reported. A json.Marshal
//     round trip anywhere in Bakery's ingest path fails both -- and only ever on a
//     multi-arch index, which is why the fixture is one.
func TestContainerdResolverPullsMultiArchIndex(t *testing.T) {
	e := newEnv(t)

	res := containerdResolver(e, projMain)
	ref := e.up.host + "/" + repoIndex + ":" + tagIndex

	name, desc, err := res.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("containerd Resolve(%s): %v", ref, err)
	}

	if name != ref {
		t.Errorf("resolved name = %q, want %q", name, ref)
	}

	if got := desc.Digest.String(); got != indexDigest {
		t.Errorf("resolved digest = %s, want %s", got, indexDigest)
	}

	if desc.MediaType != indexMediaType {
		t.Errorf("resolved media type = %q, want %q -- containerd dispatches on this verbatim",
			desc.MediaType, indexMediaType)
	}

	want, err := os.ReadFile(filepath.Join("testdata", "alpine-index.json"))
	if err != nil {
		t.Fatalf("read the captured index: %v", err)
	}

	if desc.Size != int64(len(want)) {
		t.Errorf("resolved size = %d, want %d", desc.Size, len(want))
	}

	got := containerdFetch(t, res, ref, desc)

	// This is the check containerd's own content store performs on ingest, and the one
	// that turns a re-serialized manifest from a subtle bug into a hard pull failure.
	if digestOf(got) != indexDigest {
		t.Fatalf("the fetched index hashes to %s, want %s -- the bytes were rewritten in transit",
			digestOf(got), indexDigest)
	}

	if string(got) != string(want) {
		t.Error("the fetched index is not byte-identical to the captured upstream bytes")
	}

	// ---- what the client actually sent, read off the recorder rather than off a doc.

	reqs := e.rec.snapshot()

	var (
		heads, gets int
		sawNS       bool
	)

	for _, r := range reqs {
		if !isManifestRequest(r) {
			continue
		}

		// The value is percent-encoded on the wire (the host carries a colon), so it is
		// decoded rather than string-matched -- a raw substring check would pass for the
		// wrong reason or fail for no reason depending on the port.
		if q, qerr := url.ParseQuery(r.query); qerr == nil && q.Get("ns") == e.up.host {
			sawNS = true
		}

		switch r.method {
		case http.MethodHead:
			heads++
		case http.MethodGet:
			gets++
		}
	}

	if !sawNS {
		t.Errorf("containerd never sent ?ns=%s; recorded requests: %v", e.up.host, reqs)
	}

	if heads == 0 {
		t.Error("containerd never issued a manifest HEAD -- the resolve fast path was not exercised")
	}

	if gets != 1 {
		t.Errorf("manifest GETs = %d, want exactly 1 (the Fetch). More than one means Resolve "+
			"fell back to GET-and-hash, i.e. Docker-Content-Digest or Content-Length was "+
			"missing from the HEAD response; recorded: %v", gets, reqs)
	}

	// The token dance, and specifically its POST half. containerd sends an OAuth2 form
	// POST FIRST whenever it holds a secret, and its 405 fallback to GET is additionally
	// gated on a non-empty USERNAME -- which a Bakery credential does not have, because
	// it is one opaque token. Against a GET-only token endpoint containerd would
	// hard-fail here and the mirror would be skipped in silence.
	posts := e.rec.count(func(r recordedReq) bool {
		return r.method == http.MethodPost && strings.HasSuffix(r.path, "/v2/token")
	})
	if posts == 0 {
		t.Errorf("containerd never POSTed the token endpoint; recorded: %v", reqs)
	}
}

// TestContainerdSecondPullIsServedByBakery is the anti-bypass assertion for containerd:
// a warm pull must contact the upstream ZERO times.
//
// Without it, every other assertion in this file would also pass against a Bakery that
// 404s everything, because containerd would transparently fetch from the upstream
// itself and report success. That is the failure mode this whole gate exists for.
func TestContainerdSecondPullIsServedByBakery(t *testing.T) {
	e := newEnv(t)

	ref := e.up.host + "/" + repoIndex + ":" + tagIndex

	cold := containerdResolver(e, projMain)

	_, desc, err := cold.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("cold Resolve: %v", err)
	}

	containerdFetch(t, cold, ref, desc)

	if e.up.count() == 0 {
		t.Fatal("the cold pull contacted the upstream zero times -- the fixture proves nothing")
	}

	e.up.reset()

	// A FRESH resolver, so nothing is answered out of containerd's own in-process state.
	warm := containerdResolver(e, projMain)

	_, warmDesc, err := warm.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("warm Resolve: %v", err)
	}

	body := containerdFetch(t, warm, ref, warmDesc)

	if n := e.up.count(); n != 0 {
		t.Errorf("the warm pull made %d upstream request(s): %v.\nThe tag is inside its TTL and the "+
			"manifest is content-addressed, so Bakery must have served both from its own store.",
			n, e.up.requests())
	}

	if warmDesc.Digest.String() != indexDigest || digestOf(body) != indexDigest {
		t.Errorf("warm pull digest = %s / %s, want %s", warmDesc.Digest, digestOf(body), indexDigest)
	}
}

// containerdFetch pulls one descriptor's bytes and drains them.
func containerdFetch(t *testing.T, res remotes.Resolver, ref string, desc ocispec.Descriptor) []byte {
	t.Helper()

	fetcher, err := res.Fetcher(t.Context(), ref)
	if err != nil {
		t.Fatalf("containerd Fetcher(%s): %v", ref, err)
	}

	rc, err := fetcher.Fetch(t.Context(), desc)
	if err != nil {
		t.Fatalf("containerd Fetch: %v", err)
	}

	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read the fetched content: %v", err)
	}

	return body
}

// containerdResolver builds containerd's resolver pointed at Bakery as a MIRROR.
//
// The shape is what a hosts.toml produces. `Path` is the mirror prefix with /v2 already
// appended -- containerd's own config loader appends /v2 to any configured host that
// does not already end in it -- and `Host` is BAKERY, not the upstream, which is
// precisely what makes isProxy() true and puts ?ns=<upstream> on every request.
//
// The credential is returned with an EMPTY USERNAME, and that is not a shortcut: a
// Bakery credential is one opaque token with no id:secret halves, which is exactly the
// hosts.toml `identitytoken` shape -- the one that forces containerd down the OAuth2
// POST path with its GET fallback unavailable (the 405 branch requires a username).
func containerdResolver(e *env, project string) remotes.Resolver {
	token := e.key(project)

	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthCreds(func(string) (string, string, error) { return "", token, nil }),
	)

	hosts := func(string) ([]docker.RegistryHost, error) {
		return []docker.RegistryHost{{
			Client:       http.DefaultClient,
			Authorizer:   authorizer,
			Host:         e.host(),
			Scheme:       "http",
			Path:         e.containerdPath(project),
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
			Header:       nil,
		}}, nil
	}

	return docker.NewResolver(docker.ResolverOptions{
		Hosts: hosts, Headers: nil, Tracker: nil,
		Authorizer: nil, Credentials: nil, Host: nil, PlainHTTP: false, Client: nil,
	})
}
