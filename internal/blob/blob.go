// Package blob is the load-bearing abstraction: every cache backend except
// hashserv routes through it, so dedup, refcounting, the LRU, singleflight and the
// hit/miss metrics are implemented exactly ONCE and every future backend is
// normalized for free.
//
// It is also, by construction, THE ONLY WRITER OF OBJECT METADATA. cache.Deps
// deliberately does not carry a *repository.Queries, so a backend physically cannot
// reach around this package to touch cache_objects or blobs.
//
// # The two invariants this package exists to hold
//
// ORDERING. On create: bytes first, then metadata. On delete: metadata first, then
// bytes. Orphaned bytes are wasteful and recoverable; dangling metadata is a
// permanent 500. Put therefore stages the bytes, hashes them, takes the digest
// advisory lock, and only makes the bytes durable (storage.Writer.Commit) INSIDE the
// transaction that writes the metadata -- before the metadata rows, always. Delete
// removes metadata and never touches bytes at all; only ReapDigest deletes bytes,
// and only for a digest Postgres has already agreed is unreferenced.
//
// REFCOUNT SAFETY. The digest advisory lock + SELECT ... FOR UPDATE + a
// refcount = 0 recheck. The grace period (which lives in the GC's mark predicate) is
// luck, not correctness, and is not sufficient on its own. Note what the FOR UPDATE
// in GetBlobForWrite actually buys: the GC's mark is `FOR UPDATE ... SKIP LOCKED`,
// so a blob a PUT has locked is SKIPPED by a concurrent mark. Remove the FOR UPDATE
// and the mark tombstones a blob a live PUT is about to reference -- the PUT's
// refcount trigger then hits blobs_pending_delete_is_unreferenced and the upload
// dies with a constraint violation, or worse, the GC unlinks bytes a committed
// object names.
//
// # Digest verification is a per-call flag, never a global truth
//
// /cas/ and OCI blobs verify key == sha256(body). sstate, downloads and /ac/ do NOT
// -- /ac/ is an OPAQUE byte store (ccache, sccache and moon-over-HTTP all put
// non-ActionResult payloads there), and hardcoding verification on breaks three
// clients at once while hardcoding it off silently accepts a poisoned CAS. So
// PutOptions.Verify has NO USABLE ZERO VALUE: a caller that does not say which it
// wants gets ErrVerificationUnspecified rather than a default that is wrong for half
// the backends.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// Digest is the sha256 of an object's content. It is storage.Key: there is one
// content address in this system, and the two layers agree on it by construction
// rather than by conversion.
type Digest = storage.Key

// Sentinel errors. Handlers map these to status codes; nothing else may.
var (
	// ErrNotFound is a cache MISS. Renders as 404 -- NEVER 403 (BitBake retries a
	// 403 as a full-body GET) and never a 200 with an empty body.
	ErrNotFound = errors.New("blob: object not found")

	// ErrDanglingMetadata means Postgres named bytes that storage does not have.
	// This is a permanent 500 and the failure the ordering invariant exists to
	// prevent; if it is ever seen in production, something wrote metadata before
	// bytes.
	ErrDanglingMetadata = errors.New("blob: metadata names bytes that are not in storage")

	// ErrVerificationUnspecified means the caller did not say whether key ==
	// sha256(body) must hold. There is no safe default. See the package doc.
	ErrVerificationUnspecified = errors.New("blob: put requires an explicit verification policy")

	// ErrDigestMismatch means VerifyDigest was requested and the body did not hash
	// to the expected digest. A 400 on /cas/, and a poisoned cache prevented.
	ErrDigestMismatch = errors.New("blob: content does not match the expected digest")

	// ErrSizeMismatch means a blob row already exists for this digest with a
	// different size. sha256 did not break; we did. Fail loudly.
	ErrSizeMismatch = errors.New("blob: digest already present with a different size")

	// ErrInvalidKey means the object key is empty or longer than the 1024 bytes the
	// cache_objects btree can hold. Caught here so it is a clean 400 rather than a
	// runtime index failure on INSERT.
	ErrInvalidKey = errors.New("blob: invalid object key")
)

// errRevived aborts the reap transaction when the recheck finds the blob revived. It
// is internal: a rollback here is a normal outcome, not a failure, and it never
// escapes ReapDigest.
var errRevived = errors.New("blob: blob was revived while queued for the digest lock")

// maxKeyLen mirrors the cache_objects CHECK. A btree entry may not exceed ~2704
// bytes; without the cap a long key is not a 400, it is an INSERT that fails at
// runtime in production. Observed sstate keys are 120-160 bytes.
const maxKeyLen = 1024

// defaultCacheSize is the LRU's entry ceiling. ~500k entries is a few tens of MB of
// metadata and covers a large sstate setscene graph end to end.
const defaultCacheSize = 1 << 19

// Ref names one object: a backend, a namespace within it, and the client's opaque
// key. It carries the org/project SLUGS as well, and only as METRICS LABELS.
//
// Those slugs are safe to label with precisely because a Ref is only ever built
// from a route that has already been resolved to a DB row -- never from a raw URL
// segment. That is the structural reason the headline series is emitted here and
// not from the HTTP middleware, which sees {org}/{project} as attacker-controlled
// path values and would mint a time series per garbage slug.
type Ref struct {
	// BackendID is cache_backends.id, from the in-process route cache.
	BackendID int64

	// Org and Project are slugs, for metrics only. Never used to query.
	Org     string
	Project string

	// Backend and Kind are the metrics labels: backend is sstate|downloads|bazel|oci,
	// kind is the sub-namespace (object|siginfo|file|ac|cas|blob|manifest).
	Backend metrics.Backend
	Kind    string

	// Namespace is the cache_objects discriminator: '' for sstate and downloads,
	// 'ac' / 'cas' for bazel, 'blobs' / 'manifests' for OCI. Without it /ac/<h> and
	// /cas/<h> -- both 64 hex, one verified and one not -- collide on (backend, key).
	Namespace string

	// Key is the CLIENT's key. Never assumed to equal the digest.
	Key string
}

// appendCacheKey builds the LRU / singleflight key.
//
// It is namespaced by backend_id, which makes cross-tenant cache poisoning
// impossible: two projects can hold the same sstate key, and a cache keyed on the
// bare key would serve one project's bytes to the other.
//
// It appends into a caller-provided buffer so the hot path can hand it a stack
// array and allocate nothing.
func (r Ref) appendCacheKey(dst []byte) []byte {
	dst = strconv.AppendInt(dst, r.BackendID, 36)
	dst = append(dst, 0)
	dst = append(dst, r.Namespace...)
	dst = append(dst, 0)
	dst = append(dst, r.Key...)

	return dst
}

func (r Ref) validate() error {
	if len(r.Key) == 0 {
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	}

	if len(r.Key) > maxKeyLen {
		return fmt.Errorf("%w: %d bytes, max %d", ErrInvalidKey, len(r.Key), maxKeyLen)
	}

	return nil
}

// Meta is what we know about an object without reading it.
//
// EXISTS IS AN EXPLICIT FIELD, distinct from "not cached". A Meta{Exists: false} is
// a cached NEGATIVE result, and caching those is what stops the first build against
// an empty cache from stampeding Postgres with its entire setscene graph.
type Meta struct {
	Exists    bool
	Digest    Digest
	Size      int64
	UpdatedAt time.Time
}

// Verify is the per-call digest-verification policy. Construct it with NoVerify or
// VerifyDigest; the zero value is INVALID ON PURPOSE.
type Verify struct {
	mode verifyMode
	want Digest
}

type verifyMode uint8

const (
	verifyUnspecified verifyMode = iota
	verifyNone
	verifyContent
)

// NoVerify: the key is opaque and the body is whatever the client sent. sstate,
// downloads, and /ac/ (ccache, sccache, moon-over-HTTP).
func NoVerify() Verify { return Verify{mode: verifyNone, want: Digest{}} }

// VerifyDigest: the body MUST hash to want, or the write is rejected. /cas/ and OCI
// blobs. The caller parses want out of the client's key; blob does not guess at key
// grammars.
func VerifyDigest(want Digest) Verify { return Verify{mode: verifyContent, want: want} }

// PutOptions is the per-call policy. Both fields are per-backend facts from the
// Policy table, not global truths.
type PutOptions struct {
	// Overwrite repoints an existing key at new content. TRUE ONLY FOR /ac/, the one
	// mutable namespace. The refcount trigger does the decrement-old/increment-new
	// atomically; Go never does that arithmetic.
	Overwrite bool

	// Verify has no usable zero value. See the package doc.
	Verify Verify
}

// PutResult reports what the write actually did.
type PutResult struct {
	Digest Digest
	Size   int64

	// Deduped: the bytes were already durably in storage, so the upload was staged,
	// hashed, and discarded. This is the `put`/`hit` result on the headline series.
	Deduped bool

	// Created: the metadata row is ours. False on an immutable key that already
	// existed (DO NOTHING: someone else won the race, and their row stands).
	Created bool
}

// Reader is the READ half of the metadata store: one query, the hot one.
//
// A consumer-side interface, deliberately narrow: it is what lets the HEAD-path
// gates run against a hand-written fake that counts queries, which is the only way
// to assert "an LRU hit issues ZERO Postgres queries" -- the single most important
// property in M1.
//
// *db.Store satisfies it -- it embeds *repository.Queries, so widening this
// interface by a generated method costs the production type nothing.
type Reader interface {
	StatObject(ctx context.Context, arg repository.StatObjectParams) (repository.StatObjectRow, error)

	// StatObjectsBatch is the ExistsBatch hot path: REAPI FindMissingBlobs asks
	// about N keys in one RPC, and this answers them in ONE query. It returns only
	// the keys that exist; a requested key absent from the result is a miss.
	StatObjectsBatch(
		ctx context.Context, arg repository.StatObjectsBatchParams,
	) ([]repository.StatObjectsBatchRow, error)
}

// Txer is the WRITE half. It hands the closure a *repository.Queries REBOUND ONTO
// THE TRANSACTION -- which is the whole point, because an advisory xact lock and a
// FOR UPDATE taken in different transactions protect nothing.
//
// *db.Store satisfies it.
type Txer interface {
	Tx(ctx context.Context, fn func(*repository.Queries) error) error
}

// Config builds a Service. Everything is required except CacheSize.
type Config struct {
	Reader  Reader
	Tx      Txer
	Storage storage.Store
	Metrics *metrics.Metrics

	// CacheSize is the LRU's entry ceiling. Zero means defaultCacheSize.
	CacheSize int
}

// Service is the keyed blob service.
type Service struct {
	reader  Reader
	tx      Txer
	store   storage.Store
	metrics *metrics.Metrics

	lru *lruCache
	sf  singleflight.Group

	// THE RECORDER MEMO, and it is not the same thing as metrics.RecorderCache.
	//
	// RecorderCache keys on org + project + backend + kind CONCATENATED INTO A STRING,
	// which allocates 32 B on EVERY call -- measured, on the sstate HEAD hot path, and
	// twice per Exists. A backend_id already determines the org, the project and the
	// backend (it is a row in cache_backends), so the key here is a COMPARABLE STRUCT:
	// Go hashes it without allocating and the hot path is allocation-free.
	recMu sync.RWMutex
	recs  map[recKey]*metrics.Recorder

	// Pre-resolved bakery_db_queries_total counters, one per sqlc query we issue.
	// Resolved once: WithLabelValues costs ~95 ns on the hot path, which is most of
	// an LRU hit.
	qStat       counter
	qStatBatch  counter
	qLock       counter
	qGetWrite   counter
	qRevive     counter
	qPut        counter
	qPutOver    counter
	qDelete     counter
	qGetPhysDel counter
	qReap       counter

	sfInFlight gauge
}

// counter and gauge keep the prometheus types out of the struct's signature and make
// it obvious these are pre-resolved children, not vectors to be looked up per call.
type (
	counter interface{ Inc() }
	gauge   interface {
		Inc()
		Dec()
	}
)

// New validates cfg and builds the service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Reader == nil:
		return nil, errors.New("blob: Config.Reader is required")
	case cfg.Storage == nil:
		return nil, errors.New("blob: Config.Storage is required")
	case cfg.Metrics == nil:
		return nil, errors.New("blob: Config.Metrics is required")
	}

	size := cfg.CacheSize
	if size <= 0 {
		size = defaultCacheSize
	}

	m := cfg.Metrics

	return &Service{
		reader:      cfg.Reader,
		tx:          cfg.Tx,
		store:       cfg.Storage,
		metrics:     m,
		recs:        make(map[recKey]*metrics.Recorder),
		lru:         newLRU(m, size),
		qStat:       m.DBQueries.WithLabelValues("StatObject"),
		qStatBatch:  m.DBQueries.WithLabelValues("StatObjectsBatch"),
		qLock:       m.DBQueries.WithLabelValues("LockBlobDigest"),
		qGetWrite:   m.DBQueries.WithLabelValues("GetBlobForWrite"),
		qRevive:     m.DBQueries.WithLabelValues("CreateOrReviveBlob"),
		qPut:        m.DBQueries.WithLabelValues("PutObjectImmutable"),
		qPutOver:    m.DBQueries.WithLabelValues("PutObjectOverwritable"),
		qDelete:     m.DBQueries.WithLabelValues("DeleteObject"),
		qGetPhysDel: m.DBQueries.WithLabelValues("GetBlobForPhysicalDelete"),
		qReap:       m.DBQueries.WithLabelValues("ReapBlob"),
		sfInFlight:  m.SingleflightInFlight,
	}, nil
}

// recKey identifies a Recorder. A backend_id is a row in cache_backends, so it
// already determines the org, the project and the backend kind; only the metrics
// sub-kind (sstate's object vs siginfo) varies within one.
type recKey struct {
	backendID int64
	kind      string
}

// recorder returns the memoized Recorder for ref. The read path is an RLock and a
// map probe on a comparable key: no allocation, ~20 ns, in front of a
// BB_NUMBER_THREADS-parallel HEAD storm.
func (s *Service) recorder(ref Ref) *metrics.Recorder {
	k := recKey{backendID: ref.BackendID, kind: ref.Kind}

	s.recMu.RLock()
	r, ok := s.recs[k]
	s.recMu.RUnlock()

	if ok {
		return r
	}

	s.recMu.Lock()
	defer s.recMu.Unlock()

	if r, ok := s.recs[k]; ok {
		return r
	}

	r = s.metrics.Recorder(ref.Org, ref.Project, ref.Backend, ref.Kind)
	s.recs[k] = r

	return r
}

// --- the HEAD path ----------------------------------------------------------

// Exists answers the sstate HEAD storm.
//
// HEAD, not GET, is the hot path: BitBake fires a BB_NUMBER_THREADS-parallel HEAD
// storm over the whole setscene graph at the start of every build. An LRU hit here
// MUST issue zero Postgres queries -- TestExists_LRUHitIssuesZeroQueries is the gate,
// and BenchmarkExists_LRUHot reports db/op so a regression is visible.
func (s *Service) Exists(ctx context.Context, ref Ref) (bool, error) {
	rec := s.recorder(ref)

	meta, err := s.stat(ctx, ref, rec)
	if err != nil {
		rec.Observe(metrics.OpExists, metrics.ResultError)

		return false, err
	}

	rec.Observe(metrics.OpExists, result(meta.Exists))

	return meta.Exists, nil
}

// Stat is Exists plus the size and digest, for a HEAD that answers Content-Length.
// A miss is (Meta{Exists:false}, nil) -- NOT an error -- because a miss is the
// normal case and 404 is the answer, not 500.
func (s *Service) Stat(ctx context.Context, ref Ref) (Meta, error) {
	rec := s.recorder(ref)

	meta, err := s.stat(ctx, ref, rec)
	if err != nil {
		rec.Observe(metrics.OpHead, metrics.ResultError)

		return Meta{}, err
	}

	rec.Observe(metrics.OpHead, result(meta.Exists))

	return meta, nil
}

// stat is the uninstrumented lookup: LRU, then singleflight, then one primary-key
// probe. Every public method funnels through it so the cache, the collapse and the
// negative caching are implemented once.
func (s *Service) stat(ctx context.Context, ref Ref, rec *metrics.Recorder) (Meta, error) {
	var buf [512]byte

	ck := ref.appendCacheKey(buf[:0])

	if meta, ok := s.lru.get(ck); ok {
		return meta, nil
	}

	// SINGLEFLIGHT. 64 bitbake threads asking for the same setscene object at the
	// same instant must produce ONE query, not 64.
	//
	// The key is string(ck) -- an allocation, but only on the COLD path, which is
	// about to talk to Postgres anyway.
	key := string(ck)

	// THE PROBE IS DETACHED FROM THE CALLER THAT HAPPENS TO LEAD THE FLIGHT.
	//
	// A storm collapses N callers onto ONE probe. If that probe rides the leader's
	// request context, the leader disconnecting -- Ctrl-C, an ingress idle timeout, an
	// HTTP/2 RST_STREAM -- cancels the shared probe and singleflight hands
	// context.Canceled to every OTHER caller, whose contexts are perfectly alive. On the
	// sstate miss path that renders as a 500, and a 500 there breaks the build. The
	// leader is just whoever arrived first; it has no authority over anybody else's
	// request. context.WithoutCancel keeps the values (trace IDs, and so pgx's tracing)
	// while dropping the cancellation and the deadline.
	probeCtx := context.WithoutCancel(ctx)

	ch := s.sf.DoChan(key, func() (any, error) {
		s.sfInFlight.Inc()
		defer s.sfInFlight.Dec()

		return s.statDB(probeCtx, ref, []byte(key))
	})

	// And each caller still honours ITS OWN deadline against the shared flight: a
	// caller whose context dies waits for nobody. Only the leader's cancellation is
	// prevented from becoming everybody's.
	select {
	case <-ctx.Done():
		return Meta{}, fmt.Errorf("stat object: %w", ctx.Err())

	case res := <-ch:
		rec.Singleflight(res.Shared)

		if res.Err != nil {
			return Meta{}, res.Err
		}

		meta, ok := res.Val.(Meta)
		if !ok {
			return Meta{}, errors.New("blob: singleflight returned an unexpected type")
		}

		return meta, nil
	}
}

// statDB is the one query on the hot path: a single primary-key probe on
// cache_objects (backend_id, namespace, key). No join to blobs -- digest and size
// are denormalised and pinned by a composite FK -- and no lock.
func (s *Service) statDB(ctx context.Context, ref Ref, ck []byte) (Meta, error) {
	// THE ORDERING GUARD, and it must be read BEFORE the round-trip.
	//
	// This probe's answer describes the world as of NOW. By the time Postgres replies,
	// a concurrent Put or Delete may have published an AUTHORITATIVE entry for this
	// key. Landing our finding unconditionally would clobber it -- and because this
	// cache serves negative entries, a stale "absent" fill that lands on top of a
	// committed Put is a PERMANENT 404 for an object that exists, served from memory
	// with no further query until eviction. putIfUnchanged drops the fill if the
	// shard's write generation moved while we were in flight.
	seq := s.lru.seq(ck)

	s.qStat.Inc()

	row, err := s.reader.StatObject(ctx, repository.StatObjectParams{
		BackendID: ref.BackendID,
		Namespace: ref.Namespace,
		Key:       ref.Key,
	})

	if errors.Is(err, pgx.ErrNoRows) {
		// THE NEGATIVE ENTRY. Not an optimisation: without it the first build
		// against an empty cache sends every HEAD to Postgres, and no test that
		// pre-populates the repository will ever reveal it.
		meta := Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}}
		s.lru.putIfUnchanged(ck, meta, seq)

		return meta, nil
	}

	if err != nil {
		return Meta{}, fmt.Errorf("stat object: %w", err)
	}

	d, err := storage.KeyFromBytes(row.Digest)
	if err != nil {
		return Meta{}, fmt.Errorf("decode stored digest: %w", err)
	}

	meta := Meta{Exists: true, Digest: d, Size: row.SizeBytes, UpdatedAt: row.UpdatedAt.Time}
	s.lru.putIfUnchanged(ck, meta, seq)

	return meta, nil
}

// ExistsBatch answers REAPI FindMissingBlobs: "which of these do you have?", asked
// about N keys in ONE RPC. The result is POSITIONALLY ALIGNED with refs -- out[i] is
// the answer for refs[i] -- because Bazel and moon repeat a digest WITHIN a single
// request; the dedup happens INTERNALLY (on the query and the LRU fills), never in
// the returned slice.
//
// It is the batch sibling of Exists and holds the same two properties: an LRU hit
// issues zero queries, and every miss is negative-cached. The negative cache is not
// optional here -- a cold moon build has EVERY digest missing, so a positive-only
// fill would re-query all of them on every FindMissingBlobs.
//
// It deliberately does NOT use singleflight. Singleflight collapses N callers onto
// ONE key; this is ONE caller with N keys, so per-key flights would serialize the
// batch and buy nothing. Two concurrent batches with overlapping keys cost two
// queries, not 2N, and both land through putIfUnchanged, so neither can corrupt the
// other's fills.
func (s *Service) ExistsBatch(ctx context.Context, refs []Ref) ([]bool, error) {
	out := make([]bool, len(refs))
	if len(refs) == 0 {
		return out, nil
	}

	// One FindMissingBlobs = one instance_name = one backend_id + namespace, so every
	// ref shares a Recorder; recKey is a comparable struct, so this memo probe
	// allocates nothing even though we take it per RPC.
	rec := s.recorder(refs[0])

	// The residue is the set of LRU misses, grouped by (backend_id, namespace) -- in
	// practice exactly one group -- and deduped within a group by key. Each pending
	// remembers the out[] indices that asked for the key, and the shard write
	// generation captured BEFORE the round trip.
	type groupKey struct {
		backendID int64
		namespace string
	}

	type pending struct {
		ck      []byte // a COPY: the scan reuses one stack buffer for appendCacheKey
		seq     uint64
		indices []int
	}

	groups := map[groupKey]map[string]*pending{}

	var buf [512]byte

	for i, ref := range refs {
		ck := ref.appendCacheKey(buf[:0])

		if meta, ok := s.lru.get(ck); ok {
			out[i] = meta.Exists

			continue
		}

		gk := groupKey{backendID: ref.BackendID, namespace: ref.Namespace}

		byKey := groups[gk]
		if byKey == nil {
			byKey = map[string]*pending{}
			groups[gk] = byKey
		}

		p := byKey[ref.Key]
		if p == nil {
			// CAPTURE seq PER KEY, BEFORE the query. Each key hashes to its own shard and
			// each shard carries its own generation, so this is per key, never once for
			// the batch. putIfUnchanged then drops the fill if an authoritative put/del
			// bumped that shard while we were in flight -- so a stale batch read can never
			// clobber a concurrent Put's positive entry (a permanent 404 from memory).
			p = &pending{ck: append([]byte(nil), ck...), seq: s.lru.seq(ck)}
			byKey[ref.Key] = p
		}

		p.indices = append(p.indices, i)
	}

	for gk, byKey := range groups {
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}

		// ONCE PER QUERY, not per key -- that is what keeps bakery_db_queries_total
		// honest and makes the db/batch gate meaningful. In practice one group => one
		// increment for the whole FindMissingBlobs.
		s.qStatBatch.Inc()

		rows, err := s.reader.StatObjectsBatch(ctx, repository.StatObjectsBatchParams{
			BackendID: gk.backendID,
			Namespace: gk.namespace,
			Keys:      keys,
		})
		if err != nil {
			return nil, fmt.Errorf("stat objects batch: %w", err)
		}

		// The query returns ONLY the keys that exist. Fill those positive.
		present := make(map[string]struct{}, len(rows))

		for _, row := range rows {
			present[row.Key] = struct{}{}

			p := byKey[row.Key]
			if p == nil {
				continue // a key we did not ask about; ignore it defensively
			}

			d, err := storage.KeyFromBytes(row.Digest)
			if err != nil {
				return nil, fmt.Errorf("decode stored digest: %w", err)
			}

			meta := Meta{Exists: true, Digest: d, Size: row.SizeBytes, UpdatedAt: row.UpdatedAt.Time}
			s.lru.putIfUnchanged(p.ck, meta, p.seq)

			for _, i := range p.indices {
				out[i] = true
			}
		}

		// Every requested key ABSENT from the result is a MISS, and it MUST be
		// negative-cached -- out[i] is already false, but the LRU must learn it or the
		// next FindMissingBlobs re-queries every missing digest.
		for k, p := range byKey {
			if _, ok := present[k]; ok {
				continue
			}

			meta := Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}}
			s.lru.putIfUnchanged(p.ck, meta, p.seq)
		}
	}

	// Positionally observe hit/miss, mirroring Exists. out[] is fully resolved now, so
	// duplicated refs are each counted -- the RPC really did ask about each position.
	for i := range refs {
		rec.Observe(metrics.OpExists, result(out[i]))
	}

	return out, nil
}

// --- reads ------------------------------------------------------------------

// Get streams an object's bytes. The caller MUST Close the reader.
//
// A miss is ErrNotFound (404). Metadata that names absent bytes is
// ErrDanglingMetadata (500) and is NEVER served as a miss: a 404 there would let a
// build silently rebuild from a corrupted cache forever, while a 500 is loud.
func (s *Service) Get(ctx context.Context, ref Ref) (Meta, io.ReadCloser, error) {
	rec := s.recorder(ref)

	meta, err := s.stat(ctx, ref, rec)
	if err != nil {
		rec.Observe(metrics.OpGet, metrics.ResultError)

		return Meta{}, nil, err
	}

	if !meta.Exists {
		rec.Observe(metrics.OpGet, metrics.ResultMiss)

		return meta, nil, ErrNotFound
	}

	rc, err := s.store.Get(ctx, meta.Digest)
	if err != nil {
		rec.Observe(metrics.OpGet, metrics.ResultError)

		if errors.Is(err, storage.ErrNotFound) {
			return meta, nil, fmt.Errorf("%w: %s", ErrDanglingMetadata, meta.Digest)
		}

		return meta, nil, fmt.Errorf("read object bytes: %w", err)
	}

	rec.Observe(metrics.OpGet, metrics.ResultHit)
	rec.AddBytes(metrics.OpGet, meta.Size)

	return meta, rc, nil
}

// --- writes -----------------------------------------------------------------

// Put stores r under ref.
//
// THE ORDER IS THE POINT:
//
//  1. Stage the bytes and hash them as they stream. Nothing is durable, nothing is
//     named, and an sstate tarball is never buffered in memory.
//  2. Enforce the caller's verification policy against the digest we computed.
//  3. BEGIN. Take the DIGEST advisory lock -- keyed on the digest, not on a row,
//     because the row is exactly what may not exist yet, and FOR UPDATE cannot lock
//     a row that is not there. The GC takes the same lock before it unlinks.
//  4. SELECT ... FOR UPDATE. Absent, or 'pending_delete', means the bytes may
//     already be gone: we MUST commit ours. 'live' means the bytes are durably in
//     storage (that is what 'live' MEANS) and the write is elided -- dedup.
//  5. Commit the staged bytes, INSIDE the transaction, BEFORE any metadata row.
//     [BYTES FIRST.]
//  6. Create-or-revive the blob row, then insert the object row. The trigger does
//     the refcount arithmetic; Go never does. [THEN METADATA.]
//
// A crash anywhere leaves at worst orphaned bytes, which the GC reclaims. It cannot
// leave metadata naming bytes that are not there.
func (s *Service) Put(ctx context.Context, ref Ref, r io.Reader, opts PutOptions) (PutResult, error) {
	rec := s.recorder(ref)

	res, err := s.put(ctx, ref, r, opts)
	if err != nil {
		rec.Observe(metrics.OpPut, metrics.ResultError)

		return PutResult{}, err
	}

	// put/hit means "content already present, dedup elided the write"; put/miss means
	// "bytes newly written". Not obvious, and pinned in the metrics doc comment.
	rec.Observe(metrics.OpPut, result(res.Deduped))
	rec.AddBytes(metrics.OpPut, res.Size)

	return res, nil
}

// it into helpers would hide the ordering that is the entire point of the function.
//
//nolint:cyclop // The PUT protocol is a linear sequence of guarded steps; splitting
func (s *Service) put(ctx context.Context, ref Ref, r io.Reader, opts PutOptions) (PutResult, error) {
	if err := ref.validate(); err != nil {
		return PutResult{}, err
	}

	if opts.Verify.mode == verifyUnspecified {
		return PutResult{}, ErrVerificationUnspecified
	}

	if s.tx == nil {
		return PutResult{}, errors.New("blob: service is read-only (no Txer configured)")
	}

	w, err := s.store.Create(ctx)
	if err != nil {
		return PutResult{}, fmt.Errorf("stage object: %w", err)
	}

	// Correct on every path: a no-op after a successful Commit, and the only thing
	// that removes the staged bytes on a deduped write or an error.
	defer func() { _ = w.Abort() }()

	if _, err := io.Copy(w, r); err != nil {
		return PutResult{}, fmt.Errorf("stage object bytes: %w", err)
	}

	digest, size := w.Digest()

	if opts.Verify.mode == verifyContent && opts.Verify.want != digest {
		return PutResult{}, fmt.Errorf("%w: want %s, got %s", ErrDigestMismatch, opts.Verify.want, digest)
	}

	// THE FSYNC HAPPENS HERE -- OUTSIDE THE TRANSACTION, BEFORE THE DIGEST LOCK.
	//
	// Making the bytes durable is the expensive half of Commit, and on an sstate
	// tarball it is seconds of I/O. Inside the transaction it would hold a pool
	// connection (there are 16) AND the digest advisory lock across that fsync, so a
	// handful of concurrent large PUTs starve every other Postgres user: /readyz's
	// ping, the next build's HEAD storm, every /api/v1 call. Commit then only renames
	// and fsyncs the directory, which is microseconds.
	//
	// Bytes-first is preserved and in fact strengthened: the data is durable HERE,
	// strictly before any metadata row exists to name it.
	if err := w.Sync(); err != nil {
		return PutResult{}, fmt.Errorf("sync staged object: %w", err)
	}

	var (
		deduped bool
		created bool
	)

	err = s.tx.Tx(ctx, func(q *repository.Queries) error {
		s.qLock.Inc()

		if err := q.LockBlobDigest(ctx, digest.Bytes()); err != nil {
			return fmt.Errorf("lock blob digest: %w", err)
		}

		s.qGetWrite.Inc()

		row, err := q.GetBlobForWrite(ctx, digest.Bytes())

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No row: the bytes are not ours to assume. Upload.
		case err != nil:
			return fmt.Errorf("lock blob row: %w", err)
		case row.SizeBytes != size:
			return fmt.Errorf("%w: %s is %d bytes, we hashed %d", ErrSizeMismatch, digest, row.SizeBytes, size)
		case row.State == repository.BlobStateLive:
			// 'live' MEANS the bytes are durably in storage. Dedup: elide the write.
			deduped = true
		default:
			// 'pending_delete': the GC may have already unlinked the bytes. The
			// tombstone outlives the bytes precisely so that we can see this and
			// refuse to trust them. Re-upload.
		}

		if !deduped {
			if _, err := w.Commit(ctx); err != nil { // [BYTES FIRST]
				return fmt.Errorf("commit object bytes: %w", err)
			}
		}

		s.qRevive.Inc()

		n, err := q.CreateOrReviveBlob(ctx, repository.CreateOrReviveBlobParams{
			Digest: digest.Bytes(), SizeBytes: size,
		})
		if err != nil {
			return fmt.Errorf("create or revive blob: %w", err)
		}

		if n == 0 {
			// The DO UPDATE's WHERE guards on size. Zero rows means a row exists for
			// this digest with a different size: sha256 broke, or we lied.
			return fmt.Errorf("%w: %s", ErrSizeMismatch, digest)
		}

		created, err = s.putObject(ctx, q, ref, digest, size, opts.Overwrite)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return PutResult{}, err
	}

	s.cache(ref, digest, size, created)

	return PutResult{Digest: digest, Size: size, Deduped: deduped, Created: created}, nil
}

// putObject writes the metadata row. [THEN METADATA.] The trigger increments the
// refcount; there is no increment query, and there must never be one.
func (s *Service) putObject(
	ctx context.Context, q *repository.Queries, ref Ref, digest Digest, size int64, overwrite bool,
) (bool, error) {
	if overwrite {
		s.qPutOver.Inc()

		n, err := q.PutObjectOverwritable(ctx, repository.PutObjectOverwritableParams{
			BackendID: ref.BackendID, Namespace: ref.Namespace, Key: ref.Key,
			Digest: digest.Bytes(), SizeBytes: size,
		})
		if err != nil {
			return false, fmt.Errorf("put object (overwritable): %w", err)
		}

		return n == 1, nil
	}

	s.qPut.Inc()

	n, err := q.PutObjectImmutable(ctx, repository.PutObjectImmutableParams{
		BackendID: ref.BackendID, Namespace: ref.Namespace, Key: ref.Key,
		Digest: digest.Bytes(), SizeBytes: size,
	})
	if err != nil {
		return false, fmt.Errorf("put object (immutable): %w", err)
	}

	// n == 0 is DO NOTHING: an immutable key that already exists. Someone else's row
	// stands, and it may name different content than ours.
	return n == 1, nil
}

// cache refreshes the LRU after a write. If we did NOT create the row (an immutable
// key someone else won), the row in the database is not the one we just described,
// so the entry is DROPPED rather than asserted -- a wrong positive entry serves the
// wrong digest to every subsequent GET.
func (s *Service) cache(ref Ref, digest Digest, size int64, created bool) {
	var buf [512]byte

	ck := ref.appendCacheKey(buf[:0])

	if !created {
		s.lru.del(ck)

		return
	}

	s.lru.put(ck, Meta{Exists: true, Digest: digest, Size: size, UpdatedAt: time.Now()})
}

// Delete removes an object's METADATA and nothing else. [METADATA FIRST.]
//
// The bytes are deliberately untouched: another project may reference the same blob
// (that is what dedup means), and cache_objects_blob_fk is ON DELETE RESTRICT so the
// database itself will refuse to let the blob row vanish while any object names it.
// Only ReapDigest deletes bytes, and only after Postgres has agreed the refcount is
// zero.
func (s *Service) Delete(ctx context.Context, ref Ref) (bool, error) {
	if s.tx == nil {
		return false, errors.New("blob: service is read-only (no Txer configured)")
	}

	var n int64

	err := s.tx.Tx(ctx, func(q *repository.Queries) error {
		s.qDelete.Inc()

		var err error

		n, err = q.DeleteObject(ctx, repository.DeleteObjectParams{
			BackendID: ref.BackendID, Namespace: ref.Namespace, Key: ref.Key,
		})
		if err != nil {
			return fmt.Errorf("delete object: %w", err)
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	// A NEGATIVE entry, not an eviction: the next HEAD for this key must be answered
	// from memory, not from Postgres. Deletes are rare; the HEAD after them is not.
	var buf [512]byte

	ck := ref.appendCacheKey(buf[:0])
	s.lru.put(ck, Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}})

	return n > 0, nil
}

// ReapDigest is the PHYSICAL delete: the only path in the system that removes bytes.
//
// It is a PRIMITIVE, not a loop. The GC (M6) supplies the policy -- which digests,
// the grace period, the write barrier, the marking -- and drives this per digest.
//
// One transaction, and storage.Delete runs INSIDE it, between the recheck and the
// reap, while the digest advisory lock is held:
//
//  1. pg_advisory_xact_lock(digest) -- the same lock every PUT takes.
//  2. SELECT ... WHERE state = 'pending_delete' AND refcount = 0 FOR UPDATE.
//     ZERO ROWS MEANS THE BLOB WAS REVIVED by a PUT while we queued for the lock:
//     roll back, and DO NOT UNLINK. This is the refcount = 0 recheck, and it is what
//     the grace period alone cannot give you.
//  3. storage.Delete.
//  4. DELETE FROM blobs (also guarded on pending_delete AND refcount = 0; the
//     ON DELETE RESTRICT FK makes refcount drift a foreign-key violation rather than
//     a silent unlink of bytes an object still names).
//
// Committing before the unlink would satisfy the crash invariant and REOPEN the
// resurrection race. A rollback after the unlink leaves the durable 'pending_delete'
// tombstone, which is exactly what a later PUT must see so it re-uploads instead of
// deduping onto bytes that are already gone.
//
// Returns false when the blob was revived or was already reaped -- both are normal
// and neither is an error.
func (s *Service) ReapDigest(ctx context.Context, digest Digest) (bool, error) {
	if s.tx == nil {
		return false, errors.New("blob: service is read-only (no Txer configured)")
	}

	reaped := false

	err := s.tx.Tx(ctx, func(q *repository.Queries) error {
		s.qLock.Inc()

		if err := q.LockBlobDigest(ctx, digest.Bytes()); err != nil {
			return fmt.Errorf("lock blob digest: %w", err)
		}

		s.qGetPhysDel.Inc()

		if _, err := q.GetBlobForPhysicalDelete(ctx, digest.Bytes()); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errRevived
			}

			return fmt.Errorf("lock blob for physical delete: %w", err)
		}

		if err := s.store.Delete(ctx, digest); err != nil {
			return fmt.Errorf("delete object bytes: %w", err)
		}

		s.qReap.Inc()

		n, err := q.ReapBlob(ctx, digest.Bytes())
		if err != nil {
			return fmt.Errorf("reap blob: %w", err)
		}

		reaped = n > 0

		return nil
	})

	if errors.Is(err, errRevived) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return reaped, nil
}

// result maps a boolean outcome onto the headline series' result label. For a read,
// hit means served and miss means absent. For a PUT, hit means dedup elided the
// write.
func result(ok bool) metrics.Result {
	if ok {
		return metrics.ResultHit
	}

	return metrics.ResultMiss
}
