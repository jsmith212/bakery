// Package bazel is the REAPI backend: the four gRPC services Bazel and moon speak
// (Capabilities, ActionCache, ContentAddressableStorage, ByteStream) over a dedicated
// gRPC listener, plus the HTTP /ac and /cas mounts it owns and delegates to httpblob.
//
// # The two things this package must never get wrong
//
// THE AC NAMESPACE SPLIT. The HTTP /ac mount is an OPAQUE byte store (namespace
// "ac"); the gRPC ActionCache is a TYPED store (namespace "ac-grpc"). They must not
// share a namespace: moon computes the SAME action digest over both transports, so
// one namespace makes a workspace that switches api:grpc -> api:http read a protobuf
// where its client demands JSON -- a hard client error, not a miss. GetActionResult
// MUST parse; the opaque mount MUST NOT. The split is what makes both true at once.
//
// ROUTE BEFORE AUTH. Every handler calls authorize() first, which resolves the route
// (NotFound for an unconfigured backend) BEFORE authenticating. This is not an
// interceptor: an interceptor cannot make an unknown backend NotFound to an anonymous
// caller, and it cannot see ByteStream's resource_name (which is in the first frame).
package bazel

import (
	"context"
	"net/http"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/jackc/pgx/v5/pgtype"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// RouteResolver turns the instance_name's {org}/{project} into a resolved cache.Route.
// It is shape-identical to httpblob.RouteResolver and hashserv.RouteResolver on
// purpose: one CachedResolver value satisfies all three, and an /ac HTTP probe warms
// the gRPC route for free.
type RouteResolver interface {
	Resolve(ctx context.Context, org, project string, kind repository.BackendKind) (cache.Route, bool)
}

// Principal is the NARROW capability surface authorize needs. Consumer-side, so
// auth.Principal (sealed, unforgeable) satisfies it structurally and this package
// never imports auth's concrete type. Two questions is the whole truth for a cache
// credential: an API-key principal answers yes to nothing else.
type Principal interface {
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
}

// Authenticator turns the token from a gRPC "authorization" header into a Principal.
// *auth.Service's AuthenticateToken satisfies it (through the widening adapter in the
// server wiring) and runs the same constant-time, zero-join, index-only key probe as
// hashserv and the HTTP cache.
type Authenticator interface {
	AuthenticateToken(ctx context.Context, token string) (Principal, error)
}

// Backend implements cache.GRPCBackend. It embeds the generated Unimplemented servers
// so that every RPC it does not override -- GetTree, the Execution service -- answers
// UNIMPLEMENTED, and so *Backend satisfies all four service interfaces (the ByteStream
// Unimplemented has pointer receivers, which embedding by value promotes onto *Backend).
type Backend struct {
	repb.UnimplementedCapabilitiesServer
	repb.UnimplementedActionCacheServer
	repb.UnimplementedContentAddressableStorageServer
	bspb.UnimplementedByteStreamServer

	deps   cache.Deps
	routes RouteResolver
	authn  Authenticator

	// recs memoizes the headline bakery_cache_requests_total Recorder per (org,
	// project, backend, kind). The ByteStream.Write dedup early-exit bypasses
	// blob.Service.Put -- which is what normally emits op=put on that series -- so it
	// must emit the put/hit outcome itself, exactly as httpblob's /cas SkipIngestIfPresent
	// path does. Without it, gRPC (moon's and Bazel's DEFAULT transport) warm-build
	// dedup traffic records only op=exists and the put hit-rate dashboard reads 0%
	// forever.
	recs *metrics.RecorderCache

	// ac and cas are the HTTP mounts this backend OWNS. They live in httpblob because
	// httpblob.Backend's fields are unexported -- an outside package cannot build one
	// -- and reuse the same 404/401/403 ordering, Range read path, and metrics rules.
	ac  *httpblob.Backend
	cas *httpblob.Backend
}

// Compile-time proof that *Backend is a cache.GRPCBackend and implements all four
// gRPC service interfaces.
var (
	_ cache.GRPCBackend                    = (*Backend)(nil)
	_ repb.CapabilitiesServer              = (*Backend)(nil)
	_ repb.ActionCacheServer               = (*Backend)(nil)
	_ repb.ContentAddressableStorageServer = (*Backend)(nil)
	_ bspb.ByteStreamServer                = (*Backend)(nil)
)

// New builds the bazel backend. It constructs and OWNS the /ac and /cas httpblob
// backends; httpAuthn is the *http.Request-bound authenticator they need, grpcAuth is
// the token authenticator the gRPC handlers need. routes satisfies httpblob's
// RouteResolver too (identical method set), so it is shared.
func New(
	deps cache.Deps,
	routes RouteResolver,
	httpAuthn httpblob.Authenticator,
	grpcAuth Authenticator,
) *Backend {
	return &Backend{
		deps:   deps,
		routes: routes,
		authn:  grpcAuth,
		recs:   metrics.NewRecorderCache(deps.Metrics),
		ac:     httpblob.NewAC(deps, routes, httpAuthn),
		cas:    httpblob.NewCAS(deps, routes, httpAuthn),
	}
}

// recorder returns the memoized headline Recorder for ref. It nil-guards recs so a
// hand-built Backend literal in a test never panics -- matching httpblob.Backend.recorder.
func (b *Backend) recorder(ref blob.Ref) *metrics.Recorder {
	if b.recs == nil {
		return b.deps.Metrics.Recorder(ref.Org, ref.Project, ref.Backend, ref.Kind)
	}

	return b.recs.Get(ref.Org, ref.Project, ref.Backend, ref.Kind)
}

// Kind reports the DB enum this backend serves. One backend per kind per project;
// /ac, /cas, sccache and the gRPC services all share the one bazel cache_backends row.
func (b *Backend) Kind() repository.BackendKind { return repository.BackendKindBazel }

// Register mounts the HTTP routes by DELEGATING to the /ac and /cas backends. The gRPC
// services are mounted separately, via RegisterGRPC.
func (b *Backend) Register(mux *http.ServeMux) {
	b.ac.Register(mux)
	b.cas.Register(mux)
}

// RegisterGRPC registers all four REAPI services on the concrete *grpc.Server. The
// registrar is *grpc.Server, not grpc.ServiceRegistrar, because ByteStream's legacy
// genproto codegen (RegisterByteStreamServer) demands the concrete type; the other
// three take a ServiceRegistrar, which *grpc.Server satisfies. The Execution service
// is deliberately never registered -- grpc-go answers UNIMPLEMENTED for it.
func (b *Backend) RegisterGRPC(srv *grpc.Server) error {
	repb.RegisterCapabilitiesServer(srv, b)
	repb.RegisterActionCacheServer(srv, b)
	repb.RegisterContentAddressableStorageServer(srv, b)
	bspb.RegisterByteStreamServer(srv, b)

	return nil
}
