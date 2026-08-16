package storage

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jsmith212/bakery/internal/metrics"
)

// newS3Store builds a driver on the shared harness bucket under a key prefix
// nothing else uses. The prefix is per-store rather than per-process on purpose:
// it isolates tests from each other AND it means every single test here runs
// through the S3Prefix code path instead of leaving it dead config.
func newS3Store(t *testing.T, cfg ...func(*S3Config)) *S3 {
	t.Helper()

	srv := ensureMinio(t)

	c := S3Config{
		Bucket:         srv.bucket,
		Region:         srv.region,
		Endpoint:       srv.endpoint,
		ForcePathStyle: true, // minio, and every self-hosted gateway
		Prefix:         "t/" + randSuffix(),
		HTTPClient:     nil,
	}

	for _, f := range cfg {
		f(&c)
	}

	s, err := NewS3(t.Context(), c)
	if err != nil {
		t.Fatalf("NewS3() error = %v", err)
	}

	return s
}

// TestS3_Conformance runs the Store-interface-only suite (conformance_test.go)
// against a REAL S3 implementation. This is the gate the whole driver exists to
// pass: everything a caller of blob.Service may assume, proven against S3's own
// semantics rather than against a fake that would agree with whatever the driver
// happened to do.
func TestS3_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) Store { return newS3Store(t) }, metrics.DriverS3)
}

// TestS3_CommitRepublishesAfterAReap is THE F1 GATE, and it is the one test in
// this file that must never be "simplified" away.
//
// The interleaving it reproduces:
//
//  1. PUT: Sync publishes at the content address. NO LOCK IS HELD -- Sync runs
//     outside the transaction by design (blob.Service.put).
//  2. GC: ReapDigest takes the digest lock, passes its `pending_delete AND
//     refcount = 0` recheck, and deletes the object.
//  3. PUT: enters the transaction, takes the lock, finds no row, and Commits.
//
// If Commit were the no-op that "Sync already published it" suggests, step 3
// would write a LIVE blobs row naming bytes that are gone: dangling metadata,
// a permanent 500 on every subsequent GET, and the forbidden side of the
// ordering invariant. Commit's HeadObject + re-copy from the RETAINED staging
// key is what restores the mutual exclusion the advisory lock is supposed to
// provide.
func TestS3_CommitRepublishesAfterAReap(t *testing.T) {
	s := newS3Store(t)
	content := []byte("sstate:busybox-1.36.1")

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	defer func() { _ = w.Abort() }()

	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	k, size := w.Digest()

	// Step 1 really did publish: that is the ruling, and the rest of the test is
	// meaningless if it did not.
	if ok, err := s.Exists(t.Context(), k); !ok || err != nil {
		t.Fatalf("after Sync, Exists() = %v, %v; want true, nil -- S3 publishes at Sync", ok, err)
	}

	// Step 2: the reap. This is byte-for-byte what blob.Service.ReapDigest does
	// to the store between the recheck and the row delete.
	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if ok, err := s.Exists(t.Context(), k); ok || err != nil {
		t.Fatalf("after the reap, Exists() = %v, %v; want false, nil", ok, err)
	}

	// Step 3: Commit, inside the lock. It must republish, not shrug.
	info, err := w.Commit(t.Context())
	if err != nil {
		t.Fatalf("Commit() after a reap error = %v -- Commit must republish from the retained staging key", err)
	}

	if info.Key != k || info.Size != size {
		t.Errorf("Commit() = %s/%d, want %s/%d", info.Key, info.Size, k, size)
	}

	if ok, err := s.Exists(t.Context(), k); !ok || err != nil {
		t.Fatalf("after Commit, Exists() = %v, %v; want true, nil -- "+
			"a live metadata row would now name bytes that are gone", ok, err)
	}

	rc, err := s.Get(t.Context(), k)
	if err != nil {
		t.Fatalf("Get() after the republish error = %v", err)
	}

	got, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("republished object is %q, want %q", got, content)
	}
}

// TestS3_CommitIssuesOnlyConstantTimeCallsWhenObjectPresent is the OTHER half of
// the F1 correction, and it replaces the "Commit issues zero requests" gate the
// original memo proposed -- which would have enforced the dangling-metadata bug
// and made the correct implementation a review rejection.
//
// What actually matters is the COST BUDGET, not the request count: Commit runs
// inside the caller's transaction, holding the digest advisory lock and one of
// 16 pool connections, so it may make constant-time calls (a HeadObject, a
// staging DELETE) and must never make a size-proportional one. A CopyObject or
// an UploadPartCopy here would reopen exactly the starvation the Sync/Commit
// split was built to prevent.
func TestS3_CommitIssuesOnlyConstantTimeCallsWhenObjectPresent(t *testing.T) {
	rec := &recordingHTTPClient{inner: &http.Client{Timeout: 30 * time.Second}, mu: sync.Mutex{}, reqs: nil}
	s := newS3Store(t, func(c *S3Config) { c.HTTPClient = rec })

	// Big enough that a copy in Commit would be visibly size-proportional, and
	// multi-part on the upload side.
	content := bytes.Repeat([]byte("y"), 6<<20)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	defer func() { _ = w.Abort() }()

	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	// Everything above is outside the transaction and may cost whatever it
	// costs. Only what follows is under the lock.
	rec.reset()

	if _, err := w.Commit(t.Context()); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	got := rec.snapshot()

	for _, r := range got {
		if r.copySource != "" {
			t.Errorf("Commit issued a SERVER-SIDE COPY (%s %s, x-amz-copy-source=%q) -- "+
				"size-proportional work under the digest advisory lock", r.method, r.path, r.copySource)
		}

		switch r.method {
		case http.MethodHead, http.MethodDelete:
		default:
			t.Errorf("Commit issued %s %s; only HEAD (re-assert the final key) and "+
				"DELETE (drop staging) are constant-time enough to run under the lock", r.method, r.path)
		}
	}

	if len(got) != 2 {
		t.Fatalf("Commit issued %d requests (%v), want exactly 2: HeadObject on the final key, "+
			"then DeleteObject on staging", len(got), got)
	}

	if got[0].method != http.MethodHead {
		t.Errorf("Commit's first request is %s, want HEAD -- the presence re-assertion comes first, "+
			"because staging is what the re-copy reads from", got[0].method)
	}
}

// TestS3_CommitHeadIsBoundedByTheInLockRetryCeiling is the OTHER half of the same
// cost budget, and the half a request COUNT cannot see.
//
// Commit's HeadObject is constant-time per attempt; what is not bounded by that
// is how many attempts it makes. On the SDK's default retryer a flapping endpoint
// costs three attempts with exponential backoff -- while holding
// pg_advisory_xact_lock(digest) and one of 16 pool connections, on the call that
// runs on EVERY PUT rather than on the GC's occasional reap. That is exactly the
// convoy s3InLockMaxAttempts was introduced to prevent, and Commit was heading on
// the wrong client.
//
// The failing transport returns a retryable 503 for HEADs on the objects/ prefix
// only, so the attempt count is Commit's and nothing else's.
func TestS3_CommitHeadIsBoundedByTheInLockRetryCeiling(t *testing.T) {
	fail := &failingHeadHTTPClient{
		inner: &http.Client{Timeout: 30 * time.Second}, mu: sync.Mutex{}, armed: false, heads: 0,
	}
	s := newS3Store(t, func(c *S3Config) { c.HTTPClient = fail })

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	defer func() { _ = w.Abort() }()

	if _, err := w.Write([]byte("bounded")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	// Only Commit's HEAD is failed, and only its attempts are counted.
	fail.arm()

	if _, err := w.Commit(t.Context()); err == nil {
		t.Fatal("Commit() error = nil; the endpoint failed every HeadObject, so Commit must fail")
	}

	if got := fail.count(); got != s3InLockMaxAttempts {
		t.Errorf("Commit made %d HeadObject attempts, want %d (s3InLockMaxAttempts).\n"+
			"Commit runs inside the caller's transaction holding the digest advisory lock: "+
			"the SDK default of 3 attempts with backoff is the lock held for seconds while "+
			"every PUT of that digest queues behind it.", got, s3InLockMaxAttempts)
	}
}

// failingHeadHTTPClient fails HEADs on the objects/ prefix with a RETRYABLE 503
// once armed, and counts them. Everything else passes through, so the upload and
// the staging copy in Sync are untouched.
type failingHeadHTTPClient struct {
	inner aws.HTTPClient

	mu    sync.Mutex
	armed bool
	heads int
}

var _ aws.HTTPClient = (*failingHeadHTTPClient)(nil)

func (c *failingHeadHTTPClient) Do(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	armed := c.armed

	if armed && r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/"+s3ObjectsPrefix) {
		c.heads++
		c.mu.Unlock()

		return &http.Response{
			Status: "503 Service Unavailable", StatusCode: http.StatusServiceUnavailable,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")),
			ContentLength: 0, TransferEncoding: nil, Close: false, Uncompressed: false,
			Trailer: nil, Request: r, TLS: nil,
		}, nil
	}

	c.mu.Unlock()

	return c.inner.Do(r) //nolint:wrapcheck // a transport passthrough
}

func (c *failingHeadHTTPClient) arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *failingHeadHTTPClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.heads
}

// TestS3_AbortAfterCommitLeavesTheFinalObject: `defer w.Abort()` is
// unconditional in blob.Service.put and runs on every successful PUT. If Abort
// touched the content address, every successful upload would delete its own
// bytes and every later GET would be a permanent 500. Abort owns the STAGING
// KEY and nothing else.
func TestS3_AbortAfterCommitLeavesTheFinalObject(t *testing.T) {
	s := newS3Store(t)
	content := []byte("keep me")

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	staging := w.(*s3Writer).key //nolint:forcetypeassert // s.Create's concrete type

	info, err := w.Commit(t.Context())
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Errorf("Abort() after Commit error = %v, want nil", err)
	}

	if ok, err := s.Exists(t.Context(), info.Key); !ok || err != nil {
		t.Fatalf("after post-Commit Abort, Exists() = %v, %v; want true, nil", ok, err)
	}

	// A successful Commit deletes staging: the final key is proven present, so
	// the staged copy is dead weight the operator would otherwise pay for.
	assertKeyAbsent(t, s, staging)
}

// TestS3_SyncThenAbortLeavesOrphanedBytesNotDanglingMetadata documents the
// TOLERATED direction of the ruling, and it is a documenting test on purpose.
//
// S3 publishes at Sync, which runs before the metadata transaction. A
// transaction that then rolls back -- or a deduped write, where Commit is never
// called at all -- leaves bytes at a content address with no row naming them.
// That is an ORPHAN: recoverable, sweepable, and billed for until the operator's
// lifecycle rule or a future orphan sweep collects it. The direction the
// invariant forbids is the other one, a row naming absent bytes, and
// TestS3_CommitRepublishesAfterAReap is what holds that line.
func TestS3_SyncThenAbortLeavesOrphanedBytesNotDanglingMetadata(t *testing.T) {
	s := newS3Store(t)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write([]byte("orphan")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	k, _ := w.Digest()
	staging := w.(*s3Writer).key //nolint:forcetypeassert // s.Create's concrete type

	// The transaction rolled back; blob.Service.put's unconditional deferred
	// Abort runs.
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}

	if ok, err := s.Exists(t.Context(), k); !ok || err != nil {
		t.Errorf("after Sync+Abort, Exists() = %v, %v; want true, nil -- "+
			"Abort must never unlink a content address, which on the dedup path is somebody else's live object", ok, err)
	}

	assertKeyAbsent(t, s, staging)
}

// TestS3_LargeObjectPublishesViaMultipartCopy exercises the >5 GiB branch
// without a 5 GiB object, by moving the thresholds rather than the object:
// CopyObject's ceiling is a number, and everything about the branch -- the range
// arithmetic, the part numbering, the ETag collection, the Complete -- is
// identical at 5 MiB and at 5 GiB. Without this, the branch that runs on exactly
// the largest sstate tarballs is the one branch never executed.
func TestS3_LargeObjectPublishesViaMultipartCopy(t *testing.T) {
	rec := &recordingHTTPClient{inner: &http.Client{Timeout: 60 * time.Second}, mu: sync.Mutex{}, reqs: nil}
	s := newS3Store(t, func(c *S3Config) { c.HTTPClient = rec })

	// 5 MiB is S3's minimum size for a non-final part, so it is the smallest
	// legal stand-in for the real 5 GiB constants.
	s.maxSingleCopy = 5 << 20
	s.copyPartSize = 5 << 20

	content := bytes.Repeat([]byte("m"), 12<<20)
	k := put(t, s, content)

	var ranged int

	for _, r := range rec.snapshot() {
		if r.copySource != "" && strings.Contains(r.path, "partNumber=") {
			ranged++
		}
	}

	if ranged < 3 {
		t.Errorf("saw %d UploadPartCopy requests, want 3 (5+5+2 MiB) -- "+
			"the multipart-copy branch did not run", ranged)
	}

	rc, err := s.Get(t.Context(), k)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	got, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("multipart-copied object read back %d bytes, want %d", len(got), len(content))
	}
}

// TestS3_ConstructionProbesTheBucket: NewS3 HeadBuckets before it returns, so a
// misspelled bucket or an unresolvable credential chain is a refused boot rather
// than a 500 on the first cache write, hours later. Same discipline as
// TestBootRejectsAnUnusableStorageDir for NewLocal.
func TestS3_ConstructionProbesTheBucket(t *testing.T) {
	srv := ensureMinio(t)

	_, err := NewS3(t.Context(), S3Config{
		Bucket:         "bk-storage-does-not-exist-" + randSuffix(),
		Region:         srv.region,
		Endpoint:       srv.endpoint,
		ForcePathStyle: true,
		Prefix:         "",
		HTTPClient:     nil,
	})
	if err == nil {
		t.Fatal("NewS3() on an absent bucket returned nil error, want a loud boot failure")
	}

	if _, err := NewS3(t.Context(), S3Config{
		Bucket: "", Region: srv.region, Endpoint: srv.endpoint,
		ForcePathStyle: true, Prefix: "", HTTPClient: nil,
	}); err == nil {
		t.Error("NewS3() with an empty bucket returned nil error")
	}
}

// TestS3_MissingBucketIsNotAMiss: NoSuchBucket comes back as a 404 exactly like
// NoSuchKey. Mapping it to ErrNotFound would render a destroyed or misconfigured
// bucket as a serenely healthy cache with a 0% hit rate and no error metric at
// all -- the storage layer's version of the OCI backend's silent-fallback trap.
func TestS3_MissingBucketIsNotAMiss(t *testing.T) {
	srv := ensureMinio(t)

	// Built by hand: NewS3's probe would (correctly) refuse this bucket.
	client, err := srv.client(t.Context())
	if err != nil {
		t.Fatalf("client() error = %v", err)
	}

	s := &S3{
		client: client, inLock: client,
		bucket: "bk-storage-gone-" + randSuffix(), prefix: "",
		maxSingleCopy: s3MaxSingleCopyBytes, copyPartSize: s3CopyPartSize,
	}

	_, err = s.Get(t.Context(), KeyOf([]byte("anything")))
	if err == nil {
		t.Fatal("Get() against an absent bucket returned nil error")
	}

	if errors.Is(err, ErrNotFound) {
		t.Errorf("Get() against an ABSENT BUCKET = %v, and ErrNotFound means MISS -- "+
			"a destroyed bucket must be an error, not a cold cache", err)
	}
}

func TestNormalizeS3Prefix(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "/", want: ""},
		{in: "env1", want: "env1/"},
		{in: "env1/", want: "env1/"},
		{in: "/env1/", want: "env1/"},
		{in: "a/b", want: "a/b/"},
	}

	for _, tt := range tests {
		if got := normalizeS3Prefix(tt.in); got != tt.want {
			t.Errorf("normalizeS3Prefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// s3CopySource escapes each path SEGMENT: url.PathEscape over the whole string
// would eat the separators and address a key that does not exist, and the
// operator's configured prefix is not guaranteed to be URL-safe.
func TestS3CopySource(t *testing.T) {
	tests := []struct {
		bucket, key, want string
	}{
		{bucket: "bk", key: "staging/abc", want: "bk/staging/abc"},
		{bucket: "bk", key: "a b/staging/abc", want: "bk/a%20b/staging/abc"},
		{bucket: "bk", key: "p+q/objects/ff", want: "bk/p+q/objects/ff"},
	}

	for _, tt := range tests {
		if got := s3CopySource(tt.bucket, tt.key); got != tt.want {
			t.Errorf("s3CopySource(%q, %q) = %q, want %q", tt.bucket, tt.key, got, tt.want)
		}
	}
}

// assertKeyAbsent reaches past the Store interface at a raw key -- the only way
// to assert anything about staging, which the interface deliberately cannot
// name.
func assertKeyAbsent(t *testing.T, s *S3, key string) {
	t.Helper()

	_, err := s.client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Errorf("staging key %s still exists", key)

		return
	}

	if !s3IsNotFound(err) {
		t.Errorf("HeadObject(%s) error = %v, want a 404", key, err)
	}
}

// recordingHTTPClient counts what the driver puts on the wire. It is how
// TestS3_CommitIssuesOnlyConstantTimeCallsWhenObjectPresent asserts on the SHAPE
// of Commit's work rather than on its wall-clock time, which would be a flake.
type recordingHTTPClient struct {
	inner aws.HTTPClient

	mu   sync.Mutex
	reqs []recordedRequest
}

type recordedRequest struct {
	method     string
	path       string
	copySource string
}

var _ aws.HTTPClient = (*recordingHTTPClient)(nil)

func (c *recordingHTTPClient) Do(r *http.Request) (*http.Response, error) {
	rec := recordedRequest{
		method:     r.Method,
		path:       r.URL.RequestURI(),
		copySource: r.Header.Get("x-amz-copy-source"),
	}

	c.mu.Lock()
	c.reqs = append(c.reqs, rec)
	c.mu.Unlock()

	return c.inner.Do(r) //nolint:wrapcheck // a transport passthrough
}

func (c *recordingHTTPClient) reset() {
	c.mu.Lock()
	c.reqs = nil
	c.mu.Unlock()
}

func (c *recordingHTTPClient) snapshot() []recordedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedRequest(nil), c.reqs...)
}
