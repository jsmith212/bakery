package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// ErrMixedDeleteBatch means a batch named more than one (backend, namespace).
// DeleteObjectsByKeys is scoped to one of each -- every GC stage already works one
// namespace at a time -- so a mixed batch is a caller bug, not a query to widen.
var ErrMixedDeleteBatch = errors.New("blob: DeleteBatch requires one backend and one namespace per call")

// deleteBatchAttempts bounds the 40P01 retry. Two batches that race each other on the
// same blob rows can still deadlock even with both deleting in digest order (a
// concurrent /ac overwrite takes its two locks on its own schedule), and the loser is
// aborted by Postgres, not corrupted -- so a retry is exactly right and an unbounded
// one is not.
const deleteBatchAttempts = 3

// deleteBatchBackoff is the first retry's pause; it doubles per attempt. Small on
// purpose: the transaction it retries holds no locks by the time we see the error.
const deleteBatchBackoff = 10 * time.Millisecond

// DeleteRef names one doomed object: the Ref plus the digest the GC's scan read for
// it. The digest travels because the batch DELETE is DIGEST-ORDERED, and it is the
// scan's digest rather than one re-read here on purpose -- re-reading would be a
// second round trip per chunk to recover a value the scan already returned.
type DeleteRef struct {
	Ref
	Digest Digest
}

// DeleteBatch is THE ONLY DELETION PATH THE GC USES (spec 9.2), and it is what
// DeleteObjectsChunk is not: it invalidates the LRU.
//
// One transaction per chunk, through DeleteObjectsByKeys, which sorts the driving set
// by digest before the DELETE touches a row. That ordering is the whole reason this
// method exists rather than a loop over Delete: the refcount trigger takes paired blob
// locks in ascending digest order to kill an ABBA against a concurrent /ac overwrite,
// and a key-ordered batch delete would reintroduce that same deadlock a thousand
// digests wide.
//
// The LRU invalidation runs AFTER the commit, never before: delBatch's per-shard
// generation bump is what invalidates statDB fills that raced this delete, and a bump
// taken before the rows are gone would let a fill started after it read the still-live
// row and land a stale POSITIVE entry -- a cached digest for an object that no longer
// exists, which the byte reap then turns into ErrDanglingMetadata.
//
// runID IS THE WRITE BARRIER and it is a REQUIRED argument, not an option:
// DeleteObjectsByKeys re-derives `created_at < run.started_at AND
// pg_visible_in_snapshot(live_xid, run.snapshot)` FROM THE gc_runs ROW, in SQL, at
// delete time (CLAUDE.md: never select `snapshot` back into Go). The doomed set was
// computed by an EARLIER ScanObjectsForGC pass, and in the gap a build can overwrite
// or re-create any key in it; without the barrier this method would delete a row a
// build just resurrected. A caller with no run has no business deleting in bulk --
// which is why there is no runID-less overload and why the GC is the only caller.
func (s *Service) DeleteBatch(ctx context.Context, runID int64, refs []DeleteRef) (int64, error) {
	if len(refs) == 0 {
		return 0, nil
	}

	if s.tx == nil {
		return 0, errors.New("blob: service is read-only (no Txer configured)")
	}

	backendID := refs[0].BackendID
	namespace := refs[0].Namespace

	keys := make([]string, 0, len(refs))
	digests := make([][]byte, 0, len(refs))
	cks := make([]string, 0, len(refs))

	var buf [512]byte

	for _, r := range refs {
		if r.BackendID != backendID || r.Namespace != namespace {
			return 0, ErrMixedDeleteBatch
		}

		if err := r.validate(); err != nil {
			return 0, err
		}

		keys = append(keys, r.Key)
		digests = append(digests, r.Digest.Bytes())
		cks = append(cks, string(r.appendCacheKey(buf[:0])))
	}

	n, err := s.deleteBatchTx(ctx, runID, backendID, namespace, keys, digests)
	if err != nil {
		return 0, err
	}

	s.lru.delBatch(cks)

	return n, nil
}

// InvalidateKeys drops the LRU entries for rows some OTHER statement deleted.
//
// It exists for exactly one caller: the GC's stage 8 (SweepUnreferencedManifests),
// whose tag anti-join has to be evaluated in SQL at delete time and which therefore
// REPORTS the keys it deleted instead of being handed them. Everything else deletes
// through DeleteBatch or Delete, both of which invalidate for themselves.
//
// It is deliberately NOT a general "delete these rows" API and takes no digests: it
// touches no database and no bytes, it only forgets. Calling it for rows that still
// exist is safe (the next read re-fills from Postgres) and costs a cache miss;
// NOT calling it after a raw SQL delete is not safe at all -- the LRU serves
// POSITIVE answers with zero database contact, so a stale entry answers "present"
// for a row that is gone until the process restarts, and the byte reap then turns
// that answer into a permanent dangling-metadata 500.
//
// It uses the same shard-grouped, one-bump-per-shard delBatch the GC's own batch
// delete uses, for the same reason (finding 16): a per-key generation bump would
// suppress concurrent cold-HEAD fills process-wide for the whole chunk.
func (s *Service) InvalidateKeys(backendID int64, namespace string, keys []string) {
	if len(keys) == 0 {
		return
	}

	var buf [512]byte

	cks := make([]string, 0, len(keys))

	for _, k := range keys {
		ref := Ref{
			BackendID: backendID,
			Org:       "", Project: "", Backend: "", Kind: "",
			Namespace: namespace, Key: k,
		}

		cks = append(cks, string(ref.appendCacheKey(buf[:0])))
	}

	s.lru.delBatch(cks)
}

// deleteBatchTx runs the pre-lock and the batch DELETE in ONE transaction, with the
// bounded 40P01 retry.
//
// THE PRE-LOCK IS THE ABBA GUARANTEE (spec 6.2/9.2, R7#11). DeleteObjectsByKeys sorts
// its driving set `ORDER BY d.digest` inside a USING subquery, and the refcount
// trigger sorts its paired blob locks the same way -- but an ORDER BY in a subquery is
// a hint about ROW order, not a promise about the order the executor acquires the blobs
// row locks that the DELETE's cascading trigger takes. LockBlobDigests takes them
// explicitly, ordered, in the same transaction and BEFORE the DELETE, so the lock order
// is deterministic by construction instead of by planner accident. It has to be the
// SAME transaction: a lock taken in another one is released before the DELETE begins
// and protects nothing (the same reason Put's advisory lock lives inside its tx).
//
// The digests are sorted in Go as well. The query's own ORDER BY is what Postgres
// obeys, so this is belt -- but it costs one sort per chunk, makes the parameter array
// deterministic, and means a future edit that loses the SQL ORDER BY degrades to a
// still-ordered lock acquisition rather than to a deadlock under load.
func (s *Service) deleteBatchTx(
	ctx context.Context, runID, backendID int64, namespace string, keys []string, digests [][]byte,
) (int64, error) {
	locks := make([][]byte, len(digests))
	copy(locks, digests)
	slices.SortFunc(locks, bytes.Compare)

	var lastErr error

	for attempt := range deleteBatchAttempts {
		var n int64

		err := s.tx.Tx(ctx, func(q *repository.Queries) error {
			s.qLockDigests.Inc()

			if err := q.LockBlobDigests(ctx, locks); err != nil {
				return fmt.Errorf("lock blob digests: %w", err)
			}

			s.qDeleteByKeys.Inc()

			var err error

			n, err = q.DeleteObjectsByKeys(ctx, repository.DeleteObjectsByKeysParams{
				Keys: keys, Digests: digests, BackendID: backendID, Namespace: namespace, RunID: runID,
			})
			if err != nil {
				return fmt.Errorf("delete objects by keys: %w", err)
			}

			return nil
		})
		if err == nil {
			return n, nil
		}

		lastErr = err

		if !isDeadlock(err) {
			return 0, err
		}

		// The whole transaction was aborted by Postgres, so there is nothing to unwind;
		// pause briefly so the winner can finish rather than colliding again immediately.
		if err := sleepCtx(ctx, deleteBatchBackoff<<attempt); err != nil {
			return 0, err
		}
	}

	return 0, fmt.Errorf("delete objects by keys: %d attempts deadlocked: %w", deleteBatchAttempts, lastErr)
}

// isDeadlock reports whether err is Postgres 40P01. errors.As, not a string match: the
// error arrives wrapped by the transaction helper and by this package.
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.DeadlockDetected
}

// sleepCtx is a cancellable pause. A plain time.Sleep here would make a shutdown wait
// out the whole backoff.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("delete batch: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
