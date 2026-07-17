package bazel

import (
	"context"
	"errors"
	"io"

	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// readChunk is the ByteStream Read frame size. 64 KiB matches moon/Bazel's own
// framing and keeps a single ReadResponse comfortably under any message cap.
const readChunk = 64 * 1024

// Write ingests a CAS blob. The contract is exact and every clause is load-bearing:
//
//   - resource_name is latched from FRAME 1; subsequent frames carry an empty one.
//   - the digest (hash + size) is known before one payload byte is ingested, so the
//     early exit is possible: on an Exists hit, SendAndClose immediately. HTTP/2
//     RST_STREAM is per-stream, so there is NO drain to do (unlike HTTP/1.1).
//   - the framed stream is handed to blob.Put ONCE as an io.Reader (writeReader,
//     never a []byte) with Overwrite:false and Verify against the resource's digest.
//   - committed_size = res.Size. NEVER 0, NEVER -1. res.Created==false (dedup) is a
//     SUCCESS, not ALREADY_EXISTS. res.Size is compared to the declared size here --
//     blob verifies the digest, never the client's declared size.
func (b *Backend) Write(stream bspb.ByteStream_WriteServer) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return grpcstatus.Error(codes.InvalidArgument, "empty write stream")
		}

		return b.grpcError(ctx, "Write", err)
	}

	res, err := parseResourceName(first.GetResourceName())
	if err != nil {
		return b.grpcError(ctx, "Write", err) // compressed -> Unimplemented; bad -> InvalidArgument
	}

	route, err := b.authorize(ctx, res.instance, true)
	if err != nil {
		return err
	}

	// The empty blob is stored nowhere: committed_size 0, OK.
	if isEmpty(res.hash, res.size) {
		return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: 0})
	}

	if zeroSizeButNotEmpty(res.hash, res.size) {
		return grpcstatus.Error(codes.InvalidArgument, "size 0 with a non-empty hash")
	}

	key, err := storage.ParseKey(res.hash)
	if err != nil {
		return grpcstatus.Error(codes.InvalidArgument, "invalid digest")
	}

	ref := route.Ref(casNamespace, casKind, res.hash)

	// EARLY EXIT -- where the redundant-upload storm dies, and where moon lives. Probe
	// Exists (LRU-backed) and, on a hit, terminate without draining. The io.EOF the
	// client's Send sees is the documented contract; it collects the success from
	// SendAndClose cleanly.
	//
	// This bypasses blob.Service.Put, so it must record the put/hit and the write bytes
	// ITSELF -- otherwise gRPC (moon's and Bazel's default transport) warm-build dedup
	// traffic counts only op=exists and the put hit-rate + write-throughput series
	// silently undercount forever. This mirrors httpblob's /cas SkipIngestIfPresent path.
	if ok, existErr := b.deps.Blobs.Exists(ctx, ref); existErr == nil && ok {
		rec := b.recorder(ref)
		rec.Observe(metrics.OpPut, metrics.ResultHit)
		rec.AddBytes(metrics.OpPut, res.size)
		b.deps.Metrics.Bazel(route.Org, route.Project).ByteStreamBytes(metrics.BazelBSWrite, res.size)

		return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: res.size})
	}

	reader := &writeReader{stream: stream, buf: first.GetData(), done: first.GetFinishWrite()}

	putRes, err := b.deps.Blobs.Put(ctx, ref, reader, blob.PutOptions{
		Overwrite: false,
		Verify:    blob.VerifyDigest(key),
	})
	if err != nil {
		// A stream error (client disconnect mid-upload) surfaces through the reader;
		// prefer it so the mapping is honest.
		if reader.err != nil {
			return b.grpcError(ctx, "Write", reader.err)
		}

		return b.grpcError(ctx, "Write", err)
	}

	if putRes.Size != res.size {
		return grpcstatus.Errorf(codes.InvalidArgument,
			"committed %d bytes, resource declared %d", putRes.Size, res.size)
	}

	b.deps.Metrics.Bazel(route.Org, route.Project).ByteStreamBytes(metrics.BazelBSWrite, putRes.Size)

	return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: putRes.Size})
}

// writeReader adapts the client-streaming Write frames to an io.Reader for blob.Put.
// It NEVER accumulates a []byte: it yields the first frame's data, then pulls further
// frames on demand. The {uuid} in the resource name is never consulted, so moon's
// one-uuid-per-process reuse cannot interleave two uploads here.
type writeReader struct {
	stream bspb.ByteStream_WriteServer
	buf    []byte // unread bytes of the current frame; seeded with frame 1's data
	done   bool   // a finish_write frame (or EOF) has been seen
	err    error  // a non-EOF stream error, surfaced to Write for honest mapping
}

func (r *writeReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}

		req, err := r.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.done = true

				return 0, io.EOF
			}

			r.err = err

			return 0, err
		}

		r.buf = req.GetData()

		if req.GetFinishWrite() {
			r.done = true
		}
	}

	n := copy(p, r.buf)
	r.buf = r.buf[n:]

	return n, nil
}

// Read streams a CAS blob back. read_offset and read_limit are honored; the empty
// blob yields ZERO ReadResponse messages and OK (NOT one empty message).
func (b *Backend) Read(req *bspb.ReadRequest, stream bspb.ByteStream_ReadServer) error {
	ctx := stream.Context()

	res, err := parseResourceName(req.GetResourceName())
	if err != nil {
		return b.grpcError(ctx, "Read", err)
	}

	route, err := b.authorize(ctx, res.instance, false)
	if err != nil {
		return err
	}

	offset, limit := req.GetReadOffset(), req.GetReadLimit()
	if offset < 0 || limit < 0 {
		return grpcstatus.Error(codes.InvalidArgument, "negative read_offset or read_limit")
	}

	if isEmpty(res.hash, res.size) {
		if offset > 0 {
			return grpcstatus.Error(codes.OutOfRange, "read_offset past end of blob")
		}

		return nil // zero messages, OK
	}

	if zeroSizeButNotEmpty(res.hash, res.size) {
		return grpcstatus.Error(codes.InvalidArgument, "size 0 with a non-empty hash")
	}

	ref := route.Ref(casNamespace, casKind, res.hash)

	meta, rc, err := b.deps.Blobs.Get(ctx, ref)
	if errors.Is(err, blob.ErrNotFound) {
		return grpcstatus.Error(codes.NotFound, "not found")
	}

	if err != nil {
		return b.grpcError(ctx, "Read", err)
	}

	defer func() { _ = rc.Close() }()

	if offset > meta.Size {
		return grpcstatus.Error(codes.OutOfRange, "read_offset past end of blob")
	}

	if offset == meta.Size {
		return nil // nothing to send
	}

	if err := discard(rc, offset); err != nil {
		return b.grpcError(ctx, "Read", err)
	}

	var src io.Reader = rc
	if limit > 0 {
		src = io.LimitReader(rc, limit)
	}

	total, err := b.pump(stream, src)
	if err != nil {
		return err
	}

	b.deps.Metrics.Bazel(route.Org, route.Project).ByteStreamBytes(metrics.BazelBSRead, total)

	return nil
}

// discard advances rc by offset bytes, using Seek when the reader supports it (the
// local store's *os.File does) and falling back to a copy otherwise.
func discard(rc io.ReadCloser, offset int64) error {
	if offset == 0 {
		return nil
	}

	if seeker, ok := rc.(io.Seeker); ok {
		_, err := seeker.Seek(offset, io.SeekStart)

		return err
	}

	_, err := io.CopyN(io.Discard, rc, offset)

	return err
}

// pump sends src to the client in readChunk frames, returning the byte count. A Send
// error is returned verbatim -- it already carries the client's cancellation status.
func (b *Backend) pump(stream bspb.ByteStream_ReadServer, src io.Reader) (int64, error) {
	buf := make([]byte, readChunk)
	total := int64(0)

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := stream.Send(&bspb.ReadResponse{Data: buf[:n]}); err != nil {
				return total, err
			}

			total += int64(n)
		}

		if errors.Is(rerr, io.EOF) {
			return total, nil
		}

		if rerr != nil {
			return total, b.grpcError(stream.Context(), "Read", rerr)
		}
	}
}

// QueryWriteStatus returns Unimplemented -- NOT the spec-mandated NOT_FOUND. Bazel
// special-cases UNIMPLEMENTED (restart from 0, never ask again); a NOT_FOUND
// propagates and FAILS the upload.
func (b *Backend) QueryWriteStatus(
	_ context.Context, _ *bspb.QueryWriteStatusRequest,
) (*bspb.QueryWriteStatusResponse, error) {
	return nil, grpcstatus.Error(codes.Unimplemented, "QueryWriteStatus is not implemented")
}
