package bazel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
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

	// A reachability TOUCH, not the rejected reachability SWEEP (spec 6.3): only on
	// the HIT path, only after the unmarshal above has already succeeded. See
	// touchOutputs.
	b.touchOutputs(ctx, route, &ar)

	return &ar, nil
}

// touchOutputs feeds every CAS digest this ActionResult HIT names into the aux
// reachability map (blob.Service.MarkAccessed), so an action Bazel keeps re-hitting
// keeps its outputs' accessed_at fresh even when it never fetches them again.
//
// WHY THIS EXISTS (spec 6.3, verified against Bazel 6.4/6.5/7.0/7.4/8.0 + moon
// master source, 2026-08-14): under --remote_download_minimal, Bazel's
// shouldDownloadOutput decision is purely local -- an AC hit's outputs are
// injectRemoteFile'd from THIS ActionResult's own metadata, with ZERO CAS contact.
// FindMissingBlobs is never called for them. So "hot AC, cold CAS" is the NORMAL
// steady state, not an edge case, and without this touch the CAS blobs age out from
// under a permanently-hot action -- which Bazel answers with a build abort and
// rewind (LostInputsEvent), never a clean miss.
//
// This is a TOUCH, not the reachability SWEEP the spec rejects (a mark-sweep would
// race a concurrent CAS write landing before UpdateActionResult commits). A touch
// carries none of that risk: the worst case of any race here is a missed touch,
// never a wrong delete. MarkAccessed is in-memory only -- no query added to this
// RPC -- and this runs once per RPC, well below the HEAD-storm budget that keyed
// the LRU-stamp design instead.
//
// Symlink outputs are excluded on purpose: OutputSymlink/OutputFileSymlink/
// OutputDirectorySymlink name a PATH, never a Digest -- there is nothing here for
// them to reference.
//
// IT DESCENDS ONE LEVEL INTO EVERY OutputDirectory, and that descent is the whole
// reason spec 6.3 is not satisfied by touching the ActionResult's own digests: a tree
// output names ONE digest here (the Tree blob) while the files it contains are named
// only INSIDE that blob. Under --remote_download_minimal Bazel injectRemoteFiles those
// contents straight out of the Tree's metadata with zero CAS contact, so touching the
// Tree alone keeps the index hot and lets everything it indexes age out -- the failure
// mode is a tree output whose Tree blob is fresh and whose file blobs are gone, which
// is a build abort and rewind rather than a miss. See touchTreeContents.
func (b *Backend) touchOutputs(ctx context.Context, route cache.Route, ar *repb.ActionResult) {
	touch := func(d *repb.Digest) {
		hash := d.GetHash()
		if hash == "" || isEmpty(hash, d.GetSizeBytes()) {
			return // never stored: no cache_objects row, nothing to touch
		}

		b.deps.Blobs.MarkAccessed(route.Ref(casNamespace, casKind, hash))
	}

	for _, f := range ar.GetOutputFiles() {
		touch(f.GetDigest())
	}

	for _, d := range ar.GetOutputDirectories() {
		touch(d.GetTreeDigest())
		b.touchTreeContents(ctx, route, d.GetTreeDigest(), touch)
	}

	touch(ar.GetStdoutDigest())
	touch(ar.GetStderrDigest())
}

// maxTouchTreeBytes bounds the ONE read this touch is allowed to do per output
// directory. A Tree is a flat list of Directory messages -- name, digest and mode per
// entry, no content -- so 4 MiB is on the order of a hundred thousand files in a single
// output tree; anything larger is pathological, and a GetActionResult is not the place
// to find out how large it can get. Over the cap the descent is skipped and the Tree
// digest's own touch (already taken above) stands.
const maxTouchTreeBytes = 4 << 20

// touchTreeContents reads one Tree blob out of the LOCAL store and marks every FileNode
// digest in its root and children.
//
// THE READ IS FREE BY CONSTRUCTION: the Tree is a CAS blob of THIS backend, and the
// ActionResult that names it was just served as a hit, so the blob is present and its
// metadata is in the LRU. blob.Service.Get is one metadata lookup (an LRU hit) plus one
// local file read.
//
// EVERY FAILURE IS A SILENT SKIP, deliberately. This is best-effort reachability
// bookkeeping on a read path whose only job is to answer Bazel: a missing tree, a
// storage error, a truncated read or an unparseable proto must never turn a cache HIT
// into an RPC error, and a MISSED TOUCH IS THE SAFE DIRECTION (spec 6.3 -- a touch only
// makes rows younger; the W_cas = 2 x W_ac ladder is the defence underneath, and the
// worst case of losing one is that a blob ages out a window early). The only thing that
// is logged is the oversized-tree skip, at debug, because that one is a policy decision
// rather than an accident.
func (b *Backend) touchTreeContents(
	ctx context.Context, route cache.Route, treeDigest *repb.Digest, touch func(*repb.Digest),
) {
	hash := treeDigest.GetHash()
	if hash == "" || isEmpty(hash, treeDigest.GetSizeBytes()) {
		return
	}

	if treeDigest.GetSizeBytes() > maxTouchTreeBytes {
		b.deps.Logger.DebugContext(ctx, "skipping reachability touch of an oversized tree",
			slog.String("org", route.Org), slog.String("project", route.Project),
			slog.String("tree", hash), slog.Int64("size", treeDigest.GetSizeBytes()))

		return
	}

	meta, rc, err := b.deps.Blobs.Get(ctx, route.Ref(casNamespace, casKind, hash))
	if err != nil {
		return
	}

	defer func() { _ = rc.Close() }()

	// The declared size is the client's; the stored size is ours. Cap the read on both,
	// so a Tree whose row disagrees with its digest cannot make this allocate more than
	// the cap.
	if meta.Size > maxTouchTreeBytes {
		return
	}

	data, err := io.ReadAll(io.LimitReader(rc, maxTouchTreeBytes))
	if err != nil {
		return
	}

	var tree repb.Tree

	if proto.Unmarshal(data, &tree) != nil {
		return
	}

	for _, f := range tree.GetRoot().GetFiles() {
		touch(f.GetDigest())
	}

	for _, dir := range tree.GetChildren() {
		for _, f := range dir.GetFiles() {
			touch(f.GetDigest())
		}
	}
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
