package bazel

import (
	"bytes"
	"context"
	"errors"
	"io"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jsmith212/bakery/internal/blob"
)

// The gRPC ActionCache is a TYPED store and owns namespace "ac-grpc" -- SPLIT from
// the opaque HTTP "ac". It cannot share "ac": moon computes the SAME action digest
// over both transports, so one namespace makes a moon workspace that switches
// api:grpc -> api:http read a protobuf where its client demands JSON, a hard client
// error rather than a miss. GetActionResult returns a typed *repb.ActionResult, so it
// MUST parse -- which is exactly why it cannot live on the opaque mount.

// acNamespace is the gRPC ActionCache namespace. Only UpdateActionResult writes it,
// so an unparseable value there can only be storage corruption, never foreign traffic.
const acNamespace = "ac-grpc"

// acKind is the metrics sub-kind label for AC objects.
const acKind = "ac"

// GetActionResult reads the typed ActionResult for an action digest.
//
// A miss -- and an UNPARSEABLE value -- both return NotFound AND NOTHING ELSE: Bazel
// maps NOT_FOUND to a clean miss and every other status to a build-failing
// IOException. An unparseable value additionally fires a loud metric; it is NOT
// deleted (a read path must not mutate storage, and in ac-grpc only we write, so the
// operator, not the request, must resolve the corruption).
func (b *Backend) GetActionResult(
	ctx context.Context, req *repb.GetActionResultRequest,
) (*repb.ActionResult, error) {
	route, err := b.authorize(ctx, req.GetInstanceName(), false)
	if err != nil {
		return nil, err
	}

	d := req.GetActionDigest()
	if d == nil || d.GetHash() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "missing action_digest")
	}

	ref := route.Ref(acNamespace, acKind, d.GetHash())

	_, rc, err := b.deps.Blobs.Get(ctx, ref)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, grpcstatus.Error(codes.NotFound, "no cached result")
	}

	if err != nil {
		return nil, b.grpcError(ctx, "GetActionResult", err)
	}

	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, b.grpcError(ctx, "GetActionResult", err)
	}

	var ar repb.ActionResult

	// A zero-length value proto.Unmarshals SUCCESSFULLY into ActionResult{exit_code:0}
	// -- "a successful action with no outputs", the one garbage that parses. We never
	// store it (see UpdateActionResult), so seeing it is corruption: treat it as a
	// miss, loudly.
	if len(data) == 0 || proto.Unmarshal(data, &ar) != nil {
		b.deps.Metrics.Bazel(route.Org, route.Project).ACUnparseable()

		return nil, grpcstatus.Error(codes.NotFound, "no cached result")
	}

	return &ar, nil
}

// UpdateActionResult proto.Marshals the ActionResult into "ac-grpc". This is SAFE and
// is NOT the OCI trap: the AC key is sha256(the encoded Action), a DIFFERENT message,
// and the value's own bytes are addressed by nobody -- REAPI licenses modifying the
// action_result outright. An EMPTY action_result is refused: it marshals to zero
// bytes, which is precisely the poisoned value GetActionResult defends against.
func (b *Backend) UpdateActionResult(
	ctx context.Context, req *repb.UpdateActionResultRequest,
) (*repb.ActionResult, error) {
	route, err := b.authorize(ctx, req.GetInstanceName(), true)
	if err != nil {
		return nil, err
	}

	d := req.GetActionDigest()
	if d == nil || d.GetHash() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "missing action_digest")
	}

	ar := req.GetActionResult()
	if ar == nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "missing action_result")
	}

	data, err := proto.Marshal(ar)
	if err != nil {
		return nil, b.grpcError(ctx, "UpdateActionResult", err)
	}

	if len(data) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "refusing to store an empty action result")
	}

	ref := route.Ref(acNamespace, acKind, d.GetHash())

	// Overwrite: an action's result is mutable (a re-run may replace it). NoVerify:
	// the key is the Action digest, not a hash of this value.
	if _, err := b.deps.Blobs.Put(ctx, ref, bytes.NewReader(data), blob.PutOptions{
		Overwrite: true,
		Verify:    blob.NoVerify(),
	}); err != nil {
		return nil, b.grpcError(ctx, "UpdateActionResult", err)
	}

	return ar, nil
}
