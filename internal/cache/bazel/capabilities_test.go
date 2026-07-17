package bazel

import (
	"context"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// The three fields below are silent-catastrophe fields: getting any of them wrong
// leaves a cache that looks healthy and never hits (or a client that hard-fails at
// channel setup). Each gets its own named regression assertion.

func TestCapabilities_UpdateEnabledIsTrue(t *testing.T) {
	caps := capabilitiesPayload().GetCacheCapabilities()

	if !caps.GetActionCacheUpdateCapabilities().GetUpdateEnabled() {
		t.Fatal("update_enabled = false: moon would upload every CAS blob and SKIP " +
			"UpdateActionResult -- a 0% hit rate forever, silently")
	}
}

func TestCapabilities_MaxBatchTotalSizeBytesIsNonZeroAndBounded(t *testing.T) {
	got := capabilitiesPayload().GetCacheCapabilities().GetMaxBatchTotalSizeBytes()

	if got == 0 {
		t.Fatal("max_batch_total_size_bytes = 0: moon stops streaming and shoves ~48 MiB " +
			"into one unary BatchUpdateBlobs")
	}

	if got > 64<<20 {
		t.Fatalf("max_batch_total_size_bytes = %d exceeds 64 MiB: moon cannot decode its "+
			"own BatchReadBlobs response", got)
	}

	if got != maxBatchTotalSizeBytes {
		t.Fatalf("max_batch_total_size_bytes = %d, want %d", got, maxBatchTotalSizeBytes)
	}
}

func TestCapabilities_ApiVersionPairIsSet(t *testing.T) {
	caps := capabilitiesPayload()

	low, high := caps.GetLowApiVersion(), caps.GetHighApiVersion()
	if low == nil || high == nil {
		t.Fatal("low/high api version unset: 0.0.0..0.0.0 intersects nothing with Bazel's " +
			"[2.0, 2.11] and kills the channel before any cache RPC")
	}

	if low.GetMajor() != 2 || low.GetMinor() != 0 {
		t.Errorf("low api version = %d.%d, want 2.0", low.GetMajor(), low.GetMinor())
	}

	if high.GetMajor() != 2 || high.GetMinor() != 3 {
		t.Errorf("high api version = %d.%d, want 2.3", high.GetMajor(), high.GetMinor())
	}
}

func TestCapabilities_DigestAndCompressorsAndNoExecution(t *testing.T) {
	caps := capabilitiesPayload()
	cc := caps.GetCacheCapabilities()

	if len(cc.GetDigestFunctions()) != 1 || cc.GetDigestFunctions()[0] != repb.DigestFunction_SHA256 {
		t.Errorf("digest_functions = %v, want [SHA256]", cc.GetDigestFunctions())
	}

	for _, list := range [][]repb.Compressor_Value{
		cc.GetSupportedCompressors(), cc.GetSupportedBatchUpdateCompressors(),
	} {
		if len(list) != 1 || list[0] != repb.Compressor_IDENTITY {
			t.Errorf("compressors = %v, want [IDENTITY]", list)
		}
	}

	if cc.GetSymlinkAbsolutePathStrategy() != repb.SymlinkAbsolutePathStrategy_ALLOWED {
		t.Error("symlink_absolute_path_strategy != ALLOWED")
	}

	if caps.GetExecutionCapabilities() != nil {
		t.Error("execution_capabilities set: a CAS+AC-only endpoint must not advertise it")
	}
}

func TestGetCapabilities_UnconfiguredBackendIsNotFound(t *testing.T) {
	// A resolver that knows a DIFFERENT project. GetCapabilities for the unknown one is
	// NotFound, indistinguishable from "no such project" -- and it holds even before auth.
	res := fakeResolver{org: "acme", project: "widget", route: testRoute(false), ok: true}
	b, _, _ := testBackend(t, res, fakeAuthn{})

	_, err := b.GetCapabilities(bearerCtx("bkry_x"), &repb.GetCapabilitiesRequest{InstanceName: "acme/other"})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", grpcstatus.Code(err))
	}
}

func TestGetCapabilities_OpenMirrorSucceeds(t *testing.T) {
	res := fakeResolver{org: "acme", project: "widget", route: testRoute(false), ok: true}
	b, _, _ := testBackend(t, res, fakeAuthn{})

	got, err := b.GetCapabilities(context.Background(), &repb.GetCapabilitiesRequest{InstanceName: "acme/widget"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if got.GetCacheCapabilities() == nil {
		t.Fatal("nil cache_capabilities")
	}
}
