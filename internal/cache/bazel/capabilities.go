package bazel

import (
	"context"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
)

// maxBatchTotalSizeBytes is what we advertise as max_batch_total_size_bytes, and it
// is a CORRECTNESS switch, not a hint. Advertise 0 (or omit it -- proto3 cannot tell
// them apart) and moon stops streaming entirely and shoves a ~48 MiB blob into one
// unary BatchUpdateBlobs, then misreports the RESOURCE_EXHAUSTED as "out of storage
// space". The ceiling is 64 MiB (moon decodes its own responses with a 64 MiB limit);
// 4 MiB is the defensible default the grpc server's 16 MiB MaxRecvMsgSize covers with
// margin.
const maxBatchTotalSizeBytes = 4 << 20

// GetCapabilities is the first RPC of every build. It resolves the route (NotFound
// for an unconfigured backend, exactly as every other RPC) and returns a fixed
// payload. Three fields are silent-catastrophe fields with their own regression
// tests: UpdateEnabled, MaxBatchTotalSizeBytes, and the low/high SemVer pair.
func (b *Backend) GetCapabilities(
	ctx context.Context, req *repb.GetCapabilitiesRequest,
) (*repb.ServerCapabilities, error) {
	if _, err := b.authorize(ctx, req.GetInstanceName(), false); err != nil {
		return nil, err
	}

	return capabilitiesPayload(), nil
}

// capabilitiesPayload is the exact ServerCapabilities Bakery advertises.
//
//   - DigestFunctions must contain SHA256 or moon disables the backend outright.
//   - UpdateEnabled is a client-side kill switch: false makes moon upload every CAS
//     blob and then SKIP UpdateActionResult with a debug log -- a 0% hit rate,
//     forever, with no warning.
//   - SupportedCompressors / SupportedBatchUpdateCompressors are IDENTITY only:
//     advertising a compressor OBLIGES us to serve it, in two incompatible framings.
//   - NO ExecutionCapabilities: the proto's own guidance for a CAS+AC-only endpoint.
//     The Execution service is simply never registered; grpc-go answers UNIMPLEMENTED.
//   - Low/High api versions MUST be set: unset is 0.0.0..0.0.0, which intersects
//     nothing with Bazel's [2.0, 2.11] and kills the channel before any cache RPC.
func capabilitiesPayload() *repb.ServerCapabilities {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions:               []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
			ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true},
			MaxBatchTotalSizeBytes:        maxBatchTotalSizeBytes,
			SymlinkAbsolutePathStrategy:   repb.SymlinkAbsolutePathStrategy_ALLOWED,
			SupportedCompressors:          []repb.Compressor_Value{repb.Compressor_IDENTITY},
			SupportedBatchUpdateCompressors: []repb.Compressor_Value{
				repb.Compressor_IDENTITY,
			},
		},
		LowApiVersion:  &semver.SemVer{Major: 2, Minor: 0},
		HighApiVersion: &semver.SemVer{Major: 2, Minor: 3},
	}
}
