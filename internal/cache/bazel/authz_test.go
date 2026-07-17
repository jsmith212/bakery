package bazel

import (
	"context"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// callAuthorize drives one RPC purely for its authorization outcome, against a
// ReadAuthRequired backend. Only the auth CODES matter here: NotFound (unknown
// instance), Unauthenticated (no/invalid credential where required), PermissionDenied
// (authenticated, not write-scoped, on a write RPC). An authorized RPC returns a
// non-auth outcome, which this asserts is NONE of those three codes.
//
// It exercises every one of the eight RPCs -- the fence against a handler that forgot
// to call authorize(). ByteStream Read/Write carry their instance in the resource
// name, not an instance_name field, so they build one that resolves.
func callRPC(b *Backend, ctx context.Context, rpc string, write bool) error {
	const inst = "acme/widget"
	res := "acme/widget/blobs/" + emptySHA256Hex + "/0"
	ures := "acme/widget/uploads/u/blobs/" + emptySHA256Hex + "/0"

	switch rpc {
	case "GetCapabilities":
		_, err := b.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: inst})
		return err
	case "GetActionResult":
		_, err := b.GetActionResult(ctx, &repb.GetActionResultRequest{
			InstanceName: inst, ActionDigest: &repb.Digest{Hash: emptySHA256Hex},
		})
		return err
	case "UpdateActionResult":
		_, err := b.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
			InstanceName: inst, ActionDigest: &repb.Digest{Hash: emptySHA256Hex},
			ActionResult: &repb.ActionResult{ExitCode: 1},
		})
		return err
	case "FindMissingBlobs":
		_, err := b.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: inst})
		return err
	case "BatchReadBlobs":
		_, err := b.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: inst})
		return err
	case "BatchUpdateBlobs":
		_, err := b.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: inst})
		return err
	case "Read":
		return b.Read(&bspb.ReadRequest{ResourceName: res}, &fakeReadServer{ctx: ctx})
	case "Write":
		return b.Write(&fakeWriteServer{ctx: ctx, frames: []*bspb.WriteRequest{
			{ResourceName: ures, FinishWrite: true},
		}})
	}

	panic("unknown rpc " + rpc)
}

func TestAuthorizationMatrix(t *testing.T) {
	// A ReadAuthRequired backend: reads need a read key, writes need a write key.
	res := fakeResolver{org: "acme", project: "widget", route: testRoute(true), ok: true}
	authn := fakeAuthn{byToken: map[string]fakePrincipal{
		"bkry_read":  {read: true, write: false},
		"bkry_write": {read: true, write: true},
	}}

	rpcs := []struct {
		name  string
		write bool
	}{
		{"GetCapabilities", false},
		{"GetActionResult", false},
		{"FindMissingBlobs", false},
		{"BatchReadBlobs", false},
		{"Read", false},
		{"UpdateActionResult", true},
		{"BatchUpdateBlobs", true},
		{"Write", true},
	}

	creds := []struct {
		name string
		ctx  context.Context
	}{
		{"no credential", context.Background()},
		{"read key", bearerCtx("bkry_read")},
		{"write key", bearerCtx("bkry_write")},
	}

	for _, rpc := range rpcs {
		for _, cred := range creds {
			t.Run(rpc.name+"/"+cred.name, func(t *testing.T) {
				b, _, _ := testBackend(t, res, authn)
				code := grpcstatus.Code(callRPC(b, cred.ctx, rpc.name, rpc.write))

				want, denied := deniedCode(rpc.write, cred.name)
				if denied {
					if code != want {
						t.Fatalf("%s with %s: code = %v, want %v", rpc.name, cred.name, code, want)
					}

					return
				}

				// Authorized: the RPC got past authorize. Its outcome may be OK, a miss
				// (NotFound), or a read-only-fixture Internal on a write -- but it must
				// NEVER be an auth-denial code, or a handler skipped authorize().
				if code == codes.Unauthenticated || code == codes.PermissionDenied {
					t.Fatalf("%s with %s: code = %v, but this credential is authorized",
						rpc.name, cred.name, code)
				}
			})
		}
	}
}

// TestAuthenticate_BasicCredential proves the Basic path works end-to-end and offers
// the password field first: a token in EITHER field authenticates, mirroring
// AuthenticateCache. A Bakery credential is one opaque token, so there is no id:secret.
func TestAuthenticate_BasicCredential(t *testing.T) {
	authn := fakeAuthn{byToken: map[string]fakePrincipal{"bkry_tok": {read: true, write: true}}}
	b := &Backend{authn: authn}

	// token in the password field
	if _, err := b.authenticate(basicCtx("ignored", "bkry_tok")); err != nil {
		t.Errorf("token in password field: %v", err)
	}

	// token in the username field (empty password)
	if _, err := b.authenticate(basicCtx("bkry_tok", "")); err != nil {
		t.Errorf("token in username field: %v", err)
	}

	// no token anywhere -> error (which authorize renders Unauthenticated)
	if _, err := b.authenticate(basicCtx("nope", "alsono")); err == nil {
		t.Error("expected an error when neither field holds a token")
	}
}

// deniedCode gives the expected auth-denial code for (write?, credential) against a
// ReadAuthRequired backend, or denied=false when the credential is authorized.
func deniedCode(write bool, cred string) (codes.Code, bool) {
	switch {
	case cred == "no credential":
		return codes.Unauthenticated, true
	case write && cred == "read key":
		return codes.PermissionDenied, true
	default:
		return codes.OK, false
	}
}
