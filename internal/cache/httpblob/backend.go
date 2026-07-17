package httpblob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// WebDAV verbs are not in net/http's method constant set. sccache's opendal backend
// speaks them; PROPFIND is gated as a read, MKCOL as a write.
const (
	methodPropfind = "PROPFIND"
	methodMkcol    = "MKCOL"
)

// Principal is the NARROW capability surface the read/write handler needs. It is a
// consumer-side interface on purpose: auth.Principal (sealed and unforgeable)
// satisfies it structurally, so the handler never imports auth, and a test can drive
// the authorization branches with a fake that answers exactly these two questions.
type Principal interface {
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
}

// Authenticator resolves a cache request's credential to a Principal. *auth.Service's
// AuthenticateCache satisfies it (via a thin adapter that widens auth.Principal to the
// narrow Principal above); the cache mux is mounted OUTSIDE the session middleware, so
// the implementation must be safe on a context that never went through LoadAndSave.
type Authenticator interface {
	AuthenticateCache(ctx context.Context, r *http.Request) (Principal, error)
}

// RouteResolver turns the {org}/{project} wildcards of a cache request into a resolved
// cache.Route -- ResolveRoute + GetBackend, fronted by the in-process route cache the
// boot advisory lock makes sound. It returns ok=false when the org, project or backend
// does not exist (or a lookup failed): the handler renders that as 404, so a missing
// mount looks absent rather than leaking its existence.
type RouteResolver interface {
	Resolve(ctx context.Context, org, project string, kind repository.BackendKind) (cache.Route, bool)
}

// Backend is one HTTP cache backend: the shared handler bound to one Policy, one DB
// kind, and the M1 seams. sstate and downloads are two Backend values differing only
// by kind + Policy + the route shape (path... vs basename).
type Backend struct {
	kind   repository.BackendKind
	seg    string // "sstate" | "downloads" | "ac" | "cas" | "sccache": the LITERAL 4th segment
	tail   string // "path" | "basename" | "key": the PathValue name of the key
	policy Policy
	deps   cache.Deps
	routes RouteResolver
	authn  Authenticator

	// recs memoizes the headline Recorder for THIS backend's one (org, project, backend,
	// kind) tuple. Only the /cas skip-ingest no-op emits metrics from here (every other
	// path routes through blob.Service, which owns the series); memoizing keeps that off
	// WithLabelValues on the redundant-PUT storm. It may be nil on a hand-built Backend --
	// recorder() falls back to Metrics.Recorder, so the skip path never nil-panics.
	recs *metrics.RecorderCache
}

// NewSstate builds the sstate backend. Its key has slashes, so the route tail is
// {path...}.
func NewSstate(deps cache.Deps, routes RouteResolver, authn Authenticator) *Backend {
	return &Backend{
		kind:   repository.BackendKindSstate,
		seg:    "sstate",
		tail:   "path",
		policy: SstatePolicy,
		deps:   deps,
		routes: routes,
		authn:  authn,
		recs:   metrics.NewRecorderCache(deps.Metrics),
	}
}

// NewDownloads builds the downloads (source premirror) backend. Its key is a flat
// basename.
func NewDownloads(deps cache.Deps, routes RouteResolver, authn Authenticator) *Backend {
	return &Backend{
		kind:   repository.BackendKindDownloads,
		seg:    "downloads",
		tail:   "basename",
		policy: DownloadsPolicy,
		deps:   deps,
		routes: routes,
		authn:  authn,
		recs:   metrics.NewRecorderCache(deps.Metrics),
	}
}

// NewAC builds the bazel /ac action-cache backend: an OPAQUE, mutable byte store. ccache
// @layout=bazel, moon api=http and Bazel --remote_cache=http:// all write here under a
// single 64-hex segment, so the route tail is {key}. Kind is bazel; /ac, /cas and sccache
// share ONE cache_backends row (one (org, project, bazel) route), which is also what lets
// an /ac HTTP probe warm the gRPC route for free.
func NewAC(deps cache.Deps, routes RouteResolver, authn Authenticator) *Backend {
	return &Backend{
		kind:   repository.BackendKindBazel,
		seg:    "ac",
		tail:   "key",
		policy: ACPolicy,
		deps:   deps,
		routes: routes,
		authn:  authn,
		recs:   metrics.NewRecorderCache(deps.Metrics),
	}
}

// NewCAS builds the bazel /cas content-addressed backend: key == sha256(body), verified
// per request. Its key is a single 64-hex segment, so the tail is {key}.
func NewCAS(deps cache.Deps, routes RouteResolver, authn Authenticator) *Backend {
	return &Backend{
		kind:   repository.BackendKindBazel,
		seg:    "cas",
		tail:   "key",
		policy: CASPolicy,
		deps:   deps,
		routes: routes,
		authn:  authn,
		recs:   metrics.NewRecorderCache(deps.Metrics),
	}
}

// NewSccache builds the sccache WebDAV backend. sccache shards its key into a/b/c/<hex>
// subdirectories, so the route tail is {path...} (multi-segment) -- NOT a single {key},
// and NOT the /ac mount three shipped docs claimed. Kind is bazel.
func NewSccache(deps cache.Deps, routes RouteResolver, authn Authenticator) *Backend {
	return &Backend{
		kind:   repository.BackendKindBazel,
		seg:    "sccache",
		tail:   "path",
		policy: SccachePolicy,
		deps:   deps,
		routes: routes,
		authn:  authn,
		recs:   metrics.NewRecorderCache(deps.Metrics),
	}
}

// Kind reports the DB enum this backend serves. One backend per kind per project.
func (b *Backend) Kind() repository.BackendKind { return b.kind }

// Register mounts the backend's verbs on the shared mux, policy-driven: GET/HEAD/PUT
// always, DELETE iff AllowDelete, PROPFIND/MKCOL iff WebDAV.
//
// The 4th segment ("sstate"/"downloads"/"ac"/"cas"/"sccache") is a LITERAL, which is the
// whole reason the backends coexist: a wildcard {kind} there alongside sstate's {path...}
// panics at registration ("neither is more specific"). A tail of {path...} is used for
// slash-bearing keys (sstate, sccache's a/b/c/<hex>); ac/cas/downloads are a single
// segment. Proven not to panic beside GET /healthz, GET /readyz, the methodless /api/v1/
// and the methodless SPA /.
//
// The optional verbs are not optional to the clients that need them: an UNregistered
// DELETE returns 405, and ccache treats that as a hard failure that latches its whole
// backend off (reads included); an unregistered PROPFIND/MKCOL 405s opendal, and sccache
// then goes silently read-only. Registering them is what keeps those failures
// unrepresentable.
func (b *Backend) Register(mux *http.ServeMux) {
	pat := "/cache/{org}/{project}/" + b.seg + "/{" + b.tail + "}"
	if b.tail == "path" {
		pat = "/cache/{org}/{project}/" + b.seg + "/{path...}"
	}

	mux.HandleFunc("GET "+pat, b.serve)
	mux.HandleFunc("HEAD "+pat, b.serve)
	mux.HandleFunc("PUT "+pat, b.serve)

	if b.policy.AllowDelete {
		mux.HandleFunc("DELETE "+pat, b.serve)
	}

	if b.policy.WebDAV {
		mux.HandleFunc("PROPFIND "+pat, b.serve)
		mux.HandleFunc("MKCOL "+pat, b.serve)
	}
}

// serve is the whole handler. r.PathValue returns the DECODED key (sstate%3A... arrives
// as sstate:...), which is what the route AND the store agree on -- BitBake's
// wire-encoded HEAD and the CLI's PUT then resolve to one cache_objects.key.
func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	decoded := r.PathValue(b.tail)

	route, ok := b.routes.Resolve(r.Context(), r.PathValue("org"), r.PathValue("project"), b.kind)
	if !ok || !route.Enabled {
		// Unknown org/project/backend or a disabled backend: the mount looks absent.
		// Never a 5xx, never a hint.
		http.NotFound(w, r)

		return
	}

	kind, key, err := b.policy.Classify(decoded)
	if err != nil {
		http.Error(w, "bad object key", http.StatusBadRequest) // 400 -- traversal/bad grammar

		return
	}

	ref := route.Ref(b.policy.Namespace, kind, key) // the ONLY sanctioned Ref constructor

	switch r.Method {
	case http.MethodHead, http.MethodGet, methodPropfind:
		// PROPFIND is a READ: opendal probes the parent collection before every sccache
		// write, and it must clear the same read gate as GET/HEAD so an auth-required
		// mount challenges it identically.
		if route.ReadAuthRequired {
			// Reads collapse unauthenticated AND valid-but-unauthorized to 401 -- NEVER
			// 403. BitBake retries a 403 as a full-body GET (turning the HEAD storm into
			// a GET storm), and 403 is a project-existence oracle. The short-circuit is
			// load-bearing: on err != nil, p is nil and must not be dereferenced.
			p, aerr := b.authn.AuthenticateCache(r.Context(), r)
			if aerr != nil || !p.CanReadProject(route.OrgID, route.ProjectID) {
				w.Header().Set("WWW-Authenticate", `Basic realm="bakery"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized) // 401

				return
			}
		}

		if r.Method == methodPropfind {
			b.servePropfind(w, r)

			return
		}

		b.serveObject(w, r, ref)

	case http.MethodPut, http.MethodDelete, methodMkcol:
		// Writes ALWAYS require a key, regardless of ReadAuthRequired: an
		// unauthenticated write path is a cache-poisoning vector, so even an open-read
		// backend rejects an anonymous PUT. MKCOL and DELETE are writes too and share
		// this gate.
		//
		// Auth/authz/grammar all run BEFORE the first read of r.Body (Classify above,
		// authenticate+authorize here). Go's server emits 100-continue and starts
		// draining the body only on that first read, so an early 401/403/400 makes a
		// well-behaved client abort a multi-GB upload instead of the server swallowing
		// it.
		p, aerr := b.authn.AuthenticateCache(r.Context(), r)
		if aerr != nil {
			// No/invalid credential -> 401. Same challenge the read path sends, so a
			// client that netrc-authenticates a read authenticates a write identically.
			w.Header().Set("WWW-Authenticate", `Basic realm="bakery"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized) // 401

			return
		}

		if !p.CanWriteProject(route.OrgID, route.ProjectID) {
			// Authenticated but not write-authorized (a read-scoped key, a reader role).
			// 403 is CORRECT here and safe: the writer is the CLI, which never falls
			// back to a GET on a 403, the caller named its own project (no oracle), and
			// BitBake never issues a PUT at all.
			http.Error(w, "forbidden", http.StatusForbidden) // 403

			return
		}

		switch r.Method {
		case http.MethodDelete:
			b.serveDelete(w, r, ref)
		case methodMkcol:
			b.serveMkcol(w, r)
		default:
			b.servePut(w, r, ref)
		}
	}
}

// servePut streams the request body through blob.Service.Put. Dedup, refcount and the
// bytes-before-metadata ordering all come for free; the handler never touches storage
// or the metadata tables directly (Deps carries no *repository.Queries), and it never
// re-emits a metric -- blob.Service already emits bakery_cache_requests_total{op=put}.
//
// r.Body is passed straight to Put, which io.Copy's it into a staging file and hashes
// it as it goes: the multi-GB tarball is NEVER buffered whole in memory. There is no
// MaxBytesReader ceiling here -- sstate objects are multi-GB; any cap must be a config
// knob, not a hard-coded small limit.
func (b *Backend) servePut(w http.ResponseWriter, r *http.Request, ref blob.Ref) {
	verify := b.policy.Verify

	if b.policy.VerifyFromKey != nil {
		// /cas derives the body policy from THIS request's key: the digest the body must
		// hash to. A malformed key is a client error -> 400, never a 500 -- and it runs
		// before the body is read, so the client aborts its upload rather than the server
		// swallowing a multi-GB body it will reject.
		v, err := b.policy.VerifyFromKey(ref.Key)
		if err != nil {
			http.Error(w, "bad object key", http.StatusBadRequest) // 400

			return
		}

		verify = v

		// httpblob hashes the WIRE bytes. A Content-Encoding other than identity (a zstd
		// body -- bazel-remote's extension) would hash the COMPRESSED bytes and fail
		// VerifyDigest on legitimate traffic, surfacing as a misleading "digest mismatch".
		// Reject it explicitly, and ONLY on the verifying path: /ac is opaque and stores
		// whatever encoding the client sent, verbatim.
		if enc := r.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
			http.Error(w, "unsupported Content-Encoding on a verified upload", http.StatusBadRequest) // 400

			return
		}
	}

	if b.policy.SkipIngestIfPresent {
		// /cas ONLY. An existing key provably names byte-identical content (Overwrite=no +
		// digest-verified), so a redundant PUT is a semantic no-op. Probe Stat (LRU-backed,
		// zero queries warm) and skip the staged write + fsync + sha256 + the whole PG
		// transaction + the blobs-row rewrite + the self-eviction the full path would pay
		// on the redundant-PUT storm.
		meta, err := b.deps.Blobs.Stat(r.Context(), ref)
		if err == nil && meta.Exists {
			// KEEP THE DRAIN. An early 200 without draining turns a naive client's
			// in-flight upload into an EPIPE: the spike proved the client then holds BOTH a
			// 200 and a write error and may retry, bringing the storm back with extra steps.
			// Skip the drain ONLY when the client asked to be told first
			// (Expect: 100-continue), where Go answers with the final status and the body
			// is never sent.
			if !expects100Continue(r) {
				_, _ = io.Copy(io.Discard, r.Body)
			}

			// This path never enters blob.Service.Put, so it must emit the dedup outcome
			// itself -- otherwise the put/hit series and the bytes counter silently vanish
			// for exactly the redundant-PUT storm they exist to measure. Identical labels
			// to the full path (blob.Service also records put/hit on a dedup).
			rec := b.recorder(ref)
			rec.Observe(metrics.OpPut, metrics.ResultHit)
			rec.AddBytes(metrics.OpPut, meta.Size)

			w.WriteHeader(http.StatusOK) // idempotent no-op, identical to the full path's 200

			return
		}
	}

	res, err := b.deps.Blobs.Put(r.Context(), ref, r.Body, blob.PutOptions{
		Overwrite: b.policy.Overwrite,
		Verify:    verify,
	})

	switch {
	case errors.Is(err, blob.ErrInvalidKey):
		http.Error(w, "bad object key", http.StatusBadRequest) // 400
	case errors.Is(err, blob.ErrDigestMismatch):
		// Cannot occur for the M2 NoVerify policies, but map it truthfully so a future
		// VerifyDigest policy reusing this handler renders a bad body as 400, not 500.
		http.Error(w, "digest mismatch", http.StatusBadRequest) // 400
	case err != nil:
		http.Error(w, "put", http.StatusInternalServerError) // 500
	case res.Created:
		// This key now names bytes for the first time. (Deduped may be either: the
		// bytes may already have been durably present under this digest.)
		w.WriteHeader(http.StatusCreated) // 201
	default:
		// res.Created == false: the immutable key already existed. ON CONFLICT DO
		// NOTHING left the pre-existing row standing -- an idempotent no-op, never a
		// content swap and NEVER a 409. This is the common re-push case and the
		// HEAD/PUT race where another pusher won.
		w.WriteHeader(http.StatusOK) // 200
	}
}

// recorder returns the memoized headline Recorder for this backend's tuple. It is used
// ONLY by the /cas skip-ingest no-op; the nil guard keeps a hand-built Backend (whose
// recs is nil, and which never hits the skip path) from panicking.
func (b *Backend) recorder(ref blob.Ref) *metrics.Recorder {
	if b.recs == nil {
		return b.deps.Metrics.Recorder(ref.Org, ref.Project, ref.Backend, ref.Kind)
	}

	return b.recs.Get(ref.Org, ref.Project, ref.Backend, ref.Kind)
}

// expects100Continue reports whether the client asked to be told the final status before
// sending the body. Go answers a 100-continue expectation with the final status, so on
// that path the body is never sent and there is nothing to drain.
func expects100Continue(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Expect"), "100-continue")
}

// serveObject answers GET and HEAD for one resolved object.
//
// HEAD is the hot path and is answered from Stat -- metadata only, it NEVER opens the
// bytes. GET type-asserts the reader up to io.ReadSeeker (true for the local store: a
// *os.File survives unwrapped through Instrumented.Get and Service.Get) and hands it to
// http.ServeContent, which gives Range/206, unsatisfiable/416, If-Range and HEAD for
// free. The non-seekable fallback is kept for the deferred S3 store, whose Get body is
// not a Seeker.
func (b *Backend) serveObject(w http.ResponseWriter, r *http.Request, ref blob.Ref) {
	if r.Method == http.MethodHead {
		meta, err := b.deps.Blobs.Stat(r.Context(), ref) // metadata-only; no store.Get
		if err != nil {
			http.Error(w, "stat", http.StatusInternalServerError)

			return
		}

		if !meta.Exists {
			http.NotFound(w, r) // miss -> 404, NEVER 403, never a 200 with an empty body

			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK) // empty body -- cheap, no bytes opened

		return
	}

	meta, rc, err := b.deps.Blobs.Get(r.Context(), ref)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			http.NotFound(w, r) // 404

			return
		}

		// ErrDanglingMetadata and any other error are a LOUD 500 -- never folded into
		// the 404 path, or a build would silently rebuild from a corrupted cache forever.
		http.Error(w, "get", http.StatusInternalServerError)

		return
	}

	defer func() { _ = rc.Close() }()

	// Set Content-Type explicitly so ServeContent skips its 512-byte sniff read+seek.
	w.Header().Set("Content-Type", "application/octet-stream")

	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", meta.UpdatedAt, rs) // Range/206/416/If-Range for free

		return
	}

	// Non-seekable fallback (S3 deferred; its Get is not a Seeker): full 200, no Range.
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
