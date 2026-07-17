// Package cache defines the contracts every cache backend implements.
//
// M1 ships NO backends: sstate and downloads are M2, hashserv M3, bazel M4, OCI M5.
// What lands here now is the shape they plug into, and one structural rule.
//
// # THE RULE: Deps does not carry a *repository.Queries
//
// It is not an oversight and it is not a style preference. It is the mechanism that
// enforces "blob.Service is the only writer of object metadata". A backend that
// could reach a *repository.Queries could insert a cache_objects row without ever
// having written the bytes -- dangling metadata, a permanent 500 -- or could delete
// a blobs row without the digest advisory lock, resurrecting the race the
// pending_delete tombstone exists to close. Neither of those is a bug that a code
// review reliably catches, and both are bugs that only reproduce under load.
//
// So the type is simply not reachable from a backend. The rule is enforced by the
// compiler and by TestDepsCarriesNoQueries, not by discipline.
package cache

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Route is a resolved mount: one {org}/{project}/{kind} triple that the router has
// already turned into DATABASE ROWS.
//
// "Already turned into database rows" is the load-bearing part. The slugs on a Route
// came back from ResolveRoute, so they are safe to use as Prometheus labels; the raw
// {org}/{project} path segments in an http.Request are attacker-controlled and would
// mint one time series per garbage slug. Backends label from a Route, never from a
// URL.
type Route struct {
	OrgID     pgtype.UUID
	ProjectID pgtype.UUID

	// Org and Project are the resolved slugs. Metrics labels, and nothing else.
	Org     string
	Project string

	// BackendID is cache_backends.id -- the primary-key prefix of every hot-path
	// probe, which is why the HEAD path never joins to projects.
	BackendID int64
	Kind      repository.BackendKind

	// Enabled and ReadAuthRequired come from the backend's row. Writes ALWAYS require
	// a key regardless of ReadAuthRequired: an unauthenticated write path is a
	// cache-poisoning vector.
	Enabled          bool
	ReadAuthRequired bool

	// Config is the backend's jsonb blob, parsed by the backend that owns its schema.
	Config []byte
}

// Ref builds the blob.Ref for one object on this route.
//
// This is the ONLY sanctioned way to construct a blob.Ref, and the reason is the
// metrics contract: it is what guarantees the org/project labels are resolved slugs
// and the backend/kind labels are constants, so bakery_cache_requests_total stays
// bounded by (orgs x projects x 7 legal (backend,kind) pairs) instead of by the
// number of sstate objects in the cache.
//
//   - namespace is the cache_objects discriminator (” for sstate and downloads,
//     'ac' / 'cas' for bazel, 'blobs' / 'manifests' for OCI). It is part of the
//     PRIMARY KEY: without it /ac/<64hex> and /cas/<64hex> collide, and a ccache
//     write to /ac/ silently repoints a verified CAS blob at unrelated content.
//   - kind is the METRICS sub-namespace (object|siginfo|file|ac|cas|blob|manifest).
//     It is not always equal to namespace -- sstate's namespace is ” but its kinds
//     are object and siginfo.
func (r Route) Ref(namespace, kind, key string) blob.Ref {
	return blob.Ref{
		BackendID: r.BackendID,
		Org:       r.Org,
		Project:   r.Project,
		Backend:   backendLabel(r.Kind),
		Kind:      kind,
		Namespace: namespace,
		Key:       key,
	}
}

// backendLabel maps the DB enum onto the metrics constant. Two closed sets, one
// mapping, no strings built at a call site.
func backendLabel(k repository.BackendKind) metrics.Backend {
	switch k {
	case repository.BackendKindSstate:
		return metrics.BackendSstate
	case repository.BackendKindDownloads:
		return metrics.BackendDownloads
	case repository.BackendKindHashserv:
		return metrics.BackendHashserv
	case repository.BackendKindBazel:
		return metrics.BackendBazel
	case repository.BackendKindOci:
		return metrics.BackendOCI
	default:
		// Unreachable: BackendKind is a Postgres enum and the column is NOT NULL.
		// Returning a constant rather than string(k) keeps the label space closed
		// even if a future migration adds a kind this binary does not know.
		return metrics.Backend("unknown")
	}
}

// Deps is everything a backend is allowed to have.
//
// Read the package doc for what is DELIBERATELY ABSENT. Adding a *repository.Queries
// here, or anything that transitively exposes one, breaks the invariant that makes
// the storage-ordering rules enforceable -- and TestDepsCarriesNoQueries will fail.
type Deps struct {
	// Blobs is the ONLY door to object metadata and to the bytes. Dedup, refcounts,
	// the LRU, singleflight and the headline metrics all live behind it, which is why
	// every backend gets them for free and none of them can get them wrong.
	Blobs *blob.Service

	// Metrics is for series a backend owns that blob.Service cannot emit -- hashserv's
	// bakery_hashserv_*, for one, since hashserv does not route through blob at all.
	// The headline cache series are NOT emitted here; blob.Service owns them.
	Metrics *metrics.Metrics

	Logger *slog.Logger
}

// Validate is called at wiring time so a missing dependency is a startup error rather
// than a nil dereference on the first request of a build.
func (d Deps) Validate() error {
	switch {
	case d.Blobs == nil:
		return errors.New("cache: Deps.Blobs is required")
	case d.Metrics == nil:
		return errors.New("cache: Deps.Metrics is required")
	case d.Logger == nil:
		return errors.New("cache: Deps.Logger is required")
	}

	return nil
}

// Backend is an HTTP cache backend: sstate, downloads, bazel's /ac and /cas, and the
// OCI pull-through proxy.
//
// Register mounts the backend's routes on the shared mux. It registers PATTERNS
// (Go 1.22 method+pattern routing) and the routes carry {org}/{project} wildcards;
// the backend resolves those to a Route per request. Two registration hazards, both
// of which PANIC AT STARTUP rather than misroute -- which is the good outcome, and
// worth knowing about before you hit them:
//
//   - `/cache/{org}/{project}/{kind}/{key}` alongside
//     `/cache/{org}/{project}/sstate/{path...}` panics ("neither is more specific").
//     Register `ac` and `cas` as LITERAL segments.
//   - a method-less `/v2/{org}/{project}/` alongside `GET /` (the SPA catch-all)
//     panics too. Enumerate GET and HEAD explicitly on the OCI routes.
//
// HEAD IS THE HOT PATH, not GET: BitBake fires a BB_NUMBER_THREADS-parallel HEAD
// storm over the whole setscene graph at the start of every build. Register it
// explicitly and answer it from blob.Service.Exists / .Stat -- never by faking it
// with a GET whose body is discarded.
type Backend interface {
	// Kind is the DB enum this backend serves. One backend per kind per project.
	Kind() repository.BackendKind

	// Register mounts the backend's HTTP routes.
	Register(mux *http.ServeMux)
}

// GRPCBackend is a Backend that ALSO speaks gRPC: bazel's REAPI (M4), where moon
// defaults to gRPC and its HTTP mode is degraded (no FindMissingBlobs, so it
// re-uploads every blob every build) -- gRPC is not optional there.
//
// The registrar is the CONCRETE *grpc.Server, NOT grpc.ServiceRegistrar. REAPI's
// three services (Capabilities, ActionCache, ContentAddressableStorage) come from
// protoc-gen-go-grpc v1.5.1 and register onto a grpc.ServiceRegistrar, which
// *grpc.Server satisfies -- but ByteStream is generated by an OLD genproto codegen
// (SupportPackageIsVersion6) whose RegisterByteStreamServer takes *grpc.Server
// outright. Narrowing this to grpc.ServiceRegistrar does not compile. The concrete
// type registers all four services; the interface would register only three.
//
// Note: the project comes from the REAPI instance_name, which is "{org}/{project}"
// -- so it CONTAINS SLASHES. ByteStream resource names must be parsed by scanning
// for the blobs / uploads / compressed-blobs marker, never split positionally on
// '/'.
type GRPCBackend interface {
	Backend

	RegisterGRPC(srv *grpc.Server) error
}

// StreamBackend is a Backend whose connection is LONG-LIVED and BIDIRECTIONAL rather
// than request/response: hashserv (M3), which upgrades to a websocket.
//
// It is a separate interface because such a connection must be exempted from the
// things that are correct for every other route and fatal here -- a write timeout, a
// response-buffering middleware, a body-size limit -- and because of the protocol's
// own invariant: hashserv has NO REQUEST IDS, so responses must be strictly ordered
// and there must be exactly ONE WRITER GOROUTINE PER CONNECTION. A single reordered
// or dropped response desynchronizes the connection permanently and silently, and
// bitbake then hangs forever with no error. If two goroutines can write to the
// connection, the design is already wrong.
type StreamBackend interface {
	Backend

	// ServeStream takes over the connection for its lifetime.
	ServeStream(w http.ResponseWriter, r *http.Request, route Route)
}
