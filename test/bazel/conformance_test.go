// Package bazelconf proves that Bakery serves the REAL Bazel REAPI over gRPC and the
// REAL ccache + sccache HTTP clients. It is M4's CI gate, and DESIGN.md calls the
// conformance gates non-negotiable.
//
// It has THREE clients, because no single one exercises the whole surface:
//
//	Client 1 -- the remote-apis-sdks Go client (a TEST-ONLY dep). It is the reference
//	REAPI client, and it needs NO external binary: only Postgres (docker or TEST_DB_URL).
//	It drives all eight cache RPCs Bakery implements -- GetCapabilities, ByteStream
//	Write and Read, FindMissingBlobs, BatchUpdate/BatchReadBlobs, and the ActionCache
//	Get/Update pair -- plus the api-version intersection and the REAPI empty blob.
//
//	Client 2 -- the real ccache binary, over @layout=bazel. It writes ONLY to /ac and
//	must never touch /cas; the recorder proves that against the client, not a doc.
//
//	Client 3 -- the real sccache binary, over its opendal WebDAV backend. The failure
//	mode is a SILENTLY read-only backend (a PROPFIND that fails to deserialize latches
//	can_write=false), so the assertion is that a PUT round-trips: PROPFIND then PUT, a
//	warm HIT, and sccache --show-stats reporting a cache write.
//
// It lives OUTSIDE internal/ on purpose. `just race`/`just coverage` glob ./..., and
// this package is compiled and run there; every binary-guarded test therefore calls
// requireBinary FIRST, before dbtest.New and before the server boots, so a skip on a
// runner without ccache/sccache costs nothing. `just bazel-conformance` is its home,
// and there a skip FAILS the job -- CI provides the clients, so a skip in CI means the
// proof did not run. The remote-apis-sdks half needs no binary and runs everywhere
// dbtest can reach a database.
package bazelconf

import (
	"context"
	"testing"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// TestMain gives the suite its own ephemeral Postgres. dbtest.Main is lazy -- the
// container (or the TEST_DB_URL template clone) is created by the first dbtest.New and
// by nothing else -- so a run that skips every test pays nothing for it.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// bearerToken is the per-RPC credential the REAPI client sends: "authorization: Bearer
// <bkry_...>", the exact metadata shape credentialCandidates' Bearer arm reads. A Bakery
// credential is ONE opaque token -- there is no id:secret pair -- so it goes in verbatim.
//
// RequireTransportSecurity is false because the conformance channel is cleartext h2c
// (DialParams.NoSecurity, prior-knowledge HTTP/2): grpc-go refuses to attach a per-RPC
// credential that demands TLS to an insecure connection, so a `true` here would fail the
// dial before a single byte moved. Production terminates TLS in front of the listener;
// the token-in-metadata contract is identical either way.
type bearerToken string

func (t bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return false }

// newREAPIClient dials the REAL gRPC listener exactly as moon/Bazel's own client would:
// NoSecurity gives cleartext prior-knowledge h2c, and the write-scoped token rides every
// RPC as Bearer metadata. instance_name is "{org}/{project}" WITH the slash -- the shape
// ByteStream resource-name parsing must scan past, never split positionally.
func newREAPIClient(t *testing.T, e *env) *client.Client {
	t.Helper()

	c, err := client.NewClient(t.Context(), e.instanceName(), client.DialParams{
		Service:    e.grpcAddr,
		NoSecurity: true,
		DialOpts:   []grpc.DialOption{grpc.WithPerRPCCredentials(bearerToken(e.writeKey))},
	})
	if err != nil {
		t.Fatalf("dial REAPI at %s: %v", e.grpcAddr, err)
	}

	t.Cleanup(func() {
		if cerr := c.Close(); cerr != nil {
			t.Errorf("close REAPI client: %v", cerr)
		}
	})

	return c
}

// TestReferenceREAPIClient drives the reference remote-apis-sdks client against the real
// Bakery REAPI. This is the half of the gate that runs EVERYWHERE dbtest can reach a
// database -- it needs no external binary -- so it is the proof CI always has, even on a
// runner where ccache/sccache are absent.
//
// The subtests share ONE client and seed each other: the ByteStream write puts a blob the
// FindMissingBlobs and Read subtests then observe. They run in order (t.Run is
// sequential here), so the seeding is deterministic.
func TestReferenceREAPIClient(t *testing.T) {
	e := newEnv(t)
	c := newREAPIClient(t, e)
	ctx := t.Context()

	// A blob the whole test threads through: written once over ByteStream, read back,
	// and probed for by FindMissingBlobs.
	payload := []byte("bakery m4 reapi conformance -- the reference client's blob")
	payloadDg := digest.NewFromBlob(payload)

	// -----------------------------------------------------------------------
	// RPC 1 -- GetCapabilities, AND the api-version intersection. CheckCapabilities is
	// what a real build runs on startup: it fetches ServerCapabilities and verifies the
	// [low, high] api-version window intersects the client's and that SHA256 is offered.
	// An unset low/high is 0.0.0..0.0.0, which intersects nothing and kills the channel
	// before any cache RPC -- so a green here is the intersection proof.
	// -----------------------------------------------------------------------
	t.Run("get_capabilities_and_api_version_intersection", func(t *testing.T) {
		if err := c.CheckCapabilities(ctx); err != nil {
			t.Fatalf("CheckCapabilities (api-version intersection / digest function): %v", err)
		}

		caps, err := c.GetCapabilities(ctx)
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}

		if caps.GetLowApiVersion() == nil || caps.GetHighApiVersion() == nil {
			t.Fatalf("GetCapabilities returned low=%v high=%v; an unset version pair is "+
				"0.0.0..0.0.0 and intersects nothing", caps.GetLowApiVersion(), caps.GetHighApiVersion())
		}

		t.Logf("PROVEN: GetCapabilities + intersection over the reference client: api [%d.%d, %d.%d]",
			caps.GetLowApiVersion().GetMajor(), caps.GetLowApiVersion().GetMinor(),
			caps.GetHighApiVersion().GetMajor(), caps.GetHighApiVersion().GetMinor())
	})

	// -----------------------------------------------------------------------
	// RPC 2+3 -- ByteStream Write then Read, the CAS single-blob hot path. WriteBlob
	// streams over ByteStream.Write (resource_name blobs/<hash>/<size>); ReadBlob streams
	// it back. A mismatch here is a resource-name parse bug -- the instance_name has a
	// slash, so a positional split would land on the wrong marker.
	// -----------------------------------------------------------------------
	t.Run("bytestream_write_read_roundtrip", func(t *testing.T) {
		gotDg, err := c.WriteBlob(ctx, payload)
		if err != nil {
			t.Fatalf("WriteBlob (ByteStream Write): %v", err)
		}

		if gotDg != payloadDg {
			t.Fatalf("WriteBlob digest = %s, want %s", gotDg, payloadDg)
		}

		got, _, err := c.ReadBlob(ctx, payloadDg)
		if err != nil {
			t.Fatalf("ReadBlob (ByteStream Read): %v", err)
		}

		if string(got) != string(payload) {
			t.Fatalf("ReadBlob returned %q, want %q", got, payload)
		}

		t.Logf("PROVEN: ByteStream Write->Read round-tripped %d bytes at %s", len(payload), payloadDg)
	})

	// -----------------------------------------------------------------------
	// RPC 4 -- FindMissingBlobs. The blob written above is present; a fresh digest is
	// not. The setscene-equivalent hot path: the client asks which of a set it must
	// upload, and only the unknown one comes back.
	// -----------------------------------------------------------------------
	t.Run("find_missing_blobs", func(t *testing.T) {
		absent := digest.NewFromBlob([]byte("a blob that was never uploaded to this cache"))

		missing, err := c.MissingBlobs(ctx, []digest.Digest{payloadDg, absent})
		if err != nil {
			t.Fatalf("MissingBlobs (FindMissingBlobs): %v", err)
		}

		if len(missing) != 1 || missing[0] != absent {
			t.Fatalf("FindMissingBlobs = %v, want exactly [%s] -- the present blob must not "+
				"be reported missing and the absent one must be", missing, absent)
		}

		t.Logf("PROVEN: FindMissingBlobs reported only the absent digest %s", absent)
	})

	// -----------------------------------------------------------------------
	// RPC 5+6 -- BatchUpdateBlobs then BatchReadBlobs, moon's small-blob path. A distinct
	// blob, written and read back through the batch RPCs rather than ByteStream.
	// -----------------------------------------------------------------------
	t.Run("batch_update_read_blobs", func(t *testing.T) {
		batchBlob := []byte("a batch-written blob")
		batchDg := digest.NewFromBlob(batchBlob)

		if err := c.BatchWriteBlobs(ctx, map[digest.Digest][]byte{batchDg: batchBlob}); err != nil {
			t.Fatalf("BatchWriteBlobs (BatchUpdateBlobs): %v", err)
		}

		got, err := c.BatchDownloadBlobs(ctx, []digest.Digest{batchDg})
		if err != nil {
			t.Fatalf("BatchDownloadBlobs (BatchReadBlobs): %v", err)
		}

		if string(got[batchDg]) != string(batchBlob) {
			t.Fatalf("BatchReadBlobs returned %q, want %q", got[batchDg], batchBlob)
		}

		t.Logf("PROVEN: BatchUpdateBlobs->BatchReadBlobs round-tripped %s", batchDg)
	})

	// -----------------------------------------------------------------------
	// RPC 7+8 -- the ActionCache round trip: UpdateActionResult then GetActionResult. The
	// gRPC ActionCache is a TYPED store (namespace "ac-grpc"), distinct from the opaque
	// HTTP /ac mount -- GetActionResult MUST parse the ActionResult back out, which the
	// opaque byte store deliberately never does.
	// -----------------------------------------------------------------------
	t.Run("action_cache_roundtrip", func(t *testing.T) {
		// The action digest is just a key here; it need not reference a stored Action.
		actionDg := digest.NewFromBlob([]byte("bakery m4 action")).ToProto()

		want := &repb.ActionResult{ExitCode: 7, StdoutRaw: []byte("hello from the action")}

		if _, err := c.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
			InstanceName: e.instanceName(),
			ActionDigest: actionDg,
			ActionResult: want,
		}); err != nil {
			t.Fatalf("UpdateActionResult: %v", err)
		}

		got, err := c.GetActionResult(ctx, &repb.GetActionResultRequest{
			InstanceName: e.instanceName(),
			ActionDigest: actionDg,
		})
		if err != nil {
			t.Fatalf("GetActionResult: %v", err)
		}

		if got.GetExitCode() != want.GetExitCode() || string(got.GetStdoutRaw()) != string(want.GetStdoutRaw()) {
			t.Fatalf("GetActionResult = exit %d stdout %q, want exit %d stdout %q",
				got.GetExitCode(), got.GetStdoutRaw(), want.GetExitCode(), want.GetStdoutRaw())
		}

		t.Logf("PROVEN: ActionCache Update->Get round-tripped a typed ActionResult (exit=%d)",
			got.GetExitCode())
	})

	// -----------------------------------------------------------------------
	// The REAPI empty blob. "Servers MUST behave as though empty blobs are always
	// available, even if they have not been uploaded." It is never stored, so a Read of
	// it must succeed with zero bytes and FindMissingBlobs must never report it missing --
	// without ever touching blob.Service (where e3b0c442 is genuinely absent and a Stat
	// would manufacture a 500).
	// -----------------------------------------------------------------------
	t.Run("empty_blob_is_always_available", func(t *testing.T) {
		got, _, err := c.ReadBlob(ctx, digest.Empty)
		if err != nil {
			t.Fatalf("ReadBlob(empty): %v -- the empty blob must always read as zero bytes", err)
		}

		if len(got) != 0 {
			t.Fatalf("ReadBlob(empty) returned %d bytes, want 0", len(got))
		}

		missing, err := c.MissingBlobs(ctx, []digest.Digest{digest.Empty})
		if err != nil {
			t.Fatalf("MissingBlobs(empty): %v", err)
		}

		if len(missing) != 0 {
			t.Fatalf("FindMissingBlobs reported the empty blob missing (%v); it must always be present", missing)
		}

		t.Logf("PROVEN: the REAPI empty blob reads as 0 bytes and is never missing")
	})
}

// TestCapabilitiesRegressionFences are the negative controls: two fields whose wrong
// value is a SILENT catastrophe, pinned against the documented client logic rather than
// against a build. Both are driven through the reference client's real GetCapabilities.
//
//   - MaxBatchTotalSizeBytes == 0 (which proto3 cannot distinguish from unset) makes moon
//     stop streaming and shove a ~48 MiB blob into one unary BatchUpdateBlobs, then
//     misreport the RESOURCE_EXHAUSTED as "out of storage space". A non-zero ceiling is
//     the switch that keeps moon streaming.
//   - UpdateEnabled == false is a client-side kill switch: moon uploads every CAS blob and
//     then SKIPS UpdateActionResult with only a debug log -- a 0% hit rate, forever, with
//     no warning. It MUST be true.
//
// These are separate from the round-trip test because they assert on the ADVERTISED
// payload, which is the thing the client reads to decide its behaviour -- the bug is in
// the advertisement, and it must fail here even if every round trip passes.
func TestCapabilitiesRegressionFences(t *testing.T) {
	e := newEnv(t)
	c := newREAPIClient(t, e)

	caps, err := c.GetCapabilities(t.Context())
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}

	cc := caps.GetCacheCapabilities()
	if cc == nil {
		t.Fatal("GetCapabilities returned no CacheCapabilities")
	}

	if cc.GetMaxBatchTotalSizeBytes() == 0 {
		t.Errorf("max_batch_total_size_bytes == 0: proto3 cannot tell this from unset, and moon "+
			"reads it as 'no batching' -- it then unary-uploads a ~48 MiB blob and misreports the "+
			"RESOURCE_EXHAUSTED as out of storage. Want a non-zero ceiling; got %d",
			cc.GetMaxBatchTotalSizeBytes())
	}

	if !cc.GetActionCacheUpdateCapabilities().GetUpdateEnabled() {
		t.Error("ActionCacheUpdateCapabilities.update_enabled == false: moon reads this as a kill " +
			"switch and SKIPS every UpdateActionResult with only a debug log -- a 0% hit rate " +
			"forever, no warning. It MUST be true.")
	}

	t.Logf("PROVEN: capabilities fences hold -- max_batch_total_size_bytes=%d, update_enabled=%t",
		cc.GetMaxBatchTotalSizeBytes(), cc.GetActionCacheUpdateCapabilities().GetUpdateEnabled())
}
