package oci

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The literal segments that carve the WRITABLE namespace out of both registry route
// families, and the one upload-session id this backend ever hands out.
const (
	// segBuildCache is the ref shape's fixed first component:
	// <host>/{org}/{project}/buildcache/<repo>:<tag>. It is registered as a LITERAL
	// mux segment, which is what makes it win over the mirror's {rest...} with no
	// splitRef change and no registration panic.
	segBuildCache = "buildcache"

	// segUploads is the push API's marker segment, <repo>/blobs/uploads/...
	segUploads = "uploads"

	// uploadSession is the ONLY session id this backend issues, and it is a constant
	// because THE UPLOAD IS STATELESS. containerd has no chunked (PATCH) upload at
	// all -- every blob is one monolithic PUT to the Location we return, carrying the
	// whole body and the digest in the query -- and it never reads Docker-Upload-UUID.
	// So a per-request id would be state minted, stored and expired for nobody, and a
	// session table would be a new way for a push to fail.
	uploadSession = "u"
)

// maxManifestBytes bounds a manifest PUT. It is distribution's own limit, and it is a
// ceiling on a document that names blobs rather than on the blobs themselves: a
// mode=max BuildKit cache index of twenty thousand descriptors still fits. Blob PUTs
// are deliberately UNCAPPED and unbuffered (see putBlob) -- multi-hundred-MB layers
// are the normal case, exactly as on the sstate mount.
const maxManifestBytes = 4 << 20

// BuildCache is the WRITABLE OCI namespace: the target of BuildKit's
// `--cache-to type=registry`, mounted at .../buildcache/<repo> inside both registry
// route families and backed by the `registry` backend kind.
//
// # It has no upstream, and that is the point
//
// The type embeds core and adds nothing. There is NO Fetcher field, no upstream
// allowlist and no ?ns= resolution, so the open-relay class of bug that the mirror
// spends a whole gate on (an anonymous miss fetching on the operator's Docker Hub
// credentials) is not merely disabled here -- it is unrepresentable. A miss is a clean
// 404 with the OCI envelope, which is exactly what BuildKit's cache import treats as
// "no cache yet" and answers with a cold build.
//
// # What it stores, and what it refuses to understand
//
// The three OCI namespaces are reused verbatim under this backend's own backend_id:
// `blobs` and `manifests` keyed by digest hex (immutable, digest-verified), `tags`
// keyed "<repo>:<tag>" (mutable, no host prefix -- there is no host). NOTHING HERE
// PARSES A MANIFEST, and that is what makes both BuildKit cache shapes work by
// construction: the default export is an OCI image INDEX whose manifests[] entries are
// blob descriptors (which trips validating registries), and image-manifest=true is an
// image MANIFEST whose config media type is application/vnd.buildkit.cacheconfig.v0
// (which trips config-allowlisting registries). Bakery accepts both because it has no
// opinion about either.
//
// # Product framing
//
// The wire surface is a real registry push and cannot be restricted by content. v1
// documents, gates and supports ONLY BuildKit cache export; pushing ordinary images
// works and is explicitly unsupported (no listing, no delete, no docker push snippet).
type BuildCache struct {
	core

	// mirror is the pull-through backend the buildcache patterns shadow. ServeMux
	// registration is static, so the literal segment shadows EVERY project; spec §2
	// scopes the shadow to projects that actually HAVE a registry backend, and serve
	// honors that dynamically by handing reads back to the mirror when Resolve finds
	// no registry row. Nil-tolerant for tests that exercise the backend alone.
	mirror *Backend
}

var _ cache.Backend = (*BuildCache)(nil)

// NewBuildCache builds the writable buildcache backend. Note the missing parameter:
// there is no Fetcher, because there is nothing to fetch from. The mirror parameter
// is the fallback for the routing shadow (see BuildCache.mirror), not an upstream.
func NewBuildCache(deps cache.Deps, routes RouteResolver, authn Authenticator, cfg Config, mirror *Backend) *BuildCache {
	return &BuildCache{
		core:   core{deps: deps, routes: routes, authn: authn, cfg: cfg, warnRealmOnce: sync.Once{}},
		mirror: mirror,
	}
}

// Kind reports the DB enum this backend serves. The (project_id, kind) unique means
// one writable namespace per project.
func (b *BuildCache) Kind() repository.BackendKind { return repository.BackendKindRegistry }

// Register mounts the buildcache subtree on both route families.
//
// LITERAL BEATS WILDCARD, and that is the whole routing design. These patterns sit
// beside the mirror's `GET /v2/{org}/{project}/{rest...}` and
// `GET /cache/{org}/{project}/docker/v2/{rest...}`; ServeMux prefers the pattern whose
// matches are a strict subset, and a literal `buildcache` segment is exactly that. So
// there is no registration panic, no splitRef change, and no shared handler that has
// to guess which namespace it is in.
//
// The accepted, documented shadow: on a project that HAS a registry backend, a mirror
// pull of an upstream repository literally named `buildcache/...` resolves here
// instead. That collision is the same class as the `token` reserved slug and is
// resolved the same way -- by declaration.
//
// THE VERB SET IS THE WHOLE PUSH API. GET (which also matches HEAD -- registering
// `HEAD <pat>` beside `GET <pat>` panics the mux; see Backend.Register), POST for
// `blobs/uploads/`, and PUT for the upload completion and the manifest. PATCH and
// DELETE are deliberately UNREGISTERED so ServeMux answers its own 405: containerd
// sends neither (it has no chunked upload, and untagging is out of scope), and a 405
// is a truer answer than a handler that pretends.
func (b *BuildCache) Register(mux *http.ServeMux) {
	for _, pat := range []string{
		"/cache/{org}/{project}/docker/v2/" + segBuildCache + "/{rest...}",
		"/v2/{org}/{project}/" + segBuildCache + "/{rest...}",
	} {
		mux.HandleFunc("GET "+pat, b.serve) // also matches HEAD
		mux.HandleFunc("POST "+pat, b.serve)
		mux.HandleFunc("PUT "+pat, b.serve)
	}
}

// bcRef is one resolved buildcache request: the tenant and the parsed reference. It is
// the mirror's `request` minus everything that only an upstream needs.
type bcRef struct {
	route cache.Route
	name  string // repository name inside the writable namespace, e.g. impulse
	ref   string // tag, or sha256:<hex>
}

// buildCacheTagKey is the cache_objects key for one tag: "<repo>:<tag>".
//
// NO HOST PREFIX. The mirror's tag keys carry the normalized upstream host because one
// backend may proxy several upstreams and `library/alpine:latest` at docker.io and at
// a mirror are different images. Here there is no upstream and therefore no host to
// disambiguate, and inventing one would put a fake hostname in every key.
func buildCacheTagKey(name, tag string) string { return name + ":" + tag }

// serve is the whole handler, and its ORDER is the contract.
//
//  1. Resolve the route. An absent or disabled `registry` row 404s the entire
//     buildcache subtree, to everyone, BEFORE any authentication -- the standing
//     "never mount what you cannot serve" invariant, and it is also what stops a 401
//     from telling a scanner which projects exist.
//  2. Dispatch on the verb, because the push verbs and the read verb parse the tail
//     differently: `<repo>/blobs/uploads/...` is a push URL and is not a blob
//     reference, and splitRef rejects it on purpose.
//  3. Authenticate, inside the verb handler and BEFORE the first read of r.Body. Go's
//     server emits 100-continue and starts draining only on that first read, so an
//     early 401/403 makes a well-behaved client abort a multi-hundred-MB layer upload
//     instead of the server swallowing one it will reject.
func (b *BuildCache) serve(w http.ResponseWriter, r *http.Request) {
	route, ok := b.routes.Resolve(r.Context(), r.PathValue("org"), r.PathValue("project"), b.Kind())
	if !ok || !route.Enabled {
		// No registry backend: the buildcache shadow must not apply (spec §2 scopes
		// it to projects that HAVE one), so a read belongs to the mirror -- which
		// serves an upstream repository legitimately named `buildcache/...`, or
		// answers its own 404 when it too is unconfigured. The literal segment goes
		// back onto the tail: that is exactly what the mirror would have parsed had
		// these patterns not been registered. Writes never fall through -- the
		// mirror has no push API, and an unconfigured backend never mounts one.
		if b.mirror != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			tail := segBuildCache
			if rest := r.PathValue("rest"); rest != "" {
				tail += "/" + rest
			}

			b.mirror.serveTail(w, r, tail)

			return
		}

		notFound(w, codeNameUnknown)

		return
	}

	rest := r.PathValue("rest")

	switch r.Method {
	case http.MethodPost:
		b.serveUploadStart(w, r, route, rest)

	case http.MethodPut:
		b.servePut(w, r, route, rest)

	default:
		// GET, and HEAD via the GET pattern. HEAD is a first-class verb here: it is how
		// containerd decides whether a blob or a tag needs pushing at all.
		b.serveRead(w, r, route, rest)
	}
}

// serveRead answers GET and HEAD, FROM CACHE ONLY.
//
// A tail that names neither a manifest nor a blob is 404 rather than 400, exactly as
// on the mirror: to a client "this registry does not serve that" and "that is not a
// registry URL" are the same event. Two tails land here deliberately:
//
//   - `<repo>/blobs/uploads/...` (splitRef's errPushPath). No client GETs an upload
//     URL; there is no session to report on, because there is no session.
//   - `<repo>/tags/list`. Repository and tag LISTING is out of scope for v1 (spec §9),
//     and 404 is what distribution's own tag store answers for a repository it holds
//     nothing for. Nothing in the BuildKit export or import path asks for it.
func (b *BuildCache) serveRead(w http.ResponseWriter, r *http.Request, route cache.Route, rest string) {
	name, kind, ref, err := splitRef(rest)
	if err != nil {
		notFound(w, codeNameUnknown)

		return
	}

	if _, ok := b.authorize(w, r, route); !ok {
		return
	}

	q := bcRef{route: route, name: name, ref: ref}

	if kind == kindBlobs {
		b.serveCachedBlob(w, r, q)

		return
	}

	b.serveCachedManifest(w, r, q)
}

// serveCachedBlob answers a blob GET/HEAD out of the store and nowhere else.
//
// Range comes free through writeBlob -- BuildKit's import fetches layer blobs lazily
// and honours Range, and these are the largest objects the namespace holds.
func (b *BuildCache) serveCachedBlob(w http.ResponseWriter, r *http.Request, q bcRef) {
	hex, ok := digestHex(q.ref)
	if !ok {
		notFound(w, codeBlobUnknown)

		return
	}

	ref := q.route.Ref(nsBlobs, kindBlob, hex)

	meta, err := b.deps.Blobs.Stat(r.Context(), ref)
	if err != nil {
		b.internal(r.Context(), "stat buildcache blob", err)
		notFound(w, codeBlobUnknown)

		return
	}

	if !meta.Exists {
		// THERE IS NO UPSTREAM. This is the whole miss path.
		notFound(w, codeBlobUnknown)

		return
	}

	b.writeBlob(w, r, ref, meta)
}

// serveCachedManifest answers a manifest GET/HEAD, by digest or by tag.
//
// HEAD BY TAG IS THE HEADLINE OBLIGATION. containerd's push HEADs `manifests/<tag>`
// first and skips the PUT entirely when the response's Docker-Content-Digest EXACTLY
// matches the descriptor it was about to push. Serving that header (plus
// Content-Length and the STORED Content-Type, which writeManifest does together or not
// at all) is what makes a repeat export nearly free; getting it wrong costs a full
// re-push of every cache manifest on every build, silently.
//
// The tag lookup is StatUncached, not Stat: `tags` is the one MUTABLE namespace here
// and it bypasses the in-process LRU in both directions, so a tag repointed by one
// export is visible to the next request rather than to the next process. The BYTES are
// then served out of the immutable `manifests` namespace under the digest the tag row
// named -- the same split the mirror uses, and what keeps the manifest read fully
// cached while the mutable lookup stays honest.
func (b *BuildCache) serveCachedManifest(w http.ResponseWriter, r *http.Request, q bcRef) {
	if isDigestRef(q.ref) {
		hex, ok := digestHex(q.ref)
		if !ok {
			notFound(w, codeManifestUnknown)

			return
		}

		ref := q.route.Ref(nsManifests, kindManifest, hex)

		meta, err := b.deps.Blobs.Stat(r.Context(), ref)
		if err != nil {
			b.internal(r.Context(), "stat buildcache manifest", err)
			notFound(w, codeManifestUnknown)

			return
		}

		if !meta.Exists {
			notFound(w, codeManifestUnknown)

			return
		}

		b.writeManifest(w, r, ref, meta)

		return
	}

	tagRef := q.route.Ref(nsTags, kindTag, buildCacheTagKey(q.name, q.ref))

	meta, err := b.deps.Blobs.StatUncached(r.Context(), tagRef)
	if err != nil {
		b.internal(r.Context(), "stat buildcache tag", err)
		notFound(w, codeManifestUnknown)

		return
	}

	if !meta.Exists {
		// The clean "cache not found" miss. BuildKit's import failure is unconditionally
		// soft -- a debug log and a cold build -- so this is the correct answer for a
		// project that has never exported.
		notFound(w, codeManifestUnknown)

		return
	}

	// THE TOUCH IS LOAD-BEARING FOR GC -- same class as the ac-grpc reachability
	// touch. Exports refresh the tag's updated_at; nothing else records that a cache
	// is still IMPORTED. A cache whose exports stop (the branch goes quiet, or the
	// key is downgraded to read-only and ignore-error=true swallows every export
	// failure silently) but whose imports continue would otherwise age out at W and
	// take its manifests and blobs at 2W -- a live cache going cold on schedule.
	// `tags` bypasses the LRU (StatUncached), so the accessed_at mark machinery
	// cannot see these reads; Touch is the mechanism that can, and it deliberately
	// does not reset the GC write barrier. Monotone: a failed touch costs liveness,
	// never correctness, so it logs and the read proceeds. ErrReadOnly is the one
	// silent skip: it is the deliberate shape of read-only fixtures, and a real
	// deployment always wires a Txer.
	if _, err := b.deps.Blobs.Touch(r.Context(), tagRef); err != nil && !errors.Is(err, blob.ErrReadOnly) {
		b.internal(r.Context(), "touch buildcache tag", err)
	}

	b.writeManifest(w, r, q.route.Ref(nsManifests, kindManifest, meta.Digest.String()), meta)
}

// splitUpload parses a push tail into (repository name, session id).
//
// It scans for the LAST "/blobs/uploads", for the same reason splitRef scans right to
// left: a repository name may legally contain the marker words, and only the rightmost
// occurrence is the real separator.
//
// The remainder must be empty or start with "/", which is what keeps a blob legitimately
// named `uploadsfoo` from parsing as an upload session.
func splitUpload(rest string) (name, session string, ok bool) {
	rest = strings.TrimPrefix(rest, "/")

	const marker = "/" + kindBlobs + "/" + segUploads

	i := strings.LastIndex(rest, marker)
	if i <= 0 { // i == 0 would be an EMPTY repository name
		return "", "", false
	}

	tail := rest[i+len(marker):]
	if tail != "" && tail[0] != '/' {
		return "", "", false
	}

	session = strings.TrimPrefix(tail, "/")
	if strings.Contains(session, "/") {
		return "", "", false
	}

	return rest[:i], session, true
}

// uploadBase returns the request path up to (not including) the /blobs/uploads marker:
// the repository's own URL, in whichever route family the client actually used.
//
// IT IS DERIVED FROM THE REQUEST, not from the mount, which is what makes one handler
// emit a correct Location for both families. containerd resolves a relative Location
// against the host that answered, so returning the client's own prefix means a
// deployment behind a path-rewriting ingress still completes its upload.
func uploadBase(r *http.Request) (string, bool) {
	path := r.URL.EscapedPath()

	i := strings.LastIndex(path, "/"+kindBlobs+"/"+segUploads)
	if i < 0 {
		return "", false
	}

	return path[:i], true
}

// manifestBase returns the request path up to (not including) the /manifests/ marker.
// Same right-to-left scan, same reason, and it is what the manifest PUT's Location is
// built from.
func manifestBase(r *http.Request) (string, bool) {
	path := r.URL.EscapedPath()

	i := strings.LastIndex(path, "/"+kindManifests+"/")
	if i < 0 {
		return "", false
	}

	return path[:i], true
}
