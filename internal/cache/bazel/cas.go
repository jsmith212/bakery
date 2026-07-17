package bazel

import (
	"bytes"
	"context"
	"errors"
	"io"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/storage"
)

// casNamespace is the content-addressed namespace, shared with the HTTP /cas mount:
// the key IS the content hash, so cross-transport sharing is safe (unlike the AC
// split). "Missing" means missing from THIS project -- cache_objects.backend_id is
// the tenancy boundary -- never "missing from the installation".
const casNamespace = "cas"

// casKind is the metrics sub-kind label for CAS objects.
const casKind = "cas"

// FindMissingBlobs answers the REAPI HEAD storm via blob.Service.ExistsBatch (ONE
// query for the residue). The empty blob is NEVER reported missing: it is stripped
// before blob.Service is touched, because "servers MUST behave as though empty blobs
// are always available".
func (b *Backend) FindMissingBlobs(
	ctx context.Context, req *repb.FindMissingBlobsRequest,
) (*repb.FindMissingBlobsResponse, error) {
	route, err := b.authorize(ctx, req.GetInstanceName(), false)
	if err != nil {
		return nil, err
	}

	digests := req.GetBlobDigests()

	refs := make([]blob.Ref, 0, len(digests))
	pos := make([]int, 0, len(digests)) // refs[j] answers digests[pos[j]]

	for i, d := range digests {
		if isEmpty(d.GetHash(), d.GetSizeBytes()) {
			continue // always available -> never missing
		}

		refs = append(refs, route.Ref(casNamespace, casKind, d.GetHash()))
		pos = append(pos, i)
	}

	resp := &repb.FindMissingBlobsResponse{}

	if len(refs) == 0 {
		return resp, nil
	}

	exists, err := b.deps.Blobs.ExistsBatch(ctx, refs)
	if err != nil {
		return nil, b.grpcError(ctx, "FindMissingBlobs", err)
	}

	missing := 0

	for j, ok := range exists {
		if !ok {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, digests[pos[j]])
			missing++
		}
	}

	b.deps.Metrics.Bazel(route.Org, route.Project).FindMissing(len(refs), missing)

	return resp, nil
}

// BatchReadBlobs reads many small blobs at once. Per-blob status is tolerated: a
// missing blob is NOT_FOUND on that entry, not a top-level error. The empty blob is
// OK with empty data and IDENTITY.
func (b *Backend) BatchReadBlobs(
	ctx context.Context, req *repb.BatchReadBlobsRequest,
) (*repb.BatchReadBlobsResponse, error) {
	route, err := b.authorize(ctx, req.GetInstanceName(), false)
	if err != nil {
		return nil, err
	}

	resp := &repb.BatchReadBlobsResponse{}

	for _, d := range req.GetDigests() {
		r := &repb.BatchReadBlobsResponse_Response{
			Digest:     d,
			Compressor: repb.Compressor_IDENTITY,
		}

		switch {
		case isEmpty(d.GetHash(), d.GetSizeBytes()):
			r.Data = []byte{} // OK (nil Status)
		case zeroSizeButNotEmpty(d.GetHash(), d.GetSizeBytes()):
			r.Status = statusProto(codes.InvalidArgument, "size 0 with a non-empty hash")
		default:
			r.Data, r.Status = b.readOneBlob(ctx, route.Ref(casNamespace, casKind, d.GetHash()))
		}

		resp.Responses = append(resp.Responses, r)
	}

	return resp, nil
}

// readOneBlob reads a single CAS blob for BatchReadBlobs, returning its bytes and the
// per-blob status (nil status == OK).
func (b *Backend) readOneBlob(ctx context.Context, ref blob.Ref) ([]byte, *statusPB) {
	_, rc, err := b.deps.Blobs.Get(ctx, ref)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, statusProto(codes.NotFound, "not found")
	}

	if err != nil {
		b.deps.Logger.ErrorContext(ctx, "bazel: BatchReadBlobs read", errAttr(err))

		return nil, statusProto(codes.Internal, "internal error")
	}

	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		b.deps.Logger.ErrorContext(ctx, "bazel: BatchReadBlobs read", errAttr(err))

		return nil, statusProto(codes.Internal, "internal error")
	}

	return data, nil
}

// BatchUpdateBlobs uploads many small blobs at once. Each blob's digest is verified;
// the empty blob is an OK no-op; a non-identity compressor or a size-0/non-empty
// digest is InvalidArgument on that entry.
func (b *Backend) BatchUpdateBlobs(
	ctx context.Context, req *repb.BatchUpdateBlobsRequest,
) (*repb.BatchUpdateBlobsResponse, error) {
	route, err := b.authorize(ctx, req.GetInstanceName(), true)
	if err != nil {
		return nil, err
	}

	resp := &repb.BatchUpdateBlobsResponse{}

	for _, item := range req.GetRequests() {
		d := item.GetDigest()

		r := &repb.BatchUpdateBlobsResponse_Response{Digest: d}

		switch {
		case d == nil || d.GetHash() == "":
			r.Status = statusProto(codes.InvalidArgument, "missing digest")
		case item.GetCompressor() != repb.Compressor_IDENTITY:
			r.Status = statusProto(codes.InvalidArgument, "unsupported compressor")
		case isEmpty(d.GetHash(), d.GetSizeBytes()):
			// OK no-op (nil Status). Never stored.
		case zeroSizeButNotEmpty(d.GetHash(), d.GetSizeBytes()):
			r.Status = statusProto(codes.InvalidArgument, "size 0 with a non-empty hash")
		default:
			r.Status = b.updateOneBlob(ctx, route.Ref(casNamespace, casKind, d.GetHash()), d, item.GetData())
		}

		resp.Responses = append(resp.Responses, r)
	}

	return resp, nil
}

// updateOneBlob writes a single verified CAS blob for BatchUpdateBlobs, returning the
// per-blob status (nil status == OK).
func (b *Backend) updateOneBlob(
	ctx context.Context, ref blob.Ref, d *repb.Digest, data []byte,
) *statusPB {
	key, err := storage.ParseKey(d.GetHash())
	if err != nil {
		return statusProto(codes.InvalidArgument, "invalid digest")
	}

	res, err := b.deps.Blobs.Put(ctx, ref, bytes.NewReader(data), blob.PutOptions{
		Overwrite: false,
		Verify:    blob.VerifyDigest(key),
	})

	switch {
	case errors.Is(err, blob.ErrDigestMismatch):
		return statusProto(codes.InvalidArgument, "content does not match the declared digest")
	case err != nil:
		b.deps.Logger.ErrorContext(ctx, "bazel: BatchUpdateBlobs write", errAttr(err))

		return statusProto(codes.Internal, "internal error")
	case res.Size != d.GetSizeBytes():
		return statusProto(codes.InvalidArgument, "content size does not match the declared size")
	default:
		return nil // OK
	}
}
