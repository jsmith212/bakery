package bazel

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/jsmith212/bakery/internal/blob"
)

// The error-to-code mapping is the whole conformance surface for a client that never
// reaches the happy path, and two of the codes are silent-catastrophe triggers:
//
//   - GetActionResult MISS must be NotFound and NOTHING ELSE. Bazel maps NOT_FOUND
//     to a clean miss and EVERY other status to a build-failing IOException.
//   - Route absent/disabled/instance-not-two-segments is NotFound too, so an
//     unconfigured backend is indistinguishable from "no such project" -- a
//     non-oracle, and it holds for GetCapabilities as well.
//
// authorize() returns fully-formed gRPC status errors (NotFound / Unauthenticated /
// PermissionDenied), so a handler returns them verbatim. grpcError maps the OTHER
// errors a handler can hit -- blob sentinels and the resource sentinels -- and folds
// everything unrecognized into Internal, LOGGED, never into Unauthenticated.

// errSizeMismatch means the committed byte count disagreed with the size the client
// declared in the resource name / digest. InvalidArgument.
var errSizeMismatch = errors.New("bazel: committed size does not match the declared size")

// grpcError maps a handler-side error onto a gRPC status. It never returns nil for a
// non-nil error.
func (b *Backend) grpcError(ctx context.Context, method string, err error) error {
	switch {
	case errors.Is(err, errCompressedResource):
		return grpcstatus.Error(codes.Unimplemented, "compressed-blobs is not supported; server advertises IDENTITY only")
	case errors.Is(err, errInvalidResource):
		return grpcstatus.Error(codes.InvalidArgument, "unparseable resource name")
	case errors.Is(err, errSizeMismatch):
		return grpcstatus.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, blob.ErrDigestMismatch):
		return grpcstatus.Error(codes.InvalidArgument, "content does not match the declared digest")
	case errors.Is(err, blob.ErrInvalidKey):
		return grpcstatus.Error(codes.InvalidArgument, "invalid digest")
	case errors.Is(err, context.Canceled):
		// The client cancelled -- routine in a parallel build (a local action already
		// satisfied the dependency, or the build finished). NOT a server fault: map it
		// to Canceled and do NOT log at ERROR, or a healthy server spams the error log
		// and inflates any alert keyed on gRPC Internal / error-log rate.
		return grpcstatus.Error(codes.Canceled, "cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return grpcstatus.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		// Internal, and it must be loud: a bug that folds into Unauthenticated or
		// NotFound would look like an auth or cache-miss problem forever.
		b.deps.Logger.ErrorContext(ctx, "bazel: internal error",
			slog.String("method", method), slog.Any("error", err))

		return grpcstatus.Error(codes.Internal, "internal error")
	}
}

// statusPB is the genproto rpc/status message carried inside BatchReadBlobs /
// BatchUpdateBlobs per-blob responses. Aliased so call sites do not import genproto
// directly.
type statusPB = status.Status

// statusProto builds the per-blob *status.Status carried inside a BatchReadBlobs /
// BatchUpdateBlobs response. grpc's status.New(...).Proto() yields exactly the
// genproto rpc/status message those response fields expect.
func statusProto(code codes.Code, msg string) *statusPB {
	return grpcstatus.New(code, msg).Proto()
}

// errAttr is the standard slog attribute for a logged error.
func errAttr(err error) slog.Attr { return slog.Any("error", err) }
