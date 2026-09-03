package oci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/storage"
)

// serveUploadStart answers `POST <repo>/blobs/uploads/`, with or without
// `?mount=&from=`.
//
// 202 ALWAYS, INCLUDING FOR A CROSS-REPO MOUNT. containerd sends the mount query only
// when it holds a `containerd.io/distribution.source.<host>` label for the blob, and
// it treats 200/202/204 identically: "upload started, parse Location". Only a 201
// short-circuits the upload, and ignoring the mount is therefore ALWAYS safe -- the
// client simply uploads bytes we may already hold, and blob.Service's dedup elides the
// write anyway. Implementing the 201 fast path would trade a guaranteed-correct answer
// for a cross-repo existence check that saves nothing this namespace does not already
// save by content addressing.
//
// The Location is RELATIVE and stateless: nothing is recorded here, because the
// completing PUT carries the digest and the whole body.
func (b *BuildCache) serveUploadStart(
	w http.ResponseWriter, r *http.Request, route cache.Route, rest string,
) {
	_, session, ok := splitUpload(rest)
	if !ok || session != "" {
		// A POST anywhere but `<repo>/blobs/uploads/` names no upload. 404 rather than
		// 405: the path is wrong, not the verb.
		notFound(w, codeNameUnknown)

		return
	}

	if !b.authorizeWrite(w, r, route) {
		return
	}

	base, ok := uploadBase(r)
	if !ok {
		notFound(w, codeNameUnknown)

		return
	}

	// Only Location is load-bearing -- Docker-Upload-UUID is never read by containerd
	// and Range is read by no client that gets here -- but real registries send all
	// three and a human reading a packet capture expects them.
	w.Header().Set("Location", base+"/"+kindBlobs+"/"+segUploads+"/"+uploadSession)
	w.Header().Set("Range", "0-0")
	w.Header().Set("Docker-Upload-UUID", uploadSession)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

// servePut splits the two things a PUT can be: the completion of a blob upload, or a
// manifest by tag (or by digest).
//
// The upload check runs FIRST, for the same reason splitRef checks the push path
// before its marker scan: `<repo>/blobs/uploads/u` would otherwise parse as a blob
// reference named "u" and take the wrong branch.
func (b *BuildCache) servePut(w http.ResponseWriter, r *http.Request, route cache.Route, rest string) {
	if _, _, ok := splitUpload(rest); ok {
		// The session id is deliberately NOT checked against uploadSession. The upload is
		// stateless, so any session the client echoes back names the same nothing, and
		// refusing an unrecognized one would break a client that normalized our Location.
		if !b.authorizeWrite(w, r, route) {
			return
		}

		b.putBlob(w, r, route)

		return
	}

	name, kind, ref, err := splitRef(rest)
	if err != nil || kind != kindManifests {
		// PUT is defined only for an upload completion and a manifest. A PUT to
		// `<repo>/blobs/<digest>` is not a registry operation at all.
		notFound(w, codeNameUnknown)

		return
	}

	if !b.authorizeWrite(w, r, route) {
		return
	}

	b.putManifest(w, r, bcRef{route: route, name: name, ref: ref})
}

// putBlob completes an upload: `PUT <Location>?digest=<d>`.
//
// THE BODY STREAMS STRAIGHT INTO blob.Service.Put -- hash-as-you-copy, no buffering
// and no size cap, the same posture as the sstate mount, because multi-hundred-MB
// layers are the normal case. VerifyDigest re-hashes the bytes that actually arrived
// against the digest the client named: a blob is addressed by its OWN content, so this
// is the OCI trap in its pure form and bytes that do not hash to the key must never be
// stored under it.
//
// THE ANSWER IS 201 AND NEVER 202. containerd accepts 200/201/204 from the final PUT
// and HARD-REJECTS 202 (pusher.go:329) -- a 202 here fails the export, and with
// `ignore-error=true` (which the snippet always carries) it fails it SILENTLY, as a
// permanently cold cache nobody is told about.
//
// The repository name is deliberately not part of the key. Blobs are content-addressed
// and shared across every repository inside this backend, exactly as on the mirror; the
// backend_id is the tenancy boundary.
func (b *BuildCache) putBlob(w http.ResponseWriter, r *http.Request, route cache.Route) {
	hex, ok := digestHex(r.URL.Query().Get("digest"))
	if !ok {
		// Missing, malformed, or a legal-but-unsupported algorithm (sha512). On the READ
		// path an unsupported algorithm is a clean 404 so the client falls back; on a
		// write there is nowhere to fall back to, and 400 is the honest answer.
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "a sha256 digest parameter is required")

		return
	}

	digest, err := storage.ParseKey(hex)
	if err != nil {
		// Unreachable: digestHex already proved 64 lowercase hex characters.
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "malformed digest")

		return
	}

	ref := route.Ref(nsBlobs, kindBlob, hex)

	if _, err := b.deps.Blobs.Put(r.Context(), ref, r.Body, blob.PutOptions{
		Overwrite:   false, // a digest names one byte string forever
		Verify:      blob.VerifyDigest(digest),
		ContentType: "", // a blob is opaque bytes; the manifest is what types it
	}); err != nil {
		b.pushFailed(r.Context(), w, "put buildcache blob", err)

		return
	}

	// A re-PUT of a blob we already hold lands here too: Put's ON CONFLICT DO NOTHING
	// left the existing row standing and reported Created=false. 201 either way -- the
	// client's question is "is this blob now in the registry", and the answer is yes.
	w.Header().Set("Docker-Content-Digest", "sha256:"+hex)
	w.Header().Set("Content-Length", "0")

	if base, ok := uploadBase(r); ok {
		w.Header().Set("Location", base+"/"+kindBlobs+"/sha256:"+hex)
	}

	w.WriteHeader(http.StatusCreated)
}

// putManifest stores a pushed manifest and points its tag at it.
//
// # The bytes are stored VERBATIM and the digest is OURS
//
// Nothing here parses the document. The key is storage.KeyOf over the bytes we
// actually received, handed straight back to blob.Service as the VerifyDigest policy,
// so the only bytes that can be stored under digest D are bytes that hash to D. That
// is the same rule the mirror follows for the same reason, and it is what lets both
// BuildKit cache shapes -- the index-of-blob-descriptors default and the
// image-manifest=true form with its vnd.buildkit.cacheconfig.v0 config -- round-trip
// through a registry that has no opinion about either.
//
// # Two rows, one blob
//
// The manifest lands in `manifests` under its own digest (immutable) and the tag lands
// in `tags` under "<repo>:<tag>" (Overwrite: true) naming the SAME blob. The second Put
// re-presents bytes that are already durably stored, so dedup elides the byte write and
// the tag costs one metadata row. The refcount trigger does the decrement-old /
// increment-new arithmetic on an overwrite; Go never does. The displaced manifest row
// stays, untagged, and ages out on the GC ladder.
func (b *BuildCache) putManifest(w http.ResponseWriter, r *http.Request, q bcRef) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManifestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeManifestInvalid, "manifest body could not be read")

		return
	}

	if len(raw) == 0 {
		// The one payload that would sail through every check below: it hashes fine, it
		// stores fine, and it names nothing any client can use. Same trap as the /ac
		// zero-length value, and it is rejected for the same reason.
		writeError(w, http.StatusBadRequest, codeManifestInvalid, "empty manifest")

		return
	}

	digest := storage.KeyOf(raw)
	hex := digest.String()

	if isDigestRef(q.ref) {
		// A push BY DIGEST asserts what the bytes hash to. Storing them under OUR digest
		// while answering the client's would put a manifest at an address it cannot be
		// fetched from; refusing is the only answer that leaves the store consistent.
		if want, ok := digestHex(q.ref); !ok || want != hex {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "content does not match the digest")

			return
		}
	}

	// The client's Content-Type is stored and echoed on every read: containerd
	// assigns a manifest response's Content-Type straight into the descriptor's
	// MediaType and dispatches on it, and the document's own optional `mediaType` field
	// is not a trustworthy substitute. But ONLY from the closed manifest-type set --
	// the header is echoed to browsers too, and echoing an attacker-chosen text/html
	// on an open-read backend is stored XSS on Bakery's own origin (the console's
	// session cookie lives there). The BYTES are still stored verbatim; the verbatim
	// invariant covers content, never this response header. An off-list type stores
	// NULL and reads fall back to defaultManifestType, same as an absent header.
	mediaType := r.Header.Get("Content-Type")
	if !manifestMediaTypeAllowed(mediaType) {
		mediaType = ""
	}

	manifestRef := q.route.Ref(nsManifests, kindManifest, hex)

	if _, err := b.deps.Blobs.Put(r.Context(), manifestRef, bytes.NewReader(raw), blob.PutOptions{
		Overwrite:   false, // a digest names one byte string forever
		Verify:      blob.VerifyDigest(digest),
		ContentType: mediaType,
	}); err != nil {
		b.pushFailed(r.Context(), w, "put buildcache manifest", err)

		return
	}

	if !isDigestRef(q.ref) {
		tagRef := q.route.Ref(nsTags, kindTag, buildCacheTagKey(q.name, q.ref))

		if _, err := b.deps.Blobs.Put(r.Context(), tagRef, bytes.NewReader(raw), blob.PutOptions{
			Overwrite:   true, // `tags` is the one mutable namespace: every export repoints it
			Verify:      blob.VerifyDigest(digest),
			ContentType: mediaType,
		}); err != nil {
			b.pushFailed(r.Context(), w, "point buildcache tag", err)

			return
		}
	}

	w.Header().Set("Docker-Content-Digest", "sha256:"+hex)
	w.Header().Set("Content-Length", "0")

	if base, ok := manifestBase(r); ok {
		w.Header().Set("Location", base+"/"+kindManifests+"/sha256:"+hex)
	}

	w.WriteHeader(http.StatusCreated)
}

// pushFailed renders a write-path failure with the OCI envelope containerd parses.
//
// A 5xx HERE IS CORRECT, and it is the one place in this package where that is true.
// The read path never 500s because every client answers a 5xx from a mirror by
// retrying rather than falling back. A push has nothing to fall back to: mapping a
// storage fault onto a 404 would tell BuildKit "cache not found", which -- since
// `--cache-to` failures are soft only under ignore-error=true, which the snippet always
// sets -- turns an outage into a permanently cold cache and a green build.
func (b *BuildCache) pushFailed(ctx context.Context, w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, blob.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "content does not match the digest")

	case errors.Is(err, blob.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid object key")

	default:
		b.internal(ctx, op, err)
		writeError(w, http.StatusInternalServerError, codeUnknown, "storage error")
	}
}

// manifestMediaTypeAllowed is the closed set of manifest media types a push may store
// for echo-on-read. BuildKit's cache exporter sends exactly two -- the OCI index
// (default) and the OCI image manifest (image-manifest=true), verified from its
// source at v0.33.0 -- and the two Docker schema2 types are headroom for ordinary
// clients pushing through the same wire. Everything else is refused a stored type
// (NOT refused storage): text/html here would otherwise be served back from Bakery's
// own origin, and that is the whole distance to stored XSS against the console.
func manifestMediaTypeAllowed(mt string) bool {
	switch mt {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}

	return false
}
