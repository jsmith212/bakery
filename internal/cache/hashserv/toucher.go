package hashserv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// THE UNIHASH TOUCHER (M6 spec 6.1's hashserv paragraph).
//
// hashserv_unihashes is the GC root (CLAUDE.md; spec 3 stage 1): an sstate object is
// reachable only FROM a surviving unihash row, so keeping this table's accessed_at
// fresh is what keeps a live build's sstate objects alive too.
//
// It does NOT reuse blob.Service's LRU-stamped design. There is no cache in front of
// a hashserv read to piggyback a stamp on -- every hit here already pays a full
// Postgres round trip (store.getUnihash / getUnihashFull / getOuthash), so a SINGLE
// MUTEX guarding a plain map is the right shape, not a 64-shard structure: hashserv
// QPS is nowhere near BitBake's BB_NUMBER_THREADS HEAD storm against sstate, and one
// more mutex acquisition per hit is noise next to the query it just paid for.
//
// The shape is auth.Service's keyToucher (mark -> drain -> batched UPDATE on a
// ticker), but NOT its shutdown behavior: StartKeyToucher's ctx.Done() case returns
// immediately with no final flush, and blob/toucher.go already flags that as a
// recorded bug rather than a pattern to copy. This toucher flushes once more on the
// way out, exactly as blob.Service.StartAccessToucher does, under
// context.WithoutCancel with its own bounded timeout.

const (
	// defaultUnihashTouchStaleness is T when the caller supplies no ramp. Matches
	// the SQL guard in TouchUnihashesAccessed.
	defaultUnihashTouchStaleness = time.Hour

	// unihashTouchBatchSize caps the (method, taskhash) pairs in one UPDATE, mirroring
	// blob's touchBatchSize.
	unihashTouchBatchSize = 1000

	// unihashFinalFlushTimeout bounds the shutdown flush, exactly like blob's
	// finalFlushTimeout: WithoutCancel has no deadline of its own, so a wedged pool
	// would otherwise hold shutdown open indefinitely.
	unihashFinalFlushTimeout = 5 * time.Second
)

// unihashKey is hashserv_unihashes' real primary key: (backend_id, method, taskhash).
type unihashKey struct {
	backendID int64
	method    string
	taskhash  string
}

type unihashMark struct {
	key      unihashKey
	markedAt int64 // unix nanos
}

// unihashToucher is the pending set. FIRST MARK WINS, exactly like
// blob.lruEntry.markedAt and blob.auxPending: the stamp is "when the oldest
// unflushed read happened", so repeated hits inside T collapse into one flush.
type unihashToucher struct {
	q Queries

	mu    sync.Mutex
	marks map[unihashKey]int64

	// nowNano is overridden by tests; production uses the wall clock.
	nowNano func() int64
}

func newUnihashToucher(q Queries) *unihashToucher {
	return &unihashToucher{
		q:       q,
		marks:   map[unihashKey]int64{},
		nowNano: func() int64 { return time.Now().UnixNano() },
	}
}

// mark records an unflushed HIT of (backendID, method, taskhash). Called ONLY from a
// get / get-stream / get-outhash response that actually read an existing
// hashserv_unihashes row -- never on a miss, and never on an upstream write-through
// (that path INSERTs a brand-new row; created_at already says everything a touch
// would, and there is nothing pre-existing to have been "accessed").
//
// Nil-safe: a Backend built by hand in a test (New() not called) carries a nil
// toucher, and marking into it must be a no-op rather than a panic.
func (t *unihashToucher) mark(backendID int64, method, taskhash string) {
	if t == nil {
		return
	}

	k := unihashKey{backendID: backendID, method: method, taskhash: taskhash}

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.marks[k]; ok {
		return
	}

	t.marks[k] = t.nowNano()
}

// pending answers spec 6.2's veto for its hashserv-map half: "does this process hold
// an unflushed read of this (backendID, method, taskhash)?" It is not yet wired into
// a sweep -- internal/gc's stage 1 (SweepUnihashes) is a single write-barrier-filtered
// DELETE with no per-row Go-side decision point to intersect against, unlike the
// scan-then-DeleteBatch shape the cache_objects stages use -- but the answer has to
// exist and be correct on the day a caller needs it, so it is built alongside mark
// rather than bolted on later.
func (t *unihashToucher) pending(backendID int64, method, taskhash string) bool {
	if t == nil {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	_, ok := t.marks[unihashKey{backendID: backendID, method: method, taskhash: taskhash}]

	return ok
}

// collect reports marks at or older than cutoff WITHOUT removing them -- the clear is
// a separate pass, exactly as blob.auxPending.collect/clear split, so a mark stays
// visible to pending() for the entire time its UPDATE is in flight.
func (t *unihashToucher) collect(cutoff int64) []unihashMark {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []unihashMark

	for k, at := range t.marks {
		if at <= cutoff {
			out = append(out, unihashMark{key: k, markedAt: at})
		}
	}

	return out
}

// clear removes exactly the marks that were flushed, and only if unchanged -- a mark
// re-taken for the same key while its flush was in flight must survive to the next
// tick rather than being dropped underneath the read that produced it.
func (t *unihashToucher) clear(marks []unihashMark) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, m := range marks {
		if at, ok := t.marks[m.key]; ok && at == m.markedAt {
			delete(t.marks, m.key)
		}
	}
}

// flush is one tick. all=true takes every mark regardless of age (the shutdown
// flush); otherwise only marks older than staleness, which is what makes N hits
// within T cost one UPDATE per backend rather than one per tick.
func (t *unihashToucher) flush(ctx context.Context, staleness time.Duration, all bool) (int64, error) {
	cutoff := int64(math.MaxInt64)
	if !all {
		cutoff = t.nowNano() - staleness.Nanoseconds()
	}

	marks := t.collect(cutoff)
	if len(marks) == 0 {
		return 0, nil
	}

	// Grouped by backendID only: TouchUnihashesAccessedParams takes ONE backend_id
	// and parallel method/taskhash arrays, so pairs from different projects cannot
	// share a statement (mirrors blob's per-(backend,namespace) grouping).
	byBackend := map[int64][]unihashMark{}
	for _, m := range marks {
		byBackend[m.key.backendID] = append(byBackend[m.key.backendID], m)
	}

	var rows int64

	for backendID, ms := range byBackend {
		for i := 0; i < len(ms); i += unihashTouchBatchSize {
			chunk := ms[i:min(i+unihashTouchBatchSize, len(ms))]

			methods := make([]string, len(chunk))
			taskhashes := make([]string, len(chunk))

			for j, m := range chunk {
				methods[j] = m.key.method
				taskhashes[j] = m.key.taskhash
			}

			n, err := t.q.TouchUnihashesAccessed(ctx, repository.TouchUnihashesAccessedParams{
				BackendID: backendID,
				Staleness: pgtype.Interval{
					Microseconds: staleness.Microseconds(), Days: 0, Months: 0, Valid: true,
				},
				Methods:    methods,
				Taskhashes: taskhashes,
			})
			if err != nil {
				return rows, fmt.Errorf("hashserv: touch unihashes accessed: %w", err)
			}

			rows += n
		}
	}

	t.clear(marks)

	return rows, nil
}

// StartUnihashToucher runs the coalescing accessed_at flusher until ctx is cancelled,
// then flushes one last time. Wire it beside blob.Service.StartAccessToucher and the
// GC loop in the server's boot goroutines.
//
// staleness is a FUNCTION, not a duration, for the same reason blob's is: T ramps
// (spec 6.4) for the first week after the 000012 migration before tightening from
// 24h to 1h, and the ramp policy belongs to the caller that knows when the migration
// ran.
func (b *Backend) StartUnihashToucher(ctx context.Context, interval time.Duration, staleness func() time.Duration) {
	if b.touch == nil {
		return // New() was not used to build this Backend -- nothing to flush.
	}

	if staleness == nil {
		staleness = func() time.Duration { return defaultUnihashTouchStaleness }
	}

	log := b.deps.Logger

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unihashFinalFlushTimeout)
			defer cancel()

			if _, err := b.touch.flush(flushCtx, staleness(), true); err != nil {
				log.ErrorContext(flushCtx, "hashserv: final accessed_at flush failed", slog.Any("error", err))
			}

			return

		case <-ticker.C:
			if _, err := b.touch.flush(ctx, staleness(), false); err != nil && !errors.Is(err, context.Canceled) {
				log.ErrorContext(ctx, "hashserv: accessed_at flush failed", slog.Any("error", err))
			}
		}
	}
}
