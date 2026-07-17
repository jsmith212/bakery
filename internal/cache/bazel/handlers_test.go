package bazel

import (
	"context"
	"strconv"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

func openMirror() fakeResolver {
	return fakeResolver{org: "acme", project: "widget", route: testRoute(false), ok: true}
}

func TestFindMissingBlobs_EmptyBlobNeverMissing_PositionalMisses(t *testing.T) {
	b, reader, store := testBackend(t, openMirror(), fakeAuthn{})

	present := seedCAS(reader, store, []byte("i am present"))
	empty := &repb.Digest{Hash: emptySHA256Hex, SizeBytes: 0}
	absent := &repb.Digest{Hash: "1111111111111111111111111111111111111111111111111111111111111111", SizeBytes: 3}

	resp, err := b.FindMissingBlobs(context.Background(), &repb.FindMissingBlobsRequest{
		InstanceName: "acme/widget",
		BlobDigests:  []*repb.Digest{present, empty, absent},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Only the absent digest is missing: present exists, and the empty blob is ALWAYS
	// available and must never appear.
	if len(resp.GetMissingBlobDigests()) != 1 {
		t.Fatalf("missing = %v, want exactly [absent]", resp.GetMissingBlobDigests())
	}

	if got := resp.GetMissingBlobDigests()[0].GetHash(); got != absent.GetHash() {
		t.Fatalf("missing hash = %s, want %s", got, absent.GetHash())
	}
}

func TestBatchReadBlobs_EmptyMissHit(t *testing.T) {
	b, reader, store := testBackend(t, openMirror(), fakeAuthn{})

	hitData := []byte("hello from bazel")
	hit := seedCAS(reader, store, hitData)
	empty := &repb.Digest{Hash: emptySHA256Hex, SizeBytes: 0}
	miss := &repb.Digest{Hash: "2222222222222222222222222222222222222222222222222222222222222222", SizeBytes: 4}

	resp, err := b.BatchReadBlobs(context.Background(), &repb.BatchReadBlobsRequest{
		InstanceName: "acme/widget",
		Digests:      []*repb.Digest{empty, hit, miss},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(resp.GetResponses()) != 3 {
		t.Fatalf("got %d responses, want 3", len(resp.GetResponses()))
	}

	// empty blob: OK, zero-length data, IDENTITY.
	if code := grpcstatus.FromProto(resp.GetResponses()[0].GetStatus()).Code(); code != codes.OK {
		t.Errorf("empty blob status = %v, want OK", code)
	}

	if len(resp.GetResponses()[0].GetData()) != 0 {
		t.Errorf("empty blob data non-empty")
	}

	if resp.GetResponses()[0].GetCompressor() != repb.Compressor_IDENTITY {
		t.Errorf("empty blob compressor != IDENTITY")
	}

	// hit: OK with the bytes.
	if code := grpcstatus.FromProto(resp.GetResponses()[1].GetStatus()).Code(); code != codes.OK {
		t.Errorf("hit status = %v, want OK", code)
	}

	if string(resp.GetResponses()[1].GetData()) != string(hitData) {
		t.Errorf("hit data = %q, want %q", resp.GetResponses()[1].GetData(), hitData)
	}

	// miss: NOT_FOUND per-blob, never a top-level error.
	if code := grpcstatus.FromProto(resp.GetResponses()[2].GetStatus()).Code(); code != codes.NotFound {
		t.Errorf("miss status = %v, want NotFound", code)
	}
}

func TestGetActionResult_HitMissAndUnparseable(t *testing.T) {
	b, reader, store := testBackend(t, openMirror(), fakeAuthn{})

	// A hit: seed a real ActionResult into ac-grpc under the action hash.
	ar := &repb.ActionResult{ExitCode: 7}
	data, _ := proto.Marshal(ar)
	dg := storage.KeyOf(data)

	const actionHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reader.add(actionHash, dg.Bytes(), int64(len(data)))
	store.put(dg, data)

	got, err := b.GetActionResult(context.Background(), &repb.GetActionResultRequest{
		InstanceName: "acme/widget", ActionDigest: &repb.Digest{Hash: actionHash},
	})
	if err != nil {
		t.Fatalf("hit: unexpected err: %v", err)
	}

	if got.GetExitCode() != 7 {
		t.Errorf("exit_code = %d, want 7", got.GetExitCode())
	}

	// A miss: NotFound and nothing else.
	_, err = b.GetActionResult(context.Background(), &repb.GetActionResultRequest{
		InstanceName: "acme/widget",
		ActionDigest: &repb.Digest{Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("miss: code = %v, want NotFound", grpcstatus.Code(err))
	}

	// Corruption: a value that fails proto.Unmarshal -> NotFound + a loud metric.
	const badHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	garbage := []byte{0xff, 0xff, 0xff, 0xff}
	gdg := storage.KeyOf(garbage)
	reader.add(badHash, gdg.Bytes(), int64(len(garbage)))
	store.put(gdg, garbage)

	_, err = b.GetActionResult(context.Background(), &repb.GetActionResultRequest{
		InstanceName: "acme/widget", ActionDigest: &repb.Digest{Hash: badHash},
	})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("unparseable: code = %v, want NotFound", grpcstatus.Code(err))
	}

	if v := bazelCounter(t, b.deps.Metrics, "bakery_cache_ac_unparseable_total"); v < 1 {
		t.Fatalf("bakery_cache_ac_unparseable_total = %v, want >= 1", v)
	}
}

func TestByteStreamRead_EmptyMissHitOffsetLimit(t *testing.T) {
	b, reader, store := testBackend(t, openMirror(), fakeAuthn{})

	// Empty blob: ZERO ReadResponse messages, OK (not one empty message).
	empty := &fakeReadServer{ctx: context.Background()}
	if err := b.Read(&bspb.ReadRequest{
		ResourceName: "acme/widget/blobs/" + emptySHA256Hex + "/0",
	}, empty); err != nil {
		t.Fatalf("empty read err: %v", err)
	}

	if len(empty.sent) != 0 {
		t.Fatalf("empty blob sent %d messages, want 0", len(empty.sent))
	}

	// Hit.
	data := []byte("0123456789abcdef")
	dg := seedCAS(reader, store, data)

	full := &fakeReadServer{ctx: context.Background()}
	if err := b.Read(&bspb.ReadRequest{
		ResourceName: "acme/widget/blobs/" + dg.GetHash() + "/" + itoa(dg.GetSizeBytes()),
	}, full); err != nil {
		t.Fatalf("full read err: %v", err)
	}

	if string(full.body()) != string(data) {
		t.Fatalf("full body = %q, want %q", full.body(), data)
	}

	// Offset + limit: read 4 bytes starting at offset 2.
	part := &fakeReadServer{ctx: context.Background()}
	req := &bspb.ReadRequest{
		ResourceName: "acme/widget/blobs/" + dg.GetHash() + "/" + itoa(dg.GetSizeBytes()),
		ReadOffset:   2, ReadLimit: 4,
	}
	if err := b.Read(req, part); err != nil {
		t.Fatalf("partial read err: %v", err)
	}

	if string(part.body()) != "2345" {
		t.Fatalf("partial body = %q, want %q", part.body(), "2345")
	}

	// Miss -> NotFound.
	missSrv := &fakeReadServer{ctx: context.Background()}
	err := b.Read(&bspb.ReadRequest{
		ResourceName: "acme/widget/blobs/3333333333333333333333333333333333333333333333333333333333333333/9",
	}, missSrv)
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("miss: code = %v, want NotFound", grpcstatus.Code(err))
	}
}

func TestByteStreamWrite_EmptyBlobAndEarlyExit(t *testing.T) {
	// Writes ALWAYS require a write-scoped key, even on an open mirror.
	authn := fakeAuthn{byToken: map[string]fakePrincipal{"bkry_w": {read: true, write: true}}}
	b, reader, store := testBackend(t, openMirror(), authn)
	ctx := bearerCtx("bkry_w")

	// Empty blob: committed_size 0, stored nowhere, OK -- before any Put.
	emptyW := &fakeWriteServer{ctx: ctx, frames: []*bspb.WriteRequest{
		{ResourceName: "acme/widget/uploads/u/blobs/" + emptySHA256Hex + "/0", FinishWrite: true},
	}}
	if err := b.Write(emptyW); err != nil {
		t.Fatalf("empty write err: %v", err)
	}

	if emptyW.resp.GetCommittedSize() != 0 {
		t.Fatalf("empty committed_size = %d, want 0", emptyW.resp.GetCommittedSize())
	}

	// Early exit: a blob already present short-circuits to SendAndClose with the
	// declared size (committed_size = size, never 0, never -1) -- WITHOUT reaching Put
	// (the read-only fixture has no Txer, so reaching Put would error).
	data := []byte("already here")
	dg := seedCAS(reader, store, data)

	hitW := &fakeWriteServer{ctx: ctx, frames: []*bspb.WriteRequest{
		{
			ResourceName: "acme/widget/uploads/u/blobs/" + dg.GetHash() + "/" + itoa(dg.GetSizeBytes()),
			Data:         data, FinishWrite: true,
		},
	}}
	if err := b.Write(hitW); err != nil {
		t.Fatalf("early-exit write err: %v", err)
	}

	if hitW.resp.GetCommittedSize() != dg.GetSizeBytes() {
		t.Fatalf("early-exit committed_size = %d, want %d", hitW.resp.GetCommittedSize(), dg.GetSizeBytes())
	}
}

func TestQueryWriteStatus_Unimplemented(t *testing.T) {
	b, _, _ := testBackend(t, openMirror(), fakeAuthn{})

	_, err := b.QueryWriteStatus(context.Background(), &bspb.QueryWriteStatusRequest{})
	if grpcstatus.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", grpcstatus.Code(err))
	}
}

// bazelCounter sums every series of a counter family in m's registry.
func bazelCounter(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var total float64

	for _, f := range families {
		if f.GetName() != name {
			continue
		}

		for _, mm := range f.GetMetric() {
			total += mm.GetCounter().GetValue()
		}
	}

	return total
}

// itoa renders an int64 in base 10.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
