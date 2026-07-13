package httpblob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
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
	seg    string // "sstate" | "downloads": the LITERAL 4th path segment
	tail   string // "path" | "basename": the PathValue name of the key
	policy Policy
	deps   cache.Deps
	routes RouteResolver
	authn  Authenticator
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
	}
}

// Kind reports the DB enum this backend serves. One backend per kind per project.
func (b *Backend) Kind() repository.BackendKind { return b.kind }

// Register mounts GET, HEAD and PUT on the shared mux.
//
// The 4th segment ("sstate"/"downloads") is a LITERAL, which is the whole reason the
// two backends coexist: a wildcard {kind} there alongside sstate's {path...} panics at
// registration ("neither is more specific"). sstate's tail is {path...} because the
// key contains slashes; downloads' is a single {basename}. Proven not to panic beside
// GET /healthz, GET /readyz, the methodless /api/v1/ and the methodless SPA /.
func (b *Backend) Register(mux *http.ServeMux) {
	pat := "/cache/{org}/{project}/" + b.seg + "/{" + b.tail + "}"
	if b.tail == "path" {
		pat = "/cache/{org}/{project}/" + b.seg + "/{path...}"
	}

	mux.HandleFunc("GET "+pat, b.serve)
	mux.HandleFunc("HEAD "+pat, b.serve)
	mux.HandleFunc("PUT "+pat, b.serve)
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
	case http.MethodHead, http.MethodGet:
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

		b.serveObject(w, r, ref)

	case http.MethodPut:
		// Writes ALWAYS require a key, regardless of ReadAuthRequired: an
		// unauthenticated write path is a cache-poisoning vector, so even an open-read
		// backend rejects an anonymous PUT.
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

		b.servePut(w, r, ref)
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
	res, err := b.deps.Blobs.Put(r.Context(), ref, r.Body, blob.PutOptions{
		Overwrite: b.policy.Overwrite,
		Verify:    b.policy.Verify,
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
