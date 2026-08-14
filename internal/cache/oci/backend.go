// Package oci is the Docker/OCI pull-through proxy: a read-only registry mirror that
// serves manifests, tags and blobs from Bakery's own blob store and fetches what it
// does not have from an allowlisted upstream registry.
//
// # The four things this package must never get wrong
//
// NEVER RE-SERIALIZE A MANIFEST. A manifest's digest is the sha256 of its exact bytes,
// so a json.Marshal round trip -- which reorders keys and rewrites whitespace --
// changes the digest and breaks Docker-Content-Digest for every client at once. It
// reproduces only on multi-arch index manifests, i.e. not in your test and yes in
// production. It is prevented STRUCTURALLY here: manifest bytes go from the upstream
// response into blob.Service.Put with a VerifyDigest computed over those same bytes,
// and are served back byte-for-byte. Nothing in this package parses a manifest.
//
// THE STORED DIGEST IS OURS. The key a manifest is stored under is the sha256 WE
// computed over the bytes WE received -- never the upstream's Docker-Content-Digest
// header. go-containerregistry does not verify that header for tag fetches (its own
// source says so: too many registries get it wrong), so trusting it would let one
// broken upstream response store bytes under a digest they do not hash to, and every
// client's content-store verification would then hard-fail on every pull of that
// image, permanently.
//
// AN UPSTREAM FETCH REQUIRES A VERIFIED PRINCIPAL. Every Fetcher method takes an
// auth.Principal and rejects nil at entry. Without that, an anonymous request that
// misses is a fetch made on the operator's credentials and rate limit -- an open relay
// serving Docker Hub to the internet, indistinguishable from a busy cache.
//
// EVERY FAILURE IS SILENT AT THE CLIENT. containerd, BuildKit, podman and Docker Engine
// all fall back to the real registry on ANY mirror failure -- a 404, a 500, a bad
// challenge, a timeout. So a completely broken Bakery produces green builds, zero
// complaints, and a 0% hit rate. Nothing in this package may rely on a client
// reporting a problem; the metrics are the only witness there is.
package oci

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The cache_objects namespaces this backend owns, and their metrics `kind` labels.
//
// They are three separate namespaces in the PRIMARY KEY for the same reason /ac and
// /cas are: a manifest digest and a blob digest are both 64 hex, and `tags` is MUTABLE
// while the other two are immutable and digest-verified. One namespace would let a tag
// write repoint a verified manifest at unrelated content.
const (
	nsManifests = "manifests"
	nsBlobs     = "blobs"
	nsTags      = "tags"

	kindManifest = "manifest"
	kindBlob     = "blob"
	kindTag      = "tag"
)

// RouteResolver turns the {org}/{project} wildcards into a resolved cache.Route. It is
// shape-identical to httpblob's and bazel's, so one CachedResolver value serves all of
// them.
type RouteResolver interface {
	Resolve(ctx context.Context, org, project string, kind repository.BackendKind) (cache.Route, bool)
}

// Backend implements cache.Backend: the OCI pull-through proxy.
//
// There is ONE Backend VALUE for the whole server -- exactly like the bazel backend --
// and it serves every project, resolving the tenant per request. That matters here
// because two of its routes are GLOBAL rather than per-tenant (the bare `GET /v2/`
// ping that podman requires, and `GET|POST /v2/token`), and registering a global
// pattern twice panics the mux at startup.
type Backend struct {
	deps   cache.Deps
	routes RouteResolver
	authn  Authenticator
	up     Fetcher
	cfg    Config

	// sf collapses concurrent upstream work. A hundred nodes starting the same
	// deployment pull the same layer at the same instant; without this that is a
	// hundred identical upstream downloads, each of which ends up discarded by dedup
	// anyway. Keyed by (backend, namespace, key), so two projects mirroring the same
	// image do not share a flight -- they have different credentials and different
	// allowlists.
	sf singleflight.Group

	// now is injectable so a test can advance past a tag TTL without sleeping.
	now func() time.Time

	// warnRealmOnce rate-limits the EXTERNAL_URL-unset warning to one line per process:
	// the fallback fires on every token dance, and a warning per pull is noise nobody
	// reads while a single line at first use is a config nudge somebody might.
	warnRealmOnce sync.Once

	// refreshHook, when non-nil, is called with the result of every completed
	// background tag refresh. Production leaves it nil; tests use it to join the
	// refresh goroutine deterministically instead of sleeping and hoping.
	refreshHook func(result string)

	// registered guards the GLOBAL routes. Register is called once per backend VALUE
	// and there is one value, so this never fires in production -- it exists so that a
	// future wiring change that constructs two OCI backends fails as a duplicate mount
	// rather than as a panic inside net/http at startup.
	registered bool
}

var _ cache.Backend = (*Backend)(nil)

// New builds the OCI backend. up is the upstream client (NewRegistry in production, a
// fake in tests); cfg carries the server-level configuration that must not live in the
// database.
func New(deps cache.Deps, routes RouteResolver, authn Authenticator, up Fetcher, cfg Config) *Backend {
	return &Backend{
		deps: deps, routes: routes, authn: authn, up: up, cfg: cfg,
		sf: singleflight.Group{}, now: time.Now, refreshHook: nil, registered: false,
	}
}

// Kind reports the DB enum this backend serves.
func (b *Backend) Kind() repository.BackendKind { return repository.BackendKindOci }

// Register mounts both route families, both pings, and both token endpoints.
//
// TWO FAMILIES, ONE HANDLER SET, and it is not an accident of taste -- it is what the
// four clients actually do:
//
//   - /cache/{org}/{project}/docker/v2/... is containerd (whose hosts.toml parser
//     appends /v2 to any configured host or server URL that does not already end in
//     it) and Docker Engine (whose ValidateMirror accepts a path, contrary to three
//     shipped docs).
//   - /v2/{org}/{project}/... is BuildKit (which joins its mirror prefix AFTER /v2)
//     and podman/skopeo/CRI-O (which rewrite the reference, so the tenant prefix lands
//     in the repository position).
//
// Normalizing everyone onto one shape via containerd's `override_path` would fix
// containerd only, would not help Docker Engine at all, and would push the burden onto
// per-operator config discipline. Two patterns into the same handlers is ten lines.
//
// EVERY PATTERN CARRIES AN EXPLICIT METHOD, AND NONE OF THEM IS `HEAD`. Both halves
// are forced by net/http, and the second one cost a startup panic to learn:
//
//   - A METHOD-LESS pattern here panics. `/v2/{org}/{project}/` registered beside the
//     SPA's `GET /` catch-all is the documented ServeMux trap.
//   - A `GET` pattern in Go's ServeMux ALSO MATCHES HEAD. So registering `HEAD <pat>`
//     beside `GET <pat>` is not harmless redundancy -- it makes `GET <literal>` (which
//     matches {GET, HEAD}) overlap `HEAD <wildcard>` (which matches {HEAD}) with a more
//     specific path and a larger method set, and ServeMux calls that a conflict and
//     PANICS AT STARTUP. HEAD is a first-class verb in the registry protocol -- it is
//     how containerd's digest fast path and every blob existence check work -- and it is
//     handled explicitly INSIDE the handlers, on r.Method.
func (b *Backend) Register(mux *http.ServeMux) {
	if b.registered {
		panic("oci: Register called twice; the global /v2/ ping and /v2/token can only be mounted once")
	}

	b.registered = true

	for _, pat := range []string{
		"/cache/{org}/{project}/docker/v2/{rest...}",
		"/v2/{org}/{project}/{rest...}",
	} {
		mux.HandleFunc("GET "+pat, b.serve) // also matches HEAD; see above
	}

	// The GLOBAL ping, at the bare host root and outside any tenant prefix.
	// containers/image pings the HOST ROOT -- not the mirror path -- and hard-errors on
	// anything but 200 or 401, so without this route podman, skopeo and CRI-O cannot
	// use Bakery at all. It beats server.go's `/v2/` NotFoundHandler because `{$}` is
	// the more specific pattern.
	mux.HandleFunc("GET /v2/{$}", b.serveGlobalPing)

	// The token endpoints. GET AND POST on both: containerd sends an OAuth2 form POST
	// first when it holds a secret, and a mux auto-405 on that hard-fails an
	// identitytoken credential rather than falling back to GET. See serveToken.
	for _, pat := range []string{
		"/cache/{org}/{project}/docker/v2/token",
		"/v2/token",
	} {
		mux.HandleFunc("GET "+pat, b.serveToken)
		mux.HandleFunc("POST "+pat, b.serveToken)
	}
}

// serveGlobalPing answers the tenant-less `GET /v2/` that containers/image requires.
//
// IT EMITS THE BEARER CHALLENGE ON A 200. See challenge() -- containers/image harvests
// challenges from the ping response and nowhere else, and a bare 200 permanently
// convinces it that this registry needs no authentication.
func (b *Backend) serveGlobalPing(w http.ResponseWriter, r *http.Request) {
	b.writePing(w, r, cache.Route{})
}

// serveTenantPing answers the per-tenant ping -- the empty tail of either route family
// (/cache/{org}/{project}/docker/v2/ and /v2/{org}/{project}/). Docker Engine's
// PingV2Registry includes the mirror's path, so this is the one it reaches.
//
// IT IS DISPATCHED FROM INSIDE serve RATHER THAN REGISTERED AS ITS OWN `{$}` PATTERN,
// and that is forced, not stylistic: `GET .../v2/{$}` registered beside
// `HEAD .../v2/{rest...}` PANICS the mux at startup, because {rest...} matches the
// empty tail too and neither pattern is more specific than the other across differing
// method sets. Dispatching on an empty tail is the same routing decision with no
// registration hazard. (`GET .../v2/{$}` beside `GET .../v2/{rest...}` would in fact
// be legal -- {$} is strictly more specific -- but dispatching on an empty tail is one
// pattern instead of two and puts the decision next to the parsing it belongs with.)
//
// It does NOT resolve the route, and that is deliberate: the ping is a protocol
// handshake, not a resource, and 404ing it for an unconfigured project would make a
// client conclude Bakery is not a registry at all rather than that this project has no
// mirror. The subsequent manifest request 404s on its own, which is the answer that
// makes the client fall back cleanly.
func (b *Backend) serveTenantPing(w http.ResponseWriter, r *http.Request) {
	b.writePing(w, r, cache.Route{Org: r.PathValue("org"), Project: r.PathValue("project")})
}

// writePing is the shared ping body: 200, the API version header, and the challenge.
//
// Docker-Distribution-API-Version is enforced by none of the four clients, but it is
// what a human curling the endpoint looks for and it costs one header.
func (b *Backend) writePing(w http.ResponseWriter, r *http.Request, route cache.Route) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	b.challenge(w, r, route)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("{}"))
	}
}

// request is one fully-resolved registry request: the tenant, the parsed reference,
// the upstream it may dial, and the identity behind it.
type request struct {
	route     cache.Route
	policy    policy
	name      string // repository name, e.g. library/alpine
	ref       string // tag or sha256:<hex>
	upstream  string // normalized, allowlisted upstream host
	principal Principal
}

// upstreamRef is the repository at the upstream this request proxies.
func (q request) upstreamRef() UpstreamRef {
	return UpstreamRef{Host: q.upstream, Name: q.name}
}

// serve is the whole read path, and its ORDER is the contract.
//
//  0. An EMPTY tail is the tenant ping, not a resource. See serveTenantPing.
//  1. Resolve the route. An unknown or disabled backend is 404 to everyone, before any
//     authentication happens -- otherwise a 401 tells a scanner which projects exist.
//  2. Parse the reference. A tail that is not <name>/manifests|blobs/<ref> is 404, not
//     400: to a client the two are the same event.
//  3. Resolve ?ns= against the allowlist. THIS IS THE SSRF GATE and it runs before any
//     credential is even looked at, so an unlisted host costs nothing and reveals
//     nothing.
//  4. Authenticate. May be anonymous on an open backend.
//  5. Dispatch.
func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("rest") == "" {
		b.serveTenantPing(w, r)

		return
	}

	route, ok := b.routes.Resolve(r.Context(), r.PathValue("org"), r.PathValue("project"), b.Kind())
	if !ok || !route.Enabled {
		notFound(w, codeNameUnknown)

		return
	}

	name, kind, ref, err := splitRef(r.PathValue("rest"))
	if err != nil {
		if errors.Is(err, errPushPath) {
			// Honest rather than confusing: there is no push API, and a client that
			// tried one should be told so rather than shown a miss it will retry.
			writeError(w, http.StatusNotFound, codeUnsupported, "this registry is pull-through only")

			return
		}

		notFound(w, codeNameUnknown)

		return
	}

	pol, perr := parsePolicy(route.Config)
	if perr != nil {
		// A bad config blob must not take the mirror down, and parsePolicy fails
		// CLOSED (docker.io only, 10 minutes), so serving on the defaults is safe. It
		// is logged because an operator typo that silently narrows an allowlist is
		// otherwise invisible.
		b.deps.Logger.WarnContext(r.Context(), "oci: bad backend config",
			slog.String("org", route.Org), slog.String("project", route.Project),
			slog.Any("error", perr))
	}

	upstream, ok := pol.resolveUpstream(r.URL.Query().Get("ns"))
	if !ok {
		notFound(w, codeNameUnknown)

		return
	}

	principal, ok := b.authorize(w, r, route)
	if !ok {
		return
	}

	req := request{
		route: route, policy: pol, name: name, ref: ref,
		upstream: upstream, principal: principal,
	}

	if kind == kindBlobs {
		b.serveBlob(w, r, req)

		return
	}

	b.serveManifest(w, r, req)
}

// ref builds the blob.Ref for one object on this request's route.
//
// Route.Ref is the ONLY sanctioned blob.Ref constructor, and going through it is what
// pins the metrics labels to RESOLVED SLUGS and a constant backend/kind. Labelling on
// anything per-image -- a repository name, a tag, a digest -- would mint one Prometheus
// time series per image pulled, which is the OCI twin of labelling sstate metrics on
// r.URL.Path and kills the scrape just as fast.
func (q request) objectRef(namespace, kind, key string) blob.Ref {
	return q.route.Ref(namespace, kind, key)
}
