package oci

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// TestRegistryRejectsNilPrincipal is the open-relay gate, asserted on the REAL
// go-containerregistry-backed Fetcher rather than on the fake.
//
// auth.Principal is sealed -- unexported method, no exported constructor, no usable
// zero value -- so a caller cannot forge one. Passing nothing is the only remaining
// hole, and these four checks close it. Without them an anonymous request that misses
// becomes a fetch on the OPERATOR's registry credentials and rate limit: an open relay
// serving Docker Hub to the internet, indistinguishable from a busy cache.
func TestRegistryRejectsNilPrincipal(t *testing.T) {
	t.Parallel()

	up, err := NewRegistry(Config{ExternalURL: "", UpstreamAuth: nil}, metrics.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	ref := UpstreamRef{Host: "docker.io", Name: "library/alpine"}
	ctx := t.Context()

	if _, err := up.Resolve(ctx, nil, ref, "latest"); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("Resolve with a nil principal = %v, want ErrNoPrincipal", err)
	}

	if _, err := up.Manifest(ctx, nil, ref, "latest"); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("Manifest with a nil principal = %v, want ErrNoPrincipal", err)
	}

	if _, err := up.StatBlob(ctx, nil, ref, "sha256:"+testIndexDigest); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("StatBlob with a nil principal = %v, want ErrNoPrincipal", err)
	}

	if _, _, err := up.Blob(ctx, nil, ref, "sha256:"+testIndexDigest); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("Blob with a nil principal = %v, want ErrNoPrincipal", err)
	}
}

// TestRegistryAgainstARealRegistryProtocol drives the production Fetcher against an
// httptest server speaking the registry wire protocol.
//
// It is the only test that exercises go-containerregistry itself, and it exists for one
// claim in particular: Puller.Get returns the RAW manifest bytes, unparsed, so nothing
// in the upstream leg can re-serialize a manifest. gcr resolves 127.0.0.1 over plain
// HTTP with no insecure flag, so an httptest server is a legitimate registry to it.
func TestRegistryAgainstARealRegistryProtocol(t *testing.T) {
	t.Parallel()

	raw := testIndex(t)
	blobBody := []byte("a layer, for the blob half of the protocol")
	blobDigest := "sha256:" + storage.KeyOf(blobBody).String()

	srv := httptest.NewServer(fakeRegistry(t, raw, blobBody, blobDigest))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")

	up, err := NewRegistry(Config{ExternalURL: "", UpstreamAuth: nil}, metrics.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	ref := UpstreamRef{Host: host, Name: "library/alpine"}
	p := fakePrincipal{canRead: true, canWrite: true}
	ctx := t.Context()

	m, err := up.Manifest(ctx, p, ref, "3.20")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if string(m.Raw) != string(raw) {
		t.Error("Manifest returned bytes that are not the registry's own; something re-serialized")
	}

	if m.MediaType != testIndexType {
		t.Errorf("MediaType = %q, want the response Content-Type %q verbatim", m.MediaType, testIndexType)
	}

	if hex, ok := digestHex(m.Digest); !ok || hex != testIndexDigest {
		t.Errorf("Digest = %q, want sha256:%s", m.Digest, testIndexDigest)
	}

	head, err := up.Resolve(ctx, p, ref, "3.20")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if hex, _ := digestHex(head.Digest); hex != testIndexDigest {
		t.Errorf("Resolve digest = %q, want sha256:%s", head.Digest, testIndexDigest)
	}

	if len(head.Raw) != 0 {
		t.Error("Resolve returned a body; a HEAD must not")
	}

	size, err := up.StatBlob(ctx, p, ref, blobDigest)
	if err != nil {
		t.Fatalf("StatBlob: %v", err)
	}

	if size != int64(len(blobBody)) {
		t.Errorf("StatBlob size = %d, want %d", size, len(blobBody))
	}

	rc, _, err := up.Blob(ctx, p, ref, blobDigest)
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}

	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}

	if string(got) != string(blobBody) {
		t.Error("Blob returned the wrong bytes")
	}

	// A miss must be the ErrUpstreamNotFound sentinel, not an opaque error: the handler
	// renders a miss as a clean 404 and an outage as a loud metric, and folding them
	// together makes a real outage invisible.
	if _, err := up.Manifest(ctx, p, ref, "no-such-tag"); !errors.Is(err, ErrUpstreamNotFound) {
		t.Errorf("Manifest for a missing tag = %v, want ErrUpstreamNotFound", err)
	}
}

// fakeRegistry is a minimal but honest registry: the /v2/ ping, manifests by tag, and
// blobs by digest, with the headers real clients depend on.
func fakeRegistry(t *testing.T, manifest, blobBody []byte, blobDigest string) http.Handler {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
	})

	serveManifest := func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "3.20" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"unknown"}]}`))

			return
		}

		w.Header().Set("Content-Type", testIndexType)
		w.Header().Set("Docker-Content-Digest", "sha256:"+testIndexDigest)
		w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
		// A live rate-limit header, in Docker Hub's actual form: the window is
		// server-supplied and is never hardcoded on our side.
		w.Header().Set("Ratelimit-Remaining", "97;w=3600")
		w.WriteHeader(http.StatusOK)

		if r.Method != http.MethodHead {
			_, _ = w.Write(manifest)
		}
	}

	mux.HandleFunc("GET /v2/library/alpine/manifests/{ref}", serveManifest)
	mux.HandleFunc("HEAD /v2/library/alpine/manifests/{ref}", serveManifest)

	serveBlob := func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("digest") != blobDigest {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(blobBody)))
		w.WriteHeader(http.StatusOK)

		if r.Method != http.MethodHead {
			_, _ = w.Write(blobBody)
		}
	}

	mux.HandleFunc("GET /v2/library/alpine/blobs/{digest}", serveBlob)
	mux.HandleFunc("HEAD /v2/library/alpine/blobs/{digest}", serveBlob)

	return mux
}

// TestClassifyUpstreamRequest pins the metrics `op` label to a CLOSED set inferred from
// the request. A request that matches nothing (the ping, the auth realm) is recorded
// under NO op rather than an open-ended one -- an unbounded op label is the same
// cardinality bomb as labelling on an image name.
func TestClassifyUpstreamRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		want   metrics.OCIOp
		ok     bool
	}{
		{
			name: "manifest head is a resolve", method: http.MethodHead,
			path: "/v2/library/alpine/manifests/3.20", want: metrics.OCIOpResolve, ok: true,
		},
		{
			name: "manifest get", method: http.MethodGet,
			path: "/v2/library/alpine/manifests/3.20", want: metrics.OCIOpManifest, ok: true,
		},
		{
			name: "blob head", method: http.MethodHead,
			path: "/v2/library/alpine/blobs/sha256:x", want: metrics.OCIOpStatBlob, ok: true,
		},
		{
			name: "blob get", method: http.MethodGet,
			path: "/v2/library/alpine/blobs/sha256:x", want: metrics.OCIOpBlob, ok: true,
		},
		{name: "the ping is not an op", method: http.MethodGet, path: "/v2/", want: "", ok: false},
		{name: "the auth realm is not an op", method: http.MethodGet, path: "/token", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequest(tt.method, "http://registry-1.docker.io"+tt.path, nil)

			got, ok := classifyUpstreamRequest(r)
			if got != tt.want || ok != tt.ok {
				t.Errorf("classifyUpstreamRequest(%s %s) = (%q, %v), want (%q, %v)",
					tt.method, tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}
