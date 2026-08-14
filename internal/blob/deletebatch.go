package blob

import (
	"context"
	"errors"
	"fmt"
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
func (s *Service) DeleteBatch(ctx context.Context, refs []DeleteRef) (int64, error) {
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

	n, err := s.deleteBatchTx(ctx, backendID, namespace, keys, digests)
	if err != nil {
		return 0, err
	}

	s.lru.delBatch(cks)

	return n, nil
}

// deleteBatchTx runs the batch DELETE with the bounded 40P01 retry.
func (s *Service) deleteBatchTx(
	ctx context.Context, backendID int64, namespace string, keys []string, digests [][]byte,
) (int64, error) {
	var lastErr error

	for attempt := range deleteBatchAttempts {
		var n int64

		err := s.tx.Tx(ctx, func(q *repository.Queries) error {
			s.qDeleteByKeys.Inc()

			var err error

			n, err = q.DeleteObjectsByKeys(ctx, repository.DeleteObjectsByKeysParams{
				Keys: keys, Digests: digests, BackendID: backendID, Namespace: namespace,
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
