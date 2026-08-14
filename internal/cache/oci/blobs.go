package oci

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/storage"
)

// serveBlob answers a layer or config pull. Blobs are content-addressed and therefore
// IMMUTABLE: a cached blob is served forever with no upstream contact and no freshness
// question, which is the whole reason a registry mirror is worth having.
func (b *Backend) serveBlob(w http.ResponseWriter, r *http.Request, req request) {
	hex, ok := digestHex(req.ref)
	if !ok {
		notFound(w, codeBlobUnknown)

		return
	}

	ref := req.objectRef(nsBlobs, kindBlob, hex)

	meta, err := b.deps.Blobs.Stat(r.Context(), ref)
	if err != nil {
		b.internal(r.Context(), "stat blob", err)
		notFound(w, codeBlobUnknown)

		return
	}

	if meta.Exists {
		b.writeBlob(w, r, ref, meta)

		return
	}

	// NO PRINCIPAL, NO UPSTREAM.
	if req.principal == nil {
		notFound(w, codeBlobUnknown)

		return
	}

	if r.Method == http.MethodHead {
		b.headUncachedBlob(w, r, req, hex)

		return
	}

	if meta, err = b.ingestBlob(r.Context(), req, hex); err != nil {
		b.logUpstream(r.Context(), req, "blob", err)
		notFound(w, codeBlobUnknown)

		return
	}

	b.writeBlob(w, r, ref, meta)
}

// headUncachedBlob answers a HEAD for a blob we do not hold, with an upstream HEAD and
// NOTHING ELSE.
//
// It deliberately does not ingest. distribution's own proxy downloads the entire blob
// to answer a HEAD, which turns a client's cheap existence probe -- and containerd
// issues one per layer per pull -- into hundreds of megabytes of upstream traffic and
// disk writes for content nobody has asked to read yet. Answering from an upstream HEAD
// costs one round trip and gives the client exactly what it asked for; if it then GETs,
// the GET path ingests.
func (b *Backend) headUncachedBlob(w http.ResponseWriter, r *http.Request, req request, hex string) {
	size, err := b.up.StatBlob(r.Context(), req.principal, req.upstreamRef(), "sha256:"+hex)
	if err != nil {
		b.logUpstream(r.Context(), req, "stat blob", err)
		notFound(w, codeBlobUnknown)

		return
	}

	b.blobHeaders(w, hex, size)
	w.WriteHeader(http.StatusOK)
}

// ingestBlob fetches a blob and stores it, collapsing concurrent requests onto ONE
// upstream download.
//
// FETCH THEN SERVE FROM THE STORE -- no tee. Streaming the upstream body to the client
// and to storage at the same time would save one disk read on the very first pull and
// cost two things that matter more: a client disconnect mid-stream could land a partial
// object (blob.Service's staging protocol prevents that, but only by discarding the
// work), and followers on the singleflight would have nothing to attach to. Serving
// from the store afterwards is a local read of bytes that are, by then, provably
// complete and digest-verified.
func (b *Backend) ingestBlob(ctx context.Context, req request, hex string) (blob.Meta, error) {
	return b.ingest(ctx, req, nsBlobs, hex, func(fctx context.Context) (blob.Meta, error) {
		digest, err := storage.ParseKey(hex)
		if err != nil {
			return blob.Meta{}, err
		}

		// The upstream's reported size is deliberately DISCARDED: res.Size below is the
		// count of bytes that actually arrived and hashed to the requested digest, and
		// that is the only size any response may quote.
		rc, _, err := b.up.Blob(fctx, req.principal, req.upstreamRef(), "sha256:"+hex)
		if err != nil {
			return blob.Meta{}, err
		}

		defer func() { _ = rc.Close() }()

		ref := req.objectRef(nsBlobs, kindBlob, hex)

		// VerifyDigest against the digest the CLIENT asked for, re-hashed over the bytes
		// that actually arrived. A blob is addressed by its own content, so this is the
		// OCI trap in its pure form: bytes that do not hash to the key must never be
		// stored under it, or every client that verifies (all of them) fails forever.
		res, err := b.deps.Blobs.Put(fctx, ref, rc, blob.PutOptions{
			Overwrite: false, Verify: blob.VerifyDigest(digest), ContentType: "",
		})
		if err != nil {
			return blob.Meta{}, err
		}

		return blob.Meta{
			Exists: true, Digest: digest, Size: res.Size,
			UpdatedAt: b.now(), ContentType: "",
		}, nil
	})
}

// writeBlob serves a stored blob.
//
// Range support is not a nicety here: registry clients resume interrupted layer pulls
// with Range, and layers are the largest objects this system moves. httpblob.ServeObject
// gives 206/416/If-Range through http.ServeContent, exactly as distribution's own
// blobserver does.
func (b *Backend) writeBlob(w http.ResponseWriter, r *http.Request, ref blob.Ref, meta blob.Meta) {
	b.blobHeaders(w, meta.Digest.String(), meta.Size)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)

		return
	}

	_, rc, err := b.deps.Blobs.Get(r.Context(), ref)
	if err != nil {
		if !errors.Is(err, blob.ErrNotFound) {
			b.internal(r.Context(), "get blob", err)
		}

		notFound(w, codeBlobUnknown)

		return
	}

	defer func() { _ = rc.Close() }()

	httpblob.ServeObject(w, r, meta, rc)
}

// blobHeaders sets what a blob response must carry on GET AND on HEAD.
//
// Docker-Content-Digest and Content-Length must appear TOGETHER: containerd's fast path
// takes the descriptor straight off a HEAD response and requires both, and falls back
// to a full GET-and-hash if either is missing. On a HEAD that fallback silently doubles
// the traffic the HEAD existed to avoid.
//
// Content-Type is octet-stream and is deliberately NOT the layer's declared media type:
// a blob is opaque bytes to a registry, the manifest is what types it, and inventing a
// type here would be a guess that clients have no reason to trust.
func (b *Backend) blobHeaders(w http.ResponseWriter, hex string, size int64) {
	w.Header().Set("Docker-Content-Digest", "sha256:"+hex)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
}
