package ociconf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// The repositories the fake upstream is seeded with. Each one exists to make a
// different assertion possible.
const (
	// repoIndex holds REAL, captured multi-arch index bytes. It is the payload that
	// catches a re-serialization bug: a json.Marshal round trip anywhere in the ingest
	// path reorders keys and rewrites whitespace, which changes the digest -- and it
	// only reproduces on an index, i.e. not in your unit test and yes in production.
	repoIndex = "library/alpine"
	tagIndex  = "3.20"

	// repoManifestsInName is a LEGAL repository name containing a literal "manifests"
	// path component. A parser that splits on the FIRST /manifests/ marker reads it as
	// repository "acme" with reference "manifests/alpine/manifests/1.0" and 404s a real
	// image forever, silently, for exactly one customer. Same class as REAPI's
	// ByteStream instance_name.
	repoManifestsInName = "acme/manifests/alpine"
	tagManifestsInName  = "1.0"

	// repoProbe is a complete, self-consistent single-arch image -- manifest, config
	// blob and layer blobs -- so a client can pull and VALIDATE the whole thing rather
	// than just a manifest document.
	repoProbe = "bakery/probe"
	tagProbe  = "v1"

	// repoMoving is the tag the stale-while-revalidate test repoints under Bakery.
	repoMoving = "bakery/moving"
	tagMoving  = "latest"
)

// indexDigest is the digest registry-1.docker.io reported for the captured index,
// verified independently with sha256sum. Hardcoded on purpose: computing it from the
// bytes would make every digest assertion in this suite tautological.
const indexDigest = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// indexMediaType is the Content-Type the registry sent with it. containerd assigns a
// manifest response's Content-Type verbatim into ocispec.Descriptor.MediaType and
// dispatches on it, so serving the wrong one is a client-side dispatch failure, not a
// cosmetic difference.
const indexMediaType = "application/vnd.oci.image.index.v1+json"

// storedManifest is one manifest as the upstream holds it: the exact bytes and the
// exact media type, never a parsed document.
type storedManifest struct {
	raw       []byte
	mediaType string
}

// fakeRegistry is a minimal, hermetic OCI registry: enough of the pull API for
// go-containerregistry's Puller (which is what Bakery's real upstream client uses) and
// nothing else.
//
// It is hand-written rather than borrowed from go-containerregistry's pkg/registry for
// two reasons. First, that implementation REJECTS an index whose child manifests are
// not also uploaded, which would make it impossible to seed the captured alpine index
// without vendoring six more manifests. Second, and more important, this one counts
// every request and can be reset -- and that counter is the anti-bypass assertion this
// whole gate turns on.
type fakeRegistry struct {
	srv *httptest.Server

	// host is the authority clients name it by: "127.0.0.1:PORT". It goes verbatim into
	// each backend's default_upstream and upstreams allowlist.
	host string

	mu        sync.Mutex
	manifests map[string]storedManifest // repo + "\x00" + (tag | sha256:hex)
	blobs     map[string][]byte         // sha256:hex
	repoBlobs map[string][]string       // repo -> the blob digests its manifest names
	reqs      []string                  // "METHOD /path"
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()

	r := &fakeRegistry{
		srv: nil, host: "", mu: sync.Mutex{},
		manifests: map[string]storedManifest{}, blobs: map[string][]byte{},
		repoBlobs: map[string][]string{}, reqs: nil,
	}

	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)

	r.host = strings.TrimPrefix(r.srv.URL, "http://")

	r.seed(t)

	return r
}

// serve is the whole registry: a ping, manifests and blobs.
//
// It is a bare HandlerFunc rather than a ServeMux on purpose. The registry grammar puts
// slashes inside repository names, so every useful pattern is a {rest...} wildcard, and
// mixing those with a `{$}` ping is exactly the ServeMux registration hazard the real
// backend documents. Parsing one path by hand costs ten lines and carries no trap.
func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.Path)
	r.mu.Unlock()

	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)

		return
	}

	if rest == "" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte("{}"))

		return
	}

	name, kind, ref, err := splitUpstreamRef(rest)
	if err != nil {
		http.NotFound(w, req)

		return
	}

	if kind == "blobs" {
		r.serveBlob(w, req, ref)

		return
	}

	r.serveManifest(w, req, name, ref)
}

func (r *fakeRegistry) serveManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	r.mu.Lock()
	m, ok := r.manifests[name+"\x00"+ref]
	r.mu.Unlock()

	if !ok {
		writeUpstreamError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")

		return
	}

	// Docker-Content-Digest AND Content-Length on the same response, GET and HEAD
	// alike: that pair is what a real registry sends and what a resolver's HEAD fast
	// path needs. Bakery's own client (go-containerregistry) tolerates their absence,
	// but a fake that omitted them would quietly stop being a fake of anything.
	w.Header().Set("Docker-Content-Digest", digestOf(m.raw))
	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.raw)))
	w.WriteHeader(http.StatusOK)

	if req.Method != http.MethodHead {
		_, _ = w.Write(m.raw)
	}
}

func (r *fakeRegistry) serveBlob(w http.ResponseWriter, req *http.Request, ref string) {
	r.mu.Lock()
	body, ok := r.blobs[ref]
	r.mu.Unlock()

	if !ok {
		writeUpstreamError(w, http.StatusNotFound, "BLOB_UNKNOWN")

		return
	}

	w.Header().Set("Docker-Content-Digest", ref)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)

	if req.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// putManifest stores a manifest under BOTH its tag and its digest, the way every
// registry does, and returns the digest.
func (r *fakeRegistry) putManifest(repo, tag string, raw []byte, mediaType string) string {
	digest := digestOf(raw)

	r.mu.Lock()
	defer r.mu.Unlock()

	m := storedManifest{raw: raw, mediaType: mediaType}
	r.manifests[repo+"\x00"+tag] = m
	r.manifests[repo+"\x00"+digest] = m

	return digest
}

func (r *fakeRegistry) putBlob(body []byte) string {
	digest := digestOf(body)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.blobs[digest] = body

	return digest
}

// reset clears the request log. Every anti-bypass assertion is "reset, do it again,
// expect zero".
func (r *fakeRegistry) reset() {
	r.mu.Lock()
	r.reqs = nil
	r.mu.Unlock()
}

func (r *fakeRegistry) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.reqs...)
}

func (r *fakeRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.reqs)
}

// seed loads the four repositories the suite pulls from.
func (r *fakeRegistry) seed(t *testing.T) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "alpine-index.json"))
	if err != nil {
		t.Fatalf("read the captured index: %v", err)
	}

	if got := digestOf(raw); got != indexDigest {
		t.Fatalf("the captured index hashes to %s, want %s -- testdata drifted", got, indexDigest)
	}

	r.putManifest(repoIndex, tagIndex, raw, indexMediaType)
	r.putManifest(repoManifestsInName, tagManifestsInName, raw, indexMediaType)

	// A complete image, so a client can pull manifest + config + layers and validate the
	// lot. random.Image builds a genuinely self-consistent image; nothing about it is
	// mocked at the wire level.
	img, err := random.Image(512, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	r.seedImage(t, repoProbe, tagProbe, img)

	moving, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image (moving): %v", err)
	}

	r.seedImage(t, repoMoving, tagMoving, moving)
}

// seedImage stores an image's manifest, its config blob and every layer blob.
func (r *fakeRegistry) seedImage(t *testing.T, repo, tag string, img v1.Image) string {
	t.Helper()

	raw, err := img.RawManifest()
	if err != nil {
		t.Fatalf("RawManifest: %v", err)
	}

	mediaType, err := img.MediaType()
	if err != nil {
		t.Fatalf("MediaType: %v", err)
	}

	cfg, err := img.RawConfigFile()
	if err != nil {
		t.Fatalf("RawConfigFile: %v", err)
	}

	owned := []string{r.putBlob(cfg)}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}

	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatalf("Compressed: %v", err)
		}

		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}

		if err := rc.Close(); err != nil {
			t.Fatalf("close layer: %v", err)
		}

		owned = append(owned, r.putBlob(body))
	}

	r.mu.Lock()
	r.repoBlobs[repo] = owned
	r.mu.Unlock()

	return r.putManifest(repo, tag, raw, string(mediaType))
}

// aBlobOf returns one blob a repository's manifest actually names, and its size. It is
// what the HEAD-on-an-uncached-blob test pulls, and it is per-repository rather than
// "any blob in the store" so the request it drives is one a real client would make.
func (r *fakeRegistry) aBlobOf(t *testing.T, repo string) (digest string, size int) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	owned := r.repoBlobs[repo]
	if len(owned) == 0 {
		t.Fatalf("no blobs seeded for %q", repo)
	}

	return owned[0], len(r.blobs[owned[0]])
}

// seedImageFresh repoints a tag at a BRAND NEW image and returns its manifest digest.
// It is how the stale-while-revalidate test moves a tag under Bakery, which is the one
// thing a pull-through proxy has to notice and the one thing it can get wrong in
// complete silence.
func (r *fakeRegistry) seedImageFresh(t *testing.T, repo, tag string) string {
	t.Helper()

	img, err := random.Image(384, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	return r.seedImage(t, repo, tag, img)
}

// digestOf is the registry addressing function: sha256 over the exact bytes.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// splitUpstreamRef parses a registry path tail the same way the backend under test
// does -- scanning RIGHT TO LEFT for the marker -- so the fake cannot be the reason a
// pathological repository name passes or fails.
func splitUpstreamRef(rest string) (name, kind, ref string, err error) {
	mi := strings.LastIndex(rest, "/manifests/")
	bi := strings.LastIndex(rest, "/blobs/")

	var (
		idx  int
		mark string
	)

	switch {
	case mi < 0 && bi < 0:
		return "", "", "", fmt.Errorf("not a manifests or blobs reference: %q", rest)
	case mi > bi:
		idx, mark = mi, "manifests"
	default:
		idx, mark = bi, "blobs"
	}

	name = rest[:idx]
	ref = rest[idx+len(mark)+2:]

	if name == "" || ref == "" || strings.Contains(ref, "/") {
		return "", "", "", fmt.Errorf("malformed reference: %q", rest)
	}

	return name, mark, ref, nil
}

// writeUpstreamError renders the spec's error envelope, which is what
// go-containerregistry parses to tell a clean miss from an outage.
func writeUpstreamError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"not found"}]}`, code)
}
