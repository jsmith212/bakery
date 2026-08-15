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
	"github.com/jsmith212/bakery/internal/metrics"
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

	// unihashMarksCap bounds the ENTIRE pending set (R8#2), mirroring auxPending's
	// auxShardCap (blob/toucher.go) -- but there is only one shard here, not
	// lruShards of them, because a single mutex is already the right shape for
	// hashserv's QPS (see the package doc above). Unlike blob's LRU-backed mark,
	// nothing upstream of mark() bounds the key space: every hit marks
	// unconditionally, so without a cap the map grows without limit AND every
	// flush's iteration over it grows with it. A drop costs a touch, never a wrong
	// delete -- the row just keeps whatever accessed_at/created_at it already had,
	// exactly the tradeoff auxPending's comment spells out -- so refusing past the
	// cap is safe, and it is what keeps flush's cost bounded no matter how long the
	// process has been marking without a tick landing.
	unihashMarksCap = 1 << 16
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
	q  Queries
	gc *metrics.GCRecorder

	mu sync.Mutex

	// marks is the live pending set: what mark() adds to and what pending() and the
	// next flush() see. R8#2: flush() SWAPS this out (old := t.marks; t.marks =
	// fresh) rather than iterating it in place, so a flush of an arbitrarily large
	// pending set never holds this mutex for longer than a pointer assignment --
	// which is the mutex mark() needs on the get-stream hot path.
	marks map[unihashKey]int64

	// flushing holds exactly the batch a flush() call has handed to
	// TouchUnihashesAccessed and not yet resolved. It exists because, for the round
	// trip's duration, that batch is in neither marks (swapped out) nor the
	// database (not committed yet) -- and the §6.2 veto must not have a gap between
	// the two. pending() checks both maps; a failed flush merges this back into
	// marks (mergeBack) before clearing it.
	flushing map[unihashKey]int64

	// dropped counts marks refused because unihashMarksCap was full. Not (yet)
	// exported as its own Prometheus series -- unlike blob's aux map, this is a
	// brand-new cap with no operator-facing story yet -- but it must be countable
	// for tests to prove the drop path is safe rather than silently wrong.
	dropped int64

	// nowNano is overridden by tests; production uses the wall clock.
	nowNano func() int64
}

func newUnihashToucher(q Queries, gc *metrics.GCRecorder) *unihashToucher {
	return &unihashToucher{
		q:       q,
		gc:      gc,
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

	if len(t.marks) >= unihashMarksCap {
		t.dropped++

		return
	}

	t.marks[k] = t.nowNano()
}

// droppedMarks reports how many marks unihashMarksCap has refused. Unexported: it
// exists for tests to prove the cap actually engages rather than silently growing
// forever, not as an operator-facing surface (see the field comment).
func (t *unihashToucher) droppedMarks() int64 {
	if t == nil {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	return t.dropped
}

// pending answers spec 6.2's veto for its hashserv-map half: "does this process hold
// an unflushed read of this (backendID, method, taskhash)?" It is not yet wired into
// a sweep -- internal/gc's stage 1 (SweepUnihashes) is a single write-barrier-filtered
// DELETE with no per-row Go-side decision point to intersect against, unlike the
// scan-then-DeleteBatch shape the cache_objects stages use -- but the answer has to
// exist and be correct on the day a caller needs it, so it is built alongside mark
// rather than bolted on later.
//
// It checks BOTH marks and flushing: a key mid-flight in an in-flight UPDATE has
// already left marks (flush() swapped it out) but has not committed yet, and the veto
// must not have a gap for that whole round trip.
func (t *unihashToucher) pending(backendID int64, method, taskhash string) bool {
	if t == nil {
		return false
	}

	k := unihashKey{backendID: backendID, method: method, taskhash: taskhash}

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.marks[k]; ok {
		return true
	}

	_, ok := t.flushing[k]

	return ok
}

// partitionMarks splits a swapped-out mark set into what is due to flush (at <=
// cutoff) and what is not. Called with NO LOCK HELD -- that is the entire point of
// swapping the map out first (see flush).
//
// notDue is ALWAYS non-nil, even when empty: flush() assigns it straight to t.marks,
// and every other method here (mark, pending, mergeBack) writes into t.marks
// unconditionally, exactly as newUnihashToucher's initial map does. A lazily-created
// nil map here would panic the very next mark() -- or, on an all=true flush with
// nothing left over, the very next mergeBack.
func partitionMarks(marks map[unihashKey]int64, cutoff int64) (due []unihashMark, notDue map[unihashKey]int64) {
	notDue = map[unihashKey]int64{}

	for k, at := range marks {
		if at <= cutoff {
			due = append(due, unihashMark{key: k, markedAt: at})

			continue
		}

		notDue[k] = at
	}

	return due, notDue
}

// mergeBack restores marks a flush did not durably clear, FIRST MARK WINS: a key
// re-marked (by a concurrent hit) while its old mark was mid-flight must keep the
// OLDER of the two timestamps, because that older mark is the read the staleness
// clock is actually supposed to be measuring from.
func (t *unihashToucher) mergeBack(marks []unihashMark) {
	if len(marks) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, m := range marks {
		if cur, ok := t.marks[m.key]; !ok || m.markedAt < cur {
			t.marks[m.key] = m.markedAt
		}
	}
}

// flush is one tick. all=true takes every mark regardless of age (the shutdown flush
// and Backend.FlushNow's forced flush); otherwise only marks older than staleness,
// which is what makes N hits within T cost one UPDATE per backend rather than one per
// tick.
//
// R8#2: the whole pending set is swapped out from under t.mu in ONE O(1) pointer
// assignment (old := t.marks; t.marks = fresh), and everything after that -- the
// cutoff partition and the database round trip -- runs with NO LOCK HELD, so mark()
// on the get-stream hot path never waits behind a flush no matter how large the
// pending set has grown. The not-yet-due half is folded back in a second short lock
// that costs only what mark() added DURING the unlocked partition (almost always a
// small number of hits, not the whole pending set): that is the difference between
// this shape and the one it replaces, which iterated -- and filtered -- the entire
// map while holding the same mutex mark() needs.
func (t *unihashToucher) flush(ctx context.Context, staleness time.Duration, all bool) (int64, error) {
	cutoff := int64(math.MaxInt64)
	if !all {
		cutoff = t.nowNano() - staleness.Nanoseconds()
	}

	t.mu.Lock()
	old := t.marks
	t.marks = map[unihashKey]int64{}
	// flushing = old AT THE SWAP, not after the partition: between here and the
	// due-only narrowing below, pending() must keep answering true for EVERY
	// swapped key -- the not-due ones really are pending (they get folded back),
	// and the due ones are about to be written. A superset over-approximates the
	// veto in the safe direction; the gap that existed before this line was the
	// one window in which a swapped-but-unpartitioned key was in neither map.
	t.flushing = old
	t.mu.Unlock()

	due, notDue := partitionMarks(old, cutoff)

	// Fold back whatever is not due yet. interim is whatever mark() added to the
	// fresh map while the partition above ran lock-free; iterating IT (not notDue)
	// under the lock is what keeps this critical section proportional to concurrent
	// hits during the partition, not to the size of the pending set.
	t.mu.Lock()
	interim := t.marks
	for k, at := range interim {
		if cur, ok := notDue[k]; !ok || at < cur {
			notDue[k] = at
		}
	}
	t.marks = notDue
	t.mu.Unlock()

	if len(due) == 0 {
		t.mu.Lock()
		t.flushing = nil
		t.mu.Unlock()

		return 0, nil
	}

	// due has left marks and has not landed in the database: flushing is what keeps
	// pending() answering true for it for the whole round trip below.
	flushing := make(map[unihashKey]int64, len(due))
	for _, m := range due {
		flushing[m.key] = m.markedAt
	}

	t.mu.Lock()
	t.flushing = flushing
	t.mu.Unlock()

	rows, err := t.writeTouches(ctx, due, staleness)

	t.mu.Lock()
	t.flushing = nil
	t.mu.Unlock()

	if t.gc != nil {
		t.gc.TouchFlushRows(rows)
	}

	if err != nil {
		// All-or-nothing, same as the shape this replaces: a failed flush leaves
		// EVERY mark in this batch pending, including ones from chunks that already
		// wrote successfully earlier in this same call (rows counts those; the
		// retry against them is a harmless no-op under TouchUnihashesAccessed's own
		// staleness guard).
		t.mergeBack(due)

		return rows, err
	}

	return rows, nil
}

// writeTouches issues one TouchUnihashesAccessed per (backend, <=1000-key chunk).
// Grouped by backend because TouchUnihashesAccessedParams takes ONE backend_id and
// parallel method/taskhash arrays, so pairs from different projects cannot share a
// statement (mirrors blob's per-(backend,namespace) grouping). Runs with NO LOCK
// HELD -- see flush's comment.
func (t *unihashToucher) writeTouches(ctx context.Context, due []unihashMark, staleness time.Duration) (int64, error) {
	byBackend := map[int64][]unihashMark{}
	for _, m := range due {
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

// FlushNow forces a SYNCHRONOUS flush of every pending mark, regardless of staleness
// (staleness=0: TouchUnihashesAccessed's own guard is accessed_at < now() -
// staleness, so 0 means "write it unless it was already touched in this exact
// instant"), and reports rows written.
//
// This is the "force-flush before the sweep" half of A: R6#1/R7#3 -- the GC's stage 1
// (SweepUnihashes) reads accessed_at off the DATABASE row, and pending()'s in-memory
// veto has no reach into a query the toucher has not run yet. A read that landed
// inside the current staleness window is invisible to the sweep's own SELECT right up
// until something forces it onto disk. Stage F4 calls this immediately before running
// the hashserv sweep stage, for exactly the same reason the GC loop already runs
// entirely single-instance (spec 9.4): there is exactly one process whose memory this
// can come from.
func (b *Backend) FlushNow(ctx context.Context) (int64, error) {
	if b.touch == nil {
		return 0, nil
	}

	return b.touch.flush(ctx, 0, true)
}
