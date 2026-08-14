package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/metrics"
)

// refreshTimeout bounds a background tag revalidation. It is detached from the request
// that triggered it (see refresh), so nothing else would ever stop it -- an upstream
// that accepts a connection and then never answers would otherwise pin a goroutine and
// a singleflight slot for the life of the process.
const refreshTimeout = 30 * time.Second

// tagKey is the cache_objects key for one tag: "<normalized-host>/<name>:<tag>".
//
// THE HOST IS NORMALIZED AND THAT IS LOAD-BEARING. containerd sends ns=docker.io, the
// registry that actually answers is registry-1.docker.io, and Docker's own config files
// say index.docker.io. Unnormalized, one upstream tag becomes two or three independent
// rows with independent TTLs -- two upstream HEADs per window, and two answers that can
// disagree about which digest `:latest` is, depending on which client asked.
//
// The upstream host is IN the key rather than implied by the backend because a backend
// may proxy several upstreams (the ?ns= allowlist), and `library/alpine:latest` at
// docker.io and at a mirror are different images that must not share a row.
func tagKey(upstream, name, tag string) string {
	return upstream + "/" + name + ":" + tag
}

// serveTag answers a pull by tag: the one mutable, revalidated path in this package.
//
// # Stale-while-revalidate, and why it is not what the ecosystem does
//
// registry:2's pull-through proxy resolves EVERY tag pull against the remote first,
// synchronously, falling back to local only when that fails -- so every pull of a
// cached image pays a full upstream round trip, and its "TTL" is a seven-day content
// eviction timer, not a freshness horizon. That is the wrong trade for a build cache:
// the point of a mirror is to take the upstream off the hot path.
//
// Here, a tag is FRESH for tag_ttl and is served with zero upstream contact. Once
// stale it is still served IMMEDIATELY -- from cache, at local speed -- and a
// revalidation runs in the background. The client never waits for the upstream, and the
// worst case is that one pull, once per TTL, gets a digest that is at most one refresh
// interval old. For a tag, which is a mutable pointer by definition, that is the
// contract the tag already had.
//
// # The four states
//
//	MISS + principal      synchronous resolve + fetch. There is nothing to serve.
//	MISS + anonymous      404. No principal means no upstream, structurally.
//	FRESH                 serve, no upstream contact.
//	STALE                 serve immediately, revalidate in the background.
//
// An upstream OUTAGE therefore serves stale indefinitely and deliberately: a build
// cache that fails closed when Docker Hub is down is not doing its job. The outage is
// visible only in bakery_oci_tag_refresh_total{result="error"} -- no client will ever
// report it, because every client silently falls back to the real registry.
func (b *Backend) serveTag(w http.ResponseWriter, r *http.Request, req request) {
	ref := req.objectRef(nsTags, kindTag, tagKey(req.upstream, req.name, req.ref))

	// StatUncached, NOT Stat: the tags namespace bypasses the in-process LRU in both
	// directions. See blob.Service.StatUncached -- a tag is the only mutable key space
	// with a freshness contract, and a per-process cache of it would let a second
	// instance serve an old digest indefinitely and undetectably.
	meta, err := b.deps.Blobs.StatUncached(r.Context(), ref)
	if err != nil {
		b.internal(r.Context(), "stat tag", err)
		notFound(w, codeManifestUnknown)

		return
	}

	if !meta.Exists {
		b.serveTagMiss(w, r, req, ref)

		return
	}

	if b.stale(meta, req.policy.tagTTL) && req.principal != nil {
		b.refresh(r.Context(), req, ref, meta.Digest)
	}

	b.serveTagHit(w, r, req, meta)
}

// tagsResponse is the spec's tag-listing envelope: {"name": ..., "tags": [...]}.
type tagsResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// serveTagsList answers `GET <name>/tags/list` with the tags THIS CACHE HOLDS for the
// repository -- the `tags`-namespace keys under "<upstream>/<name>:" -- and never
// contacts the upstream.
//
// IT EXISTS BECAUSE `skopeo inspect` HARD-REQUIRES IT. containers/image lists a
// repository's tags on every plain inspect (only --no-tags suppresses it) and turns
// a 404 here into a fatal error -- discovered in CI, where the real skopeo binary
// runs; every PULL path in all four clients gets by without this endpoint, which is
// why M5 first shipped without it.
//
// CACHED TAGS ONLY, BY THE SAME PRODUCT DECISION AS EVERY OTHER MISS. A mirror that
// proxied this listing upstream would spend the operator's rate limit answering an
// enumeration question no build needs answered, and would need the principal gate and
// pagination plumbing to do it. Serving what we hold is honest for a cache: it is the
// same answer `registry:2`'s filesystem tag store gives, at local speed. The listing
// is one uncached query (the tags namespace bypasses the LRU in both directions; see
// serveTag) and the SQL's ORDER BY key within one fixed prefix IS lexical tag order,
// which is the ordering the spec requires.
//
// AN EMPTY LISTING IS A 404 (NAME_UNKNOWN), matching distribution, whose tag store
// answers ErrRepositoryUnknown when the tags path holds nothing. For a pull-through
// cache "no cached tags" and "no such repository" are the same fact, and 404 is the
// answer that sends the client to a registry that knows more.
//
// ?n=/?last= PAGINATION IS DELIBERATELY IGNORED: everything is returned in one page
// with no Link header, which every paginating client (crane sends n=1000,
// containers/image follows Link until absent) reads as "that was the whole list". A
// cached tag set is one row per tag ever pulled through -- small by construction --
// and truncating honestly would need the Link plumbing for a case that cannot get
// large.
func (b *Backend) serveTagsList(w http.ResponseWriter, r *http.Request, req request) {
	prefix := tagKey(req.upstream, req.name, "")

	keys, err := b.deps.Blobs.ListKeysByPrefix(r.Context(), req.objectRef(nsTags, kindTag, prefix))
	if err != nil {
		b.internal(r.Context(), "list tags", err)
		notFound(w, codeNameUnknown)

		return
	}

	if len(keys) == 0 {
		notFound(w, codeNameUnknown)

		return
	}

	tags := make([]string, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, strings.TrimPrefix(k, prefix))
	}

	body, err := json.Marshal(tagsResponse{Name: req.name, Tags: tags})
	if err != nil {
		b.internal(r.Context(), "encode tags", err)
		notFound(w, codeNameUnknown)

		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveTagMiss handles a tag we hold nothing for.
func (b *Backend) serveTagMiss(w http.ResponseWriter, r *http.Request, req request, ref blob.Ref) {
	if req.principal == nil {
		// PRODUCT DECISION: an anonymous miss is a 404, never an upstream fetch. The
		// client falls back to the real registry, which is exactly the verified-silent
		// behaviour of all four clients -- and the operator's registry credentials and
		// rate limit are never spent on an unauthenticated caller.
		notFound(w, codeManifestUnknown)

		return
	}

	meta, err := b.ingestTag(r.Context(), req, ref)
	if err != nil {
		b.logUpstream(r.Context(), req, "resolve tag", err)
		notFound(w, codeManifestUnknown)

		return
	}

	b.serveTagHit(w, r, req, meta)
}

// serveTagHit serves the manifest a tag names.
//
// IT SERVES OUT OF THE `manifests` NAMESPACE, not out of the tag row, and the digest it
// uses is the one the tag row just told us. That split is what makes the LRU bypass
// affordable: the MUTABLE lookup (tag -> digest) is one uncached query per TTL, and
// everything downstream of the digest is content-addressed and fully cached. Serving
// the bytes through the tag ref instead would put the whole manifest read back through
// the LRU, where a stale entry could hand back the previous digest's bytes.
func (b *Backend) serveTagHit(w http.ResponseWriter, r *http.Request, req request, meta blob.Meta) {
	ref := req.objectRef(nsManifests, kindManifest, meta.Digest.String())

	b.writeManifest(w, r, ref, meta)
}

// stale is the whole freshness rule: derived from updated_at, never stored.
//
// Deriving it means retuning tag_ttl applies instantly to every already-cached tag,
// with no migration against the largest table in the system and no restart -- and it
// means there is no expires_at column that can disagree with the row it describes.
func (b *Backend) stale(meta blob.Meta, ttl time.Duration) bool {
	return b.now().After(meta.UpdatedAt.Add(ttl))
}

// ingestTag resolves a tag upstream, stores the manifest, and writes the tag row.
//
// TWO ROWS, ONE BLOB. The manifest lands in the `manifests` namespace under its own
// digest (immutable), and the tag lands in `tags` under "<host>/<name>:<tag>"
// (overwritable) naming the SAME blob. The second Put re-presents bytes that are
// already durably stored, so blob.Service's dedup elides the byte write entirely and
// the tag costs one metadata row. There is no separate tag table and no join.
func (b *Backend) ingestTag(ctx context.Context, req request, ref blob.Ref) (blob.Meta, error) {
	return b.ingest(ctx, req, nsTags, ref.Key, func(fctx context.Context) (blob.Meta, error) {
		m, err := b.up.Manifest(fctx, req.principal, req.upstreamRef(), req.ref)
		if err != nil {
			return blob.Meta{}, err
		}

		return b.storeTag(fctx, req, ref, m)
	})
}

// storeTag writes the manifest and repoints the tag at it.
func (b *Backend) storeTag(
	ctx context.Context, req request, ref blob.Ref, m Manifest,
) (blob.Meta, error) {
	meta, err := b.storeManifest(ctx, req, m)
	if err != nil {
		return blob.Meta{}, err
	}

	// Overwrite: true -- `tags` is the one mutable namespace here. The refcount trigger
	// decrements the old manifest blob and increments the new one atomically, taking
	// both row locks in digest order, exactly as an /ac overwrite does. Go never does
	// that arithmetic.
	if _, err := b.deps.Blobs.Put(ctx, ref, bytes.NewReader(m.Raw), blob.PutOptions{
		Overwrite:   true,
		Verify:      blob.VerifyDigest(meta.Digest),
		ContentType: meta.ContentType,
	}); err != nil {
		return blob.Meta{}, err
	}

	return meta, nil
}

// refresh kicks off a background revalidation of a stale tag.
//
// IT IS DETACHED FROM THE REQUEST, with context.WithoutCancel -- the same pattern
// blob.Service's singleflight probe uses, and for the same reason. The request that
// noticed the staleness has already been answered from cache; if the refresh rode its
// context, a client that disconnects the instant it has its manifest (which is exactly
// what a fast client does) would cancel the revalidation every single time, and the tag
// would never refresh at all. WithoutCancel keeps the context's values -- trace IDs,
// and therefore pgx's tracing -- while dropping the cancellation and the deadline; a
// bounded timeout is then imposed here, because a detached context with no deadline is
// a goroutine leak waiting for a hung upstream.
//
// Singleflight collapses the storm: a hundred nodes hitting a stale `:latest` at the
// same moment produce ONE upstream HEAD, not a hundred.
func (b *Backend) refresh(ctx context.Context, req request, ref blob.Ref, have blob.Digest) {
	detached := context.WithoutCancel(ctx)

	go func() {
		// The flight key makes the doc comment above TRUE: without it, every request
		// that observes the stale tag spawns its own revalidation, and a thousand-node
		// cluster rolling one deployment burns a thousand upstream HEADs against the
		// operator's rate limit in the singleflight window where ONE was the promise.
		// Duplicates attach to the leader's flight and share its result; only the
		// leader (shared == false) reports the metric and the test hook, so neither
		// counts a refresh more than once.
		flight := "refresh\x00" + strconv.FormatInt(req.route.BackendID, 36) + "\x00" + ref.Key

		result, _, shared := b.sf.Do(flight, func() (any, error) {
			rctx, cancel := context.WithTimeout(detached, refreshTimeout)
			defer cancel()

			return b.revalidate(rctx, req, ref, have), nil
		})

		if shared {
			return
		}

		res, ok := result.(string)
		if !ok {
			res = metrics.OCIRefreshError
		}

		b.deps.Metrics.OCI(req.upstream).TagRefresh(res)

		if b.refreshHook != nil {
			b.refreshHook(res)
		}
	}()
}

// revalidate is one tag revalidation, and it returns the metrics result.
//
// THE UNCHANGED BRANCH MUST TOUCH THE ROW. It is the overwhelmingly common outcome --
// a stable tag does not move -- and it writes no bytes and repoints nothing, so without
// an explicit updated_at bump the row's freshness never advances. Every tag would then
// be permanently stale after its first TTL: ResultStale on every response forever, one
// upstream HEAD per singleflight window per tag forever, and a real upstream outage
// made indistinguishable from steady state. blob.Service.Touch is that bump, and it
// deliberately does not reset the GC write barrier -- a revalidation that confirmed
// nothing changed created nothing.
//
// The comparison uses the upstream's CLAIMED digest, which is unverified (see
// Manifest.Digest). That is safe in exactly this direction: a wrong claim can only
// cause a redundant full fetch, and the bytes that fetch returns are re-hashed by us
// before they are stored under anything.
func (b *Backend) revalidate(
	ctx context.Context, req request, ref blob.Ref, have blob.Digest,
) string {
	m, err := b.up.Resolve(ctx, req.principal, req.upstreamRef(), req.ref)
	if err != nil {
		// SERVE STALE, UNBOUNDED. The cached tag keeps being served; this counter is the
		// only witness the outage has.
		b.logUpstream(ctx, req, "revalidate tag", err)

		return metrics.OCIRefreshError
	}

	if hex, ok := digestHex(m.Digest); ok && hex == have.String() {
		found, terr := b.deps.Blobs.Touch(ctx, ref)
		if terr != nil {
			b.internal(ctx, "touch tag", terr)

			return metrics.OCIRefreshError
		}

		if !found {
			// The row was deleted underneath us. Not an error and not a repoint: the next
			// request takes the miss path and re-ingests.
			return metrics.OCIRefreshError
		}

		return metrics.OCIRefreshUnchanged
	}

	full, err := b.up.Manifest(ctx, req.principal, req.upstreamRef(), req.ref)
	if err != nil {
		b.logUpstream(ctx, req, "refetch tag", err)

		return metrics.OCIRefreshError
	}

	if _, err := b.storeTag(ctx, req, ref, full); err != nil {
		b.internal(ctx, "repoint tag", err)

		return metrics.OCIRefreshError
	}

	b.deps.Logger.DebugContext(ctx, "oci: tag repointed",
		slog.String("org", req.route.Org), slog.String("project", req.route.Project),
		slog.String("upstream", req.upstream))

	return metrics.OCIRefreshRepointed
}
