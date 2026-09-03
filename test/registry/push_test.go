package regconf

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The media types the two BuildKit cache shapes are made of.
//
// cacheConfigType is the one that trips CONFIG-ALLOWLISTING registries: the
// image-manifest=true export is an ordinary OCI image manifest whose config descriptor
// names a document that is not an image config at all. Bakery accepts it because
// nothing in the writable namespace parses a manifest -- which is exactly what this
// package proves from outside the process.
const (
	imageManifestType = "application/vnd.oci.image.manifest.v1+json"
	cacheConfigType   = "application/vnd.buildkit.cacheconfig.v0"
	layerType         = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// pushBlob runs the REAL two-step blob upload -- POST `blobs/uploads/`, then one
// monolithic PUT to the returned Location with `?digest=` -- and returns the digest.
//
// It is the sequence containerd emits, in the order containerd emits it, and the status
// codes are asserted rather than tolerated: containerd accepts 200/202/204 from the POST
// and HARD-REJECTS a 202 from the final PUT (pusher.go:329). A server that answered 202
// there would fail every export, and with ignore-error=true in the snippet it would fail
// them silently.
func (e *env) pushBlob(t *testing.T, repo string, body []byte) string {
	t.Helper()

	base := e.repoURL(repo)
	digest := digestOf(body)

	start := request(t, http.MethodPost, base+"/blobs/uploads/", nil, e.writeHeaders(nil))
	if start.status != http.StatusAccepted {
		t.Fatalf("POST %s/blobs/uploads/ = %d, want 202 (body %s)", base, start.status, start.body)
	}

	location := start.header.Get("Location")
	if location == "" {
		t.Fatal("the upload POST returned no Location: containerd has nowhere to PUT")
	}

	done := request(t, http.MethodPut, e.resolve(location)+"?digest="+digest,
		bytes.NewReader(body), e.writeHeaders(nil))
	if done.status != http.StatusCreated {
		t.Fatalf("PUT %s?digest=%s = %d, want 201 -- containerd rejects a 202 outright "+
			"(body %s)", location, digest, done.status, done.body)
	}

	if got := done.header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("the blob PUT answered Docker-Content-Digest %q, want %q", got, digest)
	}

	return digest
}

// pushManifest stores a manifest under a tag and returns the digest the server computed
// for it -- which is the digest of the bytes WE sent, because the server computes it
// itself and never trusts a client's claim about its own content.
func (e *env) pushManifest(t *testing.T, repo, tag string, raw []byte, mediaType string) string {
	t.Helper()

	url := e.repoURL(repo) + "/manifests/" + tag

	res := request(t, http.MethodPut, url, bytes.NewReader(raw),
		e.writeHeaders(map[string]string{"Content-Type": mediaType}))
	if res.status != http.StatusCreated {
		t.Fatalf("PUT %s = %d, want 201 (body %s)", url, res.status, res.body)
	}

	digest := res.header.Get("Docker-Content-Digest")
	if want := digestOf(raw); digest != want {
		t.Fatalf("the manifest PUT answered Docker-Content-Digest %q, want %q -- the stored "+
			"digest is computed over the bytes received, never taken from the client",
			digest, want)
	}

	return digest
}

// writeHeaders is a write-scoped credential plus whatever else the caller needs.
//
// A Bakery credential is ONE OPAQUE TOKEN with no id:secret halves, so it goes in the
// Basic password field with filler for the username -- exactly what the snippet
// generator emits and what every client here ends up sending.
func (e *env) writeHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": basicAuth(e.writeKey)}

	for k, v := range extra {
		h[k] = v
	}

	return h
}

// resolve turns the Location a push response returned into an absolute URL.
//
// Bakery answers a RELATIVE Location on purpose -- containerd resolves it against the
// host that answered, so a deployment behind a path-rewriting ingress still completes
// its upload -- and this is that resolution, done the way the client does it.
func (e *env) resolve(location string) string {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location
	}

	return e.local + location
}

// basicAuth renders a Basic header carrying one Bakery token.
func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("bakery:"+token))
}

// digestOf is the registry addressing function: sha256 over the exact bytes.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// buildKitCacheManifest renders the `image-manifest=true` cache export shape.
//
// THE FORMATTING IS DELIBERATELY NON-CANONICAL -- mixed indentation, key orders no
// encoder produces, no trailing newline. A registry that unmarshalled and re-marshalled
// this document would return something that still parses identically and hashes
// DIFFERENTLY, which is the exact bug the "never re-serialize a manifest" invariant
// exists to prevent and the exact bug a byte comparison catches.
//
// The config descriptor names a vnd.buildkit.cacheconfig.v0 document, which is what
// makes this the interesting shape: it is not an image config, and a registry that
// allowlists config media types rejects the export. Bakery has no opinion because it
// never opens the manifest.
func buildKitCacheManifest(configDigest string, configSize int, layerDigest string, layerSize int) []byte {
	return []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": %q,
  "config": {"mediaType":%q,"digest":%q,"size":%d},
  "layers": [
        {"mediaType":%q,"digest":%q,"size":%d,"annotations":{"buildkit/createdat":"2026-09-03T00:00:00Z"}}
  ]
}`,
		imageManifestType,
		cacheConfigType, configDigest, configSize,
		layerType, layerDigest, layerSize))
}
