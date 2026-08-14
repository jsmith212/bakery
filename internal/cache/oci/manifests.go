package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/storage"
)

// defaultManifestType is what we serve when a stored manifest has no recorded media
// type -- only possible for a row written before content_type existed. It is the
// docker manifest list type rather than an OCI one because that is what the
// overwhelming majority of untyped legacy content is, and because a wrong guess here
// is a client dispatch failure rather than corruption.
const defaultManifestType = "application/vnd.docker.distribution.manifest.v2+json"

// serveManifest splits the two completely different kinds of manifest request.
//
// A DIGEST reference is immutable: `sha256:abc...` names exactly one byte string
// forever, so a cached hit is served with no upstream contact, ever, and no freshness
// question exists. A TAG is mutable and gets the whole stale-while-revalidate machine
// in tags.go. Conflating them is how a proxy ends up either revalidating immutable
// content on every pull or serving a tag that has not moved in six months.
func (b *Backend) serveManifest(w http.ResponseWriter, r *http.Request, req request) {
	if isDigestRef(req.ref) {
		b.serveManifestByDigest(w, r, req)

		return
	}

	b.serveTag(w, r, req)
}

// serveManifestByDigest answers a pull by digest.
func (b *Backend) serveManifestByDigest(w http.ResponseWriter, r *http.Request, req request) {
	hex, ok := digestHex(req.ref)
	if !ok {
		// Not a sha256 -- including a perfectly legal sha512 digest, which this proxy
		// cannot fetch upstream either. A clean miss sends the client to a registry that
		// can serve it.
		notFound(w, codeManifestUnknown)

		return
	}

	ref := req.objectRef(nsManifests, kindManifest, hex)

	meta, err := b.deps.Blobs.Stat(r.Context(), ref)
	if err != nil {
		b.internal(r.Context(), "stat manifest", err)
		notFound(w, codeManifestUnknown)

		return
	}

	if !meta.Exists {
		// NO PRINCIPAL, NO UPSTREAM. An anonymous miss is a 404 and the client falls back
		// to the real registry -- it is not an upstream fetch made on the operator's
		// credentials. This is the product decision that keeps an open-read mirror from
		// being an open relay.
		if req.principal == nil {
			notFound(w, codeManifestUnknown)

			return
		}

		if meta, err = b.ingestManifestByDigest(r.Context(), req, hex); err != nil {
			b.logUpstream(r.Context(), req, "manifest", err)
			notFound(w, codeManifestUnknown)

			return
		}

		// The ingest stored the bytes under the digest WE computed. If that is not the
		// digest the client asked for, the upstream answered a digest-pinned request
		// with bytes that do not hash to it -- lying or broken. Serving the mismatch
		// would put the wrong Docker-Content-Digest on the wire (and a HEAD would 200
		// for an object this path cannot GET). A miss is the honest answer: the client
		// falls back to a registry that can serve the real thing.
		if meta.Digest.String() != hex {
			b.logUpstream(r.Context(), req, "manifest digest mismatch",
				fmt.Errorf("requested sha256:%s, upstream bytes hash to sha256:%s", hex, meta.Digest))
			notFound(w, codeManifestUnknown)

			return
		}
	}

	b.writeManifest(w, r, ref, meta)
}

// ingestManifestByDigest fetches a manifest by digest and stores it, collapsing
// concurrent requests for the same manifest onto one upstream fetch.
func (b *Backend) ingestManifestByDigest(ctx context.Context, req request, hex string) (blob.Meta, error) {
	return b.ingest(ctx, req, nsManifests, hex, func(fctx context.Context) (blob.Meta, error) {
		m, err := b.up.Manifest(fctx, req.principal, req.upstreamRef(), req.ref)
		if err != nil {
			return blob.Meta{}, err
		}

		return b.storeManifest(fctx, req, m)
	})
}

// storeManifest writes manifest bytes into the manifests namespace and returns the
// Meta the caller can serve from.
//
// THE KEY IS THE DIGEST WE COMPUTED. storage.KeyOf hashes the bytes we actually hold,
// and that hash is both the storage key and the VerifyDigest policy handed to
// blob.Service -- which re-hashes the stream on the way in and rejects a mismatch. The
// upstream's Docker-Content-Digest header is never consulted, because
// go-containerregistry does not verify it for tag fetches and a wrong one would store
// bytes under a digest they do not hash to. Every client verifies manifest bytes
// against the digest it asked for, so that mistake is not a subtle one: it is a
// permanent hard pull failure for that image, for everyone.
//
// This is also, structurally, why no manifest can be re-serialized on the way through:
// the only bytes that can be stored under digest D are bytes that hash to D.
func (b *Backend) storeManifest(ctx context.Context, req request, m Manifest) (blob.Meta, error) {
	if len(m.Raw) == 0 {
		// An empty manifest body is never legitimate, and it is the one payload that
		// would sail through every check below (it hashes fine, it stores fine, and it
		// deserializes into nothing).
		return blob.Meta{}, errors.New("oci: upstream returned an empty manifest")
	}

	digest := storage.KeyOf(m.Raw)
	hex := digest.String()

	mediaType := m.MediaType
	if mediaType == "" {
		mediaType = defaultManifestType
	}

	ref := req.objectRef(nsManifests, kindManifest, hex)

	if _, err := b.deps.Blobs.Put(ctx, ref, bytes.NewReader(m.Raw), blob.PutOptions{
		Overwrite:   false, // a digest names one byte string forever
		Verify:      blob.VerifyDigest(digest),
		ContentType: mediaType,
	}); err != nil {
		return blob.Meta{}, err
	}

	return blob.Meta{
		Exists: true, Digest: digest, Size: int64(len(m.Raw)),
		UpdatedAt: b.now(), ContentType: mediaType,
	}, nil
}

// writeManifest serves one stored manifest, for GET and for HEAD.
//
// THREE HEADERS TOGETHER OR THE CLIENT DOES EXTRA WORK. containerd's HEAD fast path
// requires Docker-Content-Digest AND a Content-Length that is not -1 ON THE SAME
// RESPONSE; missing either forces it to fall back to a full GET and hash the body
// itself. And Content-Type is assigned verbatim into the descriptor's MediaType, so it
// must be the STORED media type, not a guess.
//
// The ETag is the digest, which makes If-None-Match a free 304 -- http.ServeContent
// evaluates the precondition for us, so a client re-checking a manifest it already has
// transfers no body.
func (b *Backend) writeManifest(
	w http.ResponseWriter, r *http.Request, ref blob.Ref, meta blob.Meta,
) {
	mediaType := meta.ContentType
	if mediaType == "" {
		mediaType = defaultManifestType
	}

	if !acceptable(r, mediaType) {
		// The client asked for media types we do not hold. The reference implementation
		// answers a miss here, and a miss is the right answer: the client falls back to a
		// registry that can content-negotiate, rather than being handed a document it
		// cannot dispatch.
		notFound(w, codeManifestUnknown)

		return
	}

	digest := "sha256:" + meta.Digest.String()

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("ETag", `"`+digest+`"`)

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)

		return
	}

	_, rc, err := b.deps.Blobs.Get(r.Context(), ref)
	if err != nil {
		if !errors.Is(err, blob.ErrNotFound) {
			b.internal(r.Context(), "get manifest", err)
		}

		notFound(w, codeManifestUnknown)

		return
	}

	defer func() { _ = rc.Close() }()

	httpblob.ServeObject(w, r, meta, rc)
}

// acceptable reports whether the client's Accept header admits this media type.
//
// It is LENIENT by design. An absent Accept means "anything" (curl, and any client
// that does not negotiate), and `*/*` or a matching type-range accepts. Only an
// explicit list that excludes what we hold is a miss. Being strict here would 404
// legitimate pulls: every client sends a slightly different list, and the OCI spec's
// own content-negotiation document is still a TODO, so there is no normative behavior
// to be strict against.
func acceptable(r *http.Request, mediaType string) bool {
	accept := r.Header.Values("Accept")
	if len(accept) == 0 {
		return true
	}

	for _, header := range accept {
		for _, entry := range strings.Split(header, ",") {
			// Drop any q= or other parameter; we do not rank, we only admit.
			value, _, _ := strings.Cut(entry, ";")

			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}

			if value == "*/*" || strings.EqualFold(value, mediaType) {
				return true
			}
		}
	}

	return false
}

// ingestTimeout bounds one detached upstream fetch-and-store. Generous, because a
// blob can be a multi-GB layer on a slow link -- but BOUNDED, because a detached
// context with no deadline plus a hung upstream is a leaked goroutine AND a
// permanently stuck singleflight key: every future request for that object would then
// join a flight that never lands.
const ingestTimeout = 10 * time.Minute

// ingest runs one upstream fetch-and-store under singleflight, so that N concurrent
// requests for the same object cost ONE upstream download.
//
// The flight is keyed by (backend, namespace, key): two projects mirroring the same
// image must not share a flight, because they have different credentials, different
// allowlists, and different storage rows.
//
// THE FLIGHT IS DETACHED FROM THE CALLER THAT HAPPENS TO LEAD IT -- the same rule,
// for the same reason, as blob.Service.stat's probe. A storm collapses N pulls onto
// ONE upstream fetch; if that fetch rode the leader's request context, the leader
// disconnecting (Ctrl-C, ingress idle timeout, HTTP/2 RST_STREAM) would cancel the
// shared fetch and hand context.Canceled to every follower -- each renders a 404,
// every client silently falls back to the real registry, the object is never cached,
// and the next storm re-elects a doomed leader. context.WithoutCancel keeps the
// values (trace IDs, pgx tracing) while dropping the cancellation; each caller still
// honours ITS OWN deadline against the shared flight via the select.
func (b *Backend) ingest(
	ctx context.Context, req request, namespace, key string, fn func(context.Context) (blob.Meta, error),
) (blob.Meta, error) {
	flight := strconv.FormatInt(req.route.BackendID, 36) + "\x00" + namespace + "\x00" + key

	fetchCtx := context.WithoutCancel(ctx)

	ch := b.sf.DoChan(flight, func() (any, error) {
		fctx, cancel := context.WithTimeout(fetchCtx, ingestTimeout)
		defer cancel()

		return fn(fctx)
	})

	select {
	case <-ctx.Done():
		return blob.Meta{}, fmt.Errorf("oci: ingest: %w", ctx.Err())

	case res := <-ch:
		if res.Err != nil {
			return blob.Meta{}, res.Err
		}

		meta, ok := res.Val.(blob.Meta)
		if !ok {
			return blob.Meta{}, errors.New("oci: singleflight returned an unexpected type")
		}

		return meta, nil
	}
}

// internal logs a fault that the client will only ever see as a 404.
//
// It is the ONLY witness: every registry client silently falls back to the real
// registry on any mirror failure, so a bug here produces green builds, no complaints,
// and a hit rate of zero. Logging loudly is not optional.
func (b *Backend) internal(ctx context.Context, op string, err error) {
	b.deps.Logger.ErrorContext(ctx, "oci: "+op, slog.Any("error", err))
}

// logUpstream records an upstream failure. An upstream MISS is ordinary and is logged
// at debug; anything else is an outage on our side of the world and is logged at warn,
// because no client will ever tell us.
func (b *Backend) logUpstream(ctx context.Context, req request, op string, err error) {
	level := slog.LevelWarn
	if errors.Is(err, ErrUpstreamNotFound) {
		level = slog.LevelDebug
	}

	b.deps.Logger.Log(ctx, level, "oci: upstream "+op,
		slog.String("org", req.route.Org), slog.String("project", req.route.Project),
		slog.String("upstream", req.upstream), slog.Any("error", err))
}
