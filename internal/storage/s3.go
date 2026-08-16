package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Object layout in the bucket, under the (possibly empty) configured prefix:
//
//	{prefix}objects/{64hex}   the object, named by its full hex digest
//	{prefix}staging/{32hex}   a partial write, never at a content address
//
// NO FAN-OUT. Local's two levels of 256-wide directories exist because readdir
// is the GC's bottleneck on a filesystem; S3 has no directories, its index is
// already a sorted key range, and a fan-out would only make every key longer.
//
// THE STAGING KEY IS FORCED BY THE PROTOCOL, NOT CHOSEN. The digest is not
// known until the last byte is written, and CreateMultipartUpload demands a key
// up front -- so a direct-to-final-key write is impossible without buffering
// the whole object first, which the package doc forbids. It is also what makes
// `defer w.Abort()` safe: an aborted writer deletes only its own staging key
// and can never unlink live, deduped bytes at a content address.
const (
	s3ObjectsPrefix = "objects/"
	s3StagingPrefix = "staging/"

	// CopyObject's hard ceiling. Above it S3 requires a multipart copy
	// (UploadPartCopy per range), which is why copyToFinal branches on size.
	s3MaxSingleCopyBytes int64 = 5 << 30

	// Range size for the multipart-copy path. 1 GiB keeps the part count for a
	// 5 TiB object (S3's object ceiling) inside the 10,000-part limit with room
	// to spare, and every part is a server-side range copy -- no bytes cross
	// this process.
	s3CopyPartSize int64 = 1 << 30

	// Bounds the boot-time credential resolution + HeadBucket probe. An
	// unreachable endpoint or an IMDS lookup with nowhere to go must be a loud,
	// bounded boot failure, never a hang -- the same discipline
	// TestBootRejectsAnUnusableStorageDir enforces for NewLocal.
	s3ProbeTimeout = 30 * time.Second

	// Bounds Abort's cleanup DELETE. Abort runs on the error and dedup paths
	// where the caller's context is frequently already cancelled, so the
	// cleanup runs on a detached context (see s3Writer.ctx) and needs its own
	// bound.
	s3AbortTimeout = 15 * time.Second

	// MaxAttempts for THE CALLS THAT RUN INSIDE A TRANSACTION, HOLDING THE
	// DIGEST ADVISORY LOCK. There are two of them and the count is the whole
	// point:
	//
	//  1. Delete, from blob.Service.ReapDigest.
	//  2. HeadObject on the final key, from s3Writer.Commit -- the F1
	//     re-assertion, which runs on EVERY PUT and is therefore the far more
	//     frequent of the two.
	//
	// The SDK's standard retryer is up to 3 attempts with backoff; on a wedged
	// endpoint that is the lock held for seconds while every PUT of that digest
	// queues behind it, and one of 16 pool connections held with it. Commit
	// heading on the default retryer was exactly the convoy this constant was
	// introduced to avoid, on the hotter call. The GC re-drives its queue and a
	// failed PUT is retried by the client, so a failure here is cheap and a held
	// lock is not.
	s3InLockMaxAttempts = 2
)

// errAbortedWrite unblocks the uploader goroutine when a staged write is
// abandoned. It never escapes Abort: closing the pipe with an error makes the
// SDK's read fail, which is what makes manager.Uploader abort its own multipart
// upload rather than leave parts behind.
var errAbortedWrite = errors.New("storage: staged write aborted")

// S3Config is everything the driver needs. Deliberately NO credential fields:
// the standard AWS chain (environment, shared config files, IMDS/IRSA/EKS pod
// identity) resolves them, exactly as the OCI backend's upstream credentials
// are server-level environment and never plumbed through the database.
type S3Config struct {
	Bucket string // required
	Region string
	// Endpoint overrides the AWS endpoint entirely -- minio, Ceph, Garage, R2.
	Endpoint string
	// ForcePathStyle addresses the bucket as {endpoint}/{bucket}/{key} rather
	// than as a virtual host. Required by minio and most self-hosted gateways.
	ForcePathStyle bool
	// Prefix lets one bucket hold several environments. Normalized to either ""
	// or a single-trailing-slash form.
	Prefix string

	// HTTPClient overrides the SDK's transport. Production leaves it nil; the
	// conformance tests install a recorder so
	// TestS3_CommitIssuesOnlyConstantTimeCallsWhenObjectPresent can assert on
	// the requests Commit actually makes.
	HTTPClient aws.HTTPClient
}

// S3 is a Store over an S3-compatible bucket.
//
// THE PUBLICATION POINT DIFFERS FROM LOCAL, DELIBERATELY, AND THE Writer DOC
// CARRIES THE CONTRACT. Local renames into place in Commit; S3 copies
// staging -> objects/{hex} in SYNC, because Commit runs inside the caller's
// transaction holding the digest advisory lock and one of 16 pool connections
// (blob.Service.put), and a same-bucket copy of a multi-hundred-MB sstate
// tarball is seconds -- precisely the starvation the Sync/Commit split exists
// to prevent.
//
// Commit is not therefore a no-op: it re-asserts the final key's presence with
// ONE HeadObject under the lock and re-copies from the RETAINED staging key if
// a concurrent ReapDigest deleted it in the window between Sync and the lock.
// Without that, a live blobs row could name bytes that are gone -- dangling
// metadata, a permanent 500, the forbidden side of the ordering invariant.
type S3 struct {
	client *s3.Client
	// inLock is the same config with a TIGHTER RETRY CEILING, and it is the
	// client every call made under the digest advisory lock must use: the
	// staging/object DELETEs and Commit's HeadObject. See s3InLockMaxAttempts.
	inLock *s3.Client
	bucket string
	prefix string

	// Copy thresholds, fields rather than constants so a test can exercise the
	// multipart-copy branch without a 5 GiB object.
	maxSingleCopy int64
	copyPartSize  int64
}

var _ Store = (*S3)(nil)

// NewS3 resolves credentials, builds the clients and PROBES THE BUCKET.
//
// The HeadBucket probe is the point: without it an unreachable endpoint, a
// misspelled bucket or an unresolvable credential chain surfaces as a 500 on
// the first cache write, hours later, instead of as a refused boot.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("storage: s3 bucket is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, s3ProbeTimeout)
	defer cancel()

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	if cfg.HTTPClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(cfg.HTTPClient))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("resolve aws configuration: %w", err)
	}

	clientOpts := func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}

		o.UsePathStyle = cfg.ForcePathStyle
	}

	s := &S3{
		client: s3.NewFromConfig(awsCfg, clientOpts),
		inLock: s3.NewFromConfig(awsCfg, clientOpts, func(o *s3.Options) {
			o.RetryMaxAttempts = s3InLockMaxAttempts
		}),
		bucket:        cfg.Bucket,
		prefix:        normalizeS3Prefix(cfg.Prefix),
		maxSingleCopy: s3MaxSingleCopyBytes,
		copyPartSize:  s3CopyPartSize,
	}

	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return nil, fmt.Errorf("probe s3 bucket %q: %w", s.bucket, err)
	}

	return s, nil
}

// Bucket and Prefix are for diagnostics and the boot log line.
func (s *S3) Bucket() string { return s.bucket }
func (s *S3) Prefix() string { return s.prefix }

// normalizeS3Prefix accepts "", "env1", "env1/" and "/env1/" alike and yields
// "" or "env1/". A prefix that silently gained or lost a slash between two
// deployments would address a different key space for the same digest.
func normalizeS3Prefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}

	return p + "/"
}

// objectKey is a pure FUNCTION of the digest -- the same reason no
// storage_path column exists in the schema.
func (s *S3) objectKey(k Key) string { return s.prefix + s3ObjectsPrefix + k.String() }

func (s *S3) stagingKey(id string) string { return s.prefix + s3StagingPrefix + id }

func (s *S3) Create(ctx context.Context) (Writer, error) {
	id, err := newStagingID()
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	w := &s3Writer{
		store: s,
		// RETAINED ON PURPOSE. The uploader goroutine must be cancellable by the
		// caller's context (a client that hangs up must not leave an upload
		// running), and Abort must NOT be -- it runs on exactly the paths where
		// that context is already dead, so it detaches with WithoutCancel below.
		ctx:        ctx,
		key:        s.stagingKey(id),
		pw:         pw,
		h:          sha256.New(),
		n:          0,
		uploadDone: make(chan struct{}),
		uploadErr:  nil,
		modTime:    time.Time{},
		synced:     false,
		done:       false,
	}

	// manager.Uploader, driven off a pipe: io.Copy hands us ~32 KiB at a time
	// and S3 demands >=5 MiB parts, so part sizing, bounded concurrency,
	// checksums, retry-with-renumbering and abort-on-error are all things the
	// SDK already implements correctly. Memory is bounded by
	// PartSize x Concurrency (5 MiB x 5), so a multi-GB sstate tarball is
	// buffered neither in memory nor on local disk.
	//
	//nolint:staticcheck // SA1019: feature/s3/manager is superseded by
	// feature/s3/transfermanager, which is still pre-1.0 (v0.x). The wave-1
	// spec names manager.Uploader; revisit when transfermanager reaches v1.
	uploader := manager.NewUploader(s.client)

	go func() {
		defer close(w.uploadDone)

		//nolint:staticcheck // SA1019, and see the note on NewUploader above.
		_, err := uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(w.key),
			Body:   pr,
		})
		if err != nil {
			w.uploadErr = fmt.Errorf("upload staged object: %w", err)
		}

		// Unblocks a Write that is still feeding a pipe nobody is reading.
		_ = pr.CloseWithError(err)
	}()

	return w, nil
}

func (s *S3) Get(ctx context.Context, k Key) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(k)),
	})
	if err != nil {
		return nil, s.mapErr("get", k, err)
	}

	return out.Body, nil
}

func (s *S3) Stat(ctx context.Context, k Key) (Info, error) {
	info, err := s.head(ctx, s.objectKey(k))
	if err != nil {
		return Info{}, s.mapErr("stat", k, err)
	}

	info.Key = k

	return info, nil
}

func (s *S3) Exists(ctx context.Context, k Key) (bool, error) {
	if _, err := s.head(ctx, s.objectKey(k)); err != nil {
		if s3IsNotFound(err) {
			return false, nil
		}

		return false, s.mapErr("stat", k, err)
	}

	return true, nil
}

// Delete removes the bytes. S3 answers a DELETE of an absent key with success,
// so the idempotence the GC's re-driven queue depends on needs no special
// casing -- unlike Local's fs.ErrNotExist check.
//
// It runs on the tighter-retry client: see s3InLockMaxAttempts.
func (s *S3) Delete(ctx context.Context, k Key) error {
	if _, err := s.inLock.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(k)),
	}); err != nil {
		return s.mapErr("delete", k, err)
	}

	return nil
}

// head is the raw HeadObject, on a KEY rather than a digest, because Commit
// heads the final key and Abort/cleanup work on staging keys.
//
// It runs on the DEFAULT client: Stat and Exists are ordinary read paths outside
// any transaction, where the standard retryer is what you want. The one caller
// that runs under the digest advisory lock takes headInLock below instead.
func (s *S3) head(ctx context.Context, key string) (Info, error) {
	return s.headOn(ctx, s.client, key)
}

// headInLock is head on the tighter-retry client. Commit's presence re-assertion
// is its only caller, and it is the second of the two calls s3InLockMaxAttempts
// exists to bound.
func (s *S3) headInLock(ctx context.Context, key string) (Info, error) {
	return s.headOn(ctx, s.inLock, key)
}

func (s *S3) headOn(ctx context.Context, client *s3.Client, key string) (Info, error) {
	out, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Info{}, err
	}

	info := Info{Key: Key{}, Size: 0, ModTime: time.Time{}}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	if out.LastModified != nil {
		info.ModTime = *out.LastModified
	}

	return info, nil
}

// deleteKey removes an arbitrary key (a staging key). Absent is success.
func (s *S3) deleteKey(ctx context.Context, key string) error {
	if _, err := s.inLock.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete staging key: %w", err)
	}

	return nil
}

// copyToFinal publishes the staged bytes at their content address with a
// SERVER-SIDE copy: no bytes traverse this process, and none leave the bucket.
//
// The staging key is deliberately NOT deleted here. It is what Commit re-copies
// from when a concurrent reap wins the race between Sync and the digest lock.
func (s *S3) copyToFinal(ctx context.Context, staging string, k Key, size int64) (time.Time, error) {
	dst := s.objectKey(k)
	src := s3CopySource(s.bucket, staging)

	if size <= s.maxSingleCopy {
		out, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(dst),
			CopySource: aws.String(src),
		})
		if err != nil {
			return time.Time{}, fmt.Errorf("copy staged object to %s: %w", k, err)
		}

		if out.CopyObjectResult != nil && out.CopyObjectResult.LastModified != nil {
			return *out.CopyObjectResult.LastModified, nil
		}

		return time.Now().UTC(), nil
	}

	return s.copyToFinalMultipart(ctx, src, dst, k, size)
}

// copyToFinalMultipart is the >5 GiB branch: CopyObject's ceiling, above which
// S3 requires a multipart copy driven one byte range at a time. Still entirely
// server-side.
func (s *S3) copyToFinalMultipart(
	ctx context.Context, src, dst string, k Key, size int64,
) (modTime time.Time, err error) {
	create, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(dst),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("start multipart copy of %s: %w", k, err)
	}

	uploadID := create.UploadId

	defer func() {
		if err == nil {
			return
		}

		// Leaving parts behind would bill the operator for bytes no key names.
		// The bucket's AbortIncompleteMultipartUpload lifecycle rule (see
		// docs/deploy/s3.md) is the backstop, not the plan.
		_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(dst),
			UploadId: uploadID,
		})
	}()

	var parts []types.CompletedPart

	for offset, num := int64(0), int32(1); offset < size; num++ {
		end := min(offset+s.copyPartSize, size) - 1

		out, cErr := s.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket:          aws.String(s.bucket),
			Key:             aws.String(dst),
			UploadId:        uploadID,
			PartNumber:      aws.Int32(num),
			CopySource:      aws.String(src),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", offset, end)),
		})
		if cErr != nil {
			return time.Time{}, fmt.Errorf("copy part %d of %s: %w", num, k, cErr)
		}

		part := types.CompletedPart{PartNumber: aws.Int32(num)}
		if out.CopyPartResult != nil {
			part.ETag = out.CopyPartResult.ETag
		}

		parts = append(parts, part)
		offset = end + 1
	}

	if _, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(dst),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		return time.Time{}, fmt.Errorf("complete multipart copy of %s: %w", k, err)
	}

	return time.Now().UTC(), nil
}

// mapErr turns an SDK error into a sentinel. Every error carries the key, never
// the bucket path -- the path is derivable and the key is what Postgres knows.
func (s *S3) mapErr(op string, k Key, err error) error {
	if s3IsNotFound(err) {
		return fmt.Errorf("%s %s: %w", op, k, ErrNotFound)
	}

	return fmt.Errorf("%s %s: %w", op, k, err)
}

// s3IsNotFound distinguishes "the object is not there" (a MISS, and on a cold
// cache that is every request) from every other failure.
//
// A MISSING BUCKET IS NOT A MISS. NoSuchBucket comes back as a 404 too, and
// mapping it to ErrNotFound would render a destroyed or misconfigured bucket as
// a serenely healthy cache with a 0% hit rate.
func s3IsNotFound(err error) bool {
	if err == nil {
		return false
	}

	var noBucket *types.NoSuchBucket
	if errors.As(err, &noBucket) {
		return false
	}

	var (
		noKey    *types.NoSuchKey
		notFound *types.NotFound
	)

	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchBucket":
			return false
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	// HeadObject has no response body, so the SDK cannot always name the error:
	// a bare 404 is the only thing it can report.
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == http.StatusNotFound
	}

	return false
}

// s3CopySource builds the `x-amz-copy-source` value. Each path segment is
// escaped independently: PathEscape would eat the separators, and the operator's
// configured prefix is not guaranteed to be URL-safe.
func s3CopySource(bucket, key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}

	return url.PathEscape(bucket) + "/" + strings.Join(segs, "/")
}

func newStagingID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate staging id: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}

// s3Writer streams to a staging key while hashing, in one pass. It never
// buffers the object: an sstate tarball is multi-GB.
type s3Writer struct {
	store *S3

	// The caller's context, for the upload only. Cleanup detaches from it --
	// see Create.
	ctx context.Context //nolint:containedctx // the uploader goroutine outlives the Create call

	key string // the staging key
	pw  *io.PipeWriter
	h   hash.Hash
	n   int64

	uploadDone chan struct{}
	uploadErr  error
	uploadOnce sync.Once

	modTime time.Time
	synced  bool
	done    bool
}

var _ Writer = (*s3Writer)(nil)

func (w *s3Writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrCommitted
	}

	n, err := w.pw.Write(p)
	w.n += int64(n)

	// Hash exactly what reached the pipe, so a short write cannot produce a
	// digest that disagrees with the bytes.
	_, _ = w.h.Write(p[:n])

	if err != nil {
		// A broken pipe means the uploader died; its error is the real one.
		w.waitUpload()

		if w.uploadErr != nil {
			return n, w.uploadErr
		}

		return n, fmt.Errorf("write staged object: %w", err)
	}

	return n, nil
}

func (w *s3Writer) Digest() (Key, int64) {
	var k Key

	w.h.Sum(k[:0])

	return k, w.n
}

// Sync finishes the upload to the staging key and PUBLISHES the bytes at their
// content address with a server-side copy -- and RETAINS the staging key.
//
// This is where every size-proportional network call lives, and it is called by
// blob.Service.put OUTSIDE the transaction and BEFORE the digest advisory lock,
// which is the whole reason Writer splits Sync from Commit. Doing the copy in
// Commit instead would hold one of 16 pool connections and the digest lock
// across a multi-second copy of a multi-hundred-MB tarball.
//
// Retaining staging is not laziness: it is what Commit re-copies from when a
// concurrent ReapDigest deletes the final key in the window between here and
// the lock. Idempotent, and a no-op after Commit.
func (w *s3Writer) Sync() error {
	if w.done || w.synced {
		return nil
	}

	if err := w.finishUpload(); err != nil {
		return err
	}

	k, size := w.Digest()

	modTime, err := w.store.copyToFinal(w.ctx, w.key, k, size)
	if err != nil {
		return err
	}

	w.modTime = modTime
	w.synced = true

	return nil
}

// Commit re-asserts, under the caller's digest advisory lock, that the bytes are
// really at their content address -- and returns their Info.
//
// IT IS NOT A NO-OP, AND MUST NOT BECOME ONE. Sync publishes outside the lock,
// so between Sync and the lock a concurrent ReapDigest can pass its
// `pending_delete AND refcount = 0` recheck and delete the object. The PUT then
// takes the lock, finds no row, and writes a LIVE blobs row -- naming bytes that
// are gone. That is dangling metadata and a permanent 500, the forbidden side of
// the ordering invariant. One HeadObject re-establishes the mutual exclusion the
// lock is supposed to provide.
//
// The cost budget is respected because both calls it makes are CONSTANT TIME: a
// HeadObject and (on success) a DELETE of the staging key. The size-proportional
// re-copy happens only when the head says the object is gone, which is the rare
// race, never the common path.
//
// Both of them run on the TIGHT-RETRY client (s3InLockMaxAttempts). Constant time
// per attempt is only half a bound: on the SDK's default retryer a flapping
// endpoint turns this head into three attempts with exponential backoff, holding
// the advisory lock and a pool connection, on the call every single PUT makes.
//
// When Sync was never called (the interface says Sync is OPTIONAL), Commit pays
// the whole cost here rather than returning a wrong answer.
func (w *s3Writer) Commit(ctx context.Context) (Info, error) {
	if w.done {
		return Info{}, ErrCommitted
	}

	if !w.synced {
		if err := w.Sync(); err != nil {
			return Info{}, err
		}
	}

	k, size := w.Digest()

	info, err := w.store.headInLock(ctx, w.store.objectKey(k))

	switch {
	case err == nil:
		w.modTime = info.ModTime

	case s3IsNotFound(err):
		// A reap won the race. Re-publish from the retained staging key.
		modTime, cErr := w.store.copyToFinal(ctx, w.key, k, size)
		if cErr != nil {
			return Info{}, cErr
		}

		w.modTime = modTime

	default:
		return Info{}, fmt.Errorf("verify committed object %s: %w", k, err)
	}

	w.done = true

	// Only now is staging dead: the final key is proven present.
	//
	// Best-effort ON PURPOSE. The bytes are published and the caller is about to
	// write the metadata row that names them; failing the Commit here would roll
	// back a transaction over a cleanup. A leaked staging key is billing, not
	// correctness, and the bucket's staging/ expiry rule collects it
	// (docs/deploy/s3.md).
	_ = w.store.deleteKey(ctx, w.key)

	return Info{Key: k, Size: size, ModTime: w.modTime}, nil
}

// Abort discards the staged bytes. It touches the STAGING KEY ONLY and NEVER the
// content address: `defer w.Abort()` runs on the dedup path, where the object at
// the content address is live, referenced, and somebody else's.
//
// Idempotent, and a no-op after a successful Commit.
func (w *s3Writer) Abort() error {
	if w.done {
		return nil
	}

	w.done = true

	// Breaking the pipe makes the SDK's read fail, which makes manager.Uploader
	// abort its own multipart upload rather than leave parts behind.
	_ = w.pw.CloseWithError(errAbortedWrite)
	w.waitUpload()

	// Detached: Abort runs precisely on the paths where the caller's context is
	// already cancelled, and a cleanup that skips itself because the request
	// went away leaks an object per aborted upload.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), s3AbortTimeout)
	defer cancel()

	if err := w.store.deleteKey(ctx, w.key); err != nil {
		return err
	}

	return nil
}

// finishUpload closes the write half (EOF for the uploader) and joins the
// goroutine. Idempotent.
func (w *s3Writer) finishUpload() error {
	w.uploadOnce.Do(func() { _ = w.pw.Close() })
	w.waitUpload()

	return w.uploadErr
}

func (w *s3Writer) waitUpload() { <-w.uploadDone }
