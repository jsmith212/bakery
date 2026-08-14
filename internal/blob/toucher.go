package blob

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// THE ACCESS TOUCHER (M6 spec 6.1, 6.2, 6.3).
//
// accessed_at is what separates "cold" from "merely old". Maintaining it inline is
// not an option: one UPDATE per HEAD funnels a BB_NUMBER_THREADS-parallel storm into
// a row-lock convoy on the hottest rows in the database, to write a column nobody
// reads in real time. So reads MARK, in memory, and a flusher coalesces the marks
// into one batched UPDATE per (backend, namespace) per tick.
//
// The marks live in TWO places and there is a reason for each:
//
//   - THE LRU ITSELF carries the mark for every ordinary read, on a field of the
//     entry, stamped under the shard mutex the lookup already holds. A separate
//     pending map for this was designed and rejected (spec finding 6): it doubles the
//     hot path's lock traffic and forces an allocation on the one path that must have
//     none. Pending is therefore bounded by LRU capacity for free, which is why there
//     is no --gc-touch-max-pending knob (finding 17).
//
//   - THE AUX MAP carries reachability marks that have no LRU lookup behind them:
//     spec 6.3's GetActionResult touch of an ActionResult's output digests. That is
//     ONE insert per RPC per output, on a path that has already unmarshalled a
//     protobuf -- nowhere near the HEAD storm -- so a plain sharded map is right, and
//     an LRU entry would be wrong (the outputs were not read, only named).

const (
	// defaultTouchStaleness is T when the caller supplies no ramp. It matches the SQL
	// guard in TouchObjectsAccessed: a key's accessed_at is written at most once per T
	// no matter how often it is read.
	defaultTouchStaleness = time.Hour

	// touchBatchSize caps the keys in one UPDATE. One statement per flush per group is
	// the goal; this only splits a group that has grown past a sane array parameter.
	touchBatchSize = 1000

	// finalFlushTimeout bounds the shutdown flush. It runs under
	// context.WithoutCancel, so without a deadline of its own a wedged pool would hold
	// shutdown open indefinitely.
	finalFlushTimeout = 5 * time.Second

	// auxShardCap bounds ONE aux shard. This is not the max-pending knob the spec
	// dissolved -- it is an OOM guard on the one pending set that is not bounded by
	// the LRU's capacity, and it is deliberately not configurable. Dropping a mark
	// costs a touch, never a wrong delete (spec 6.3: the worst case of any race here
	// is a missed touch), and the W_cas = 2 x W_ac ladder is the defence in depth
	// underneath it.
	auxShardCap = 1 << 14
)

// nsTags is the OCI tags namespace, and it is EXCLUDED from touching (spec 6.1).
//
// Structurally it is already excluded: tags are read through StatUncached and
// ListKeysByPrefix, neither of which traverses the LRU, so no tag key can ever be
// marked. This filter is the belt to that braces -- it keeps the exclusion true if
// some future caller reads a tag through Stat. A tag's freshness lives on updated_at,
// maintained by the SWR revalidation, and stage 7 of the sweep reads that column;
// accessed_at would be a second, disagreeing answer to the same question.
const nsTags = "tags"

// touchGroup is TouchObjectsAccessed's scope: one backend, one namespace.
type touchGroup struct {
	backendID int64
	namespace string
}

// auxPending is the sharded map behind MarkAccessed. Sharded for the same reason the
// LRU is: a single mutex gets slower as parallelism rises.
type auxPending struct {
	seed   maphash.Seed
	shards [lruShards]auxShard
}

type auxShard struct {
	mu sync.Mutex
	m  map[string]int64 // composed cache key -> mark (unix nanos)
}

func newAuxPending() *auxPending {
	a := &auxPending{seed: maphash.MakeSeed(), shards: [lruShards]auxShard{}}

	for i := range a.shards {
		a.shards[i].m = map[string]int64{}
	}

	return a
}

func (a *auxPending) shardIndex(ck string) int {
	return int(maphash.String(a.seed, ck) & (lruShards - 1))
}

// mark records an unflushed read of ck. FIRST MARK WINS, exactly as lruEntry.markedAt
// does: the stamp means "when the oldest unflushed read happened", which is what makes
// repeated reads inside T collapse into one UPDATE instead of continuously deferring
// the flush.
func (a *auxPending) mark(ck string, now int64) {
	s := &a.shards[a.shardIndex(ck)]

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.m[ck]; ok {
		return
	}

	if len(s.m) >= auxShardCap {
		return
	}

	s.m[ck] = now
}

// collect reports marks at or older than cutoff WITHOUT removing them -- see
// lruCache.collectMarks for why the clear is a separate pass.
func (a *auxPending) collect(cutoff int64, dst []touchMark) []touchMark {
	for i := range a.shards {
		s := &a.shards[i]

		s.mu.Lock()

		for ck, at := range s.m {
			if at <= cutoff {
				dst = append(dst, touchMark{ck: ck, markedAt: at})
			}
		}

		s.mu.Unlock()
	}

	return dst
}

// clear removes exactly the marks that were flushed, and only if unchanged.
func (a *auxPending) clear(marks []touchMark) {
	for _, m := range marks {
		s := &a.shards[a.shardIndex(m.ck)]

		s.mu.Lock()

		if at, ok := s.m[m.ck]; ok && at == m.markedAt {
			delete(s.m, m.ck)
		}

		s.mu.Unlock()
	}
}

func (a *auxPending) pending(ck string) bool {
	s := &a.shards[a.shardIndex(ck)]

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.m[ck]

	return ok
}

// MarkAccessed records a read of ref that did NOT go through the LRU.
//
// ITS CALLER IS THE REACHABILITY TOUCH (spec 6.3): a GetActionResult hit names output
// CAS digests that Bazel under --remote_download_minimal will never fetch, so without
// this the CAS blobs of a permanently hot action look untouched and age out from under
// their own ActionResult -- which Bazel answers with a build abort and rewind, not a
// clean miss. A touch has none of the risk a reachability SWEEP would have: it only
// makes rows younger, so the worst case of any race is a missed touch.
//
// COLD PATH ONLY. It allocates the cache key, takes a mutex, and is meant for
// one-RPC-per-action traffic. Do not put it behind anything that resembles the HEAD
// storm -- that is what lruEntry.markedAt is for.
func (s *Service) MarkAccessed(ref Ref) {
	var buf [512]byte

	s.aux.mark(string(ref.appendCacheKey(buf[:0])), s.lru.nowNano())
}

// PendingTouch is THE GC's VETO (spec 6.2): "does this process hold an unflushed read
// of this key?". The sweep intersects every candidate chunk against it and drops any
// key that answers true.
//
// WHY AN IN-MEMORY ANSWER IS SUFFICIENT, and this is the second load-bearing reason
// bakery refuses to run GC multi-instance (spec 9.4): a pending touch is invisible to
// the sweep's SELECT -- the row still carries its old accessed_at -- so a key read
// seconds ago can be selected as ninety days cold. The only complete record of those
// reads is in THIS process's memory, and it is complete only because this process is
// the only one serving. Under --allow-multi-instance the GC loop does not start, so
// there is no configuration in which this probe answers for reads it cannot see.
//
// It takes the triple rather than a Ref because its caller holds scan rows, not routed
// requests: the org/project slugs a Ref carries are metrics labels the GC has no use
// for here.
func (s *Service) PendingTouch(backendID int64, namespace, key string) bool {
	var buf [512]byte

	ck := string(Ref{
		BackendID: backendID,
		Org:       "", Project: "", Backend: "", Kind: "",
		Namespace: namespace, Key: key,
	}.appendCacheKey(buf[:0]))

	return s.lru.pendingMark(ck) || s.aux.pending(ck)
}

// StartAccessToucher runs the coalescing accessed_at flusher until ctx is cancelled,
// then flushes one last time. Wire it to the server's shutdown context and run it as a
// goroutine, exactly like auth.Service.StartKeyToucher.
//
// staleness is a FUNCTION, not a duration, because T ramps: the migration's fillfactor
// change is catalog-only, so the first touch of every pre-existing row is a non-HOT
// update, and the toucher opens at T = 24h for the first week to spread that one-time
// index-and-WAL spike before tightening to 1h (spec 6.4). The ramp policy belongs to
// the caller that knows when the migration ran; this loop only asks what T is now.
//
// THE FINAL FLUSH IS NOT OPTIONAL. StartKeyToucher has no equivalent and that is a
// recorded bug, not a pattern to copy: every mark collected since the last tick is
// lost on shutdown, and a restart-heavy deployment can therefore lose every read
// record it ever takes. It runs under context.WithoutCancel because ctx is already
// cancelled by the time it is reached, with its own bounded timeout so a wedged pool
// cannot hold shutdown open.
func (s *Service) StartAccessToucher(ctx context.Context, interval time.Duration, staleness func() time.Duration) {
	if s.tx == nil {
		s.log.WarnContext(ctx, "access toucher not started: blob service is read-only")

		return
	}

	if staleness == nil {
		staleness = func() time.Duration { return defaultTouchStaleness }
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
			defer cancel()

			if _, err := s.flushAccess(flushCtx, staleness(), true); err != nil {
				s.log.ErrorContext(flushCtx, "final accessed_at flush failed", slog.Any("error", err))
			}

			return

		case <-ticker.C:
			if _, err := s.flushAccess(ctx, staleness(), false); err != nil && !errors.Is(err, context.Canceled) {
				s.log.ErrorContext(ctx, "accessed_at flush failed", slog.Any("error", err))
			}
		}
	}
}

// flushAccess is one tick. all=true takes every mark regardless of age (the shutdown
// flush); otherwise it takes only marks older than staleness, which is what makes N
// reads within T cost ONE UPDATE rather than one per tick.
//
// Marks are cleared only after the write commits, and only the ones that were written.
// A failed flush therefore leaves the marks pending -- retried next tick, and still
// visible to PendingTouch in the meantime.
func (s *Service) flushAccess(ctx context.Context, staleness time.Duration, all bool) (int64, error) {
	cutoff := int64(math.MaxInt64)
	if !all {
		cutoff = s.lru.nowNano() - staleness.Nanoseconds()
	}

	marks := s.lru.collectMarks(cutoff, nil)
	auxMarks := s.aux.collect(cutoff, nil)

	groups := groupMarks(marks, auxMarks)
	if len(groups) == 0 {
		return 0, nil
	}

	var rows int64

	for g, keys := range groups {
		for i := 0; i < len(keys); i += touchBatchSize {
			n, err := s.touchChunk(ctx, g, keys[i:min(i+touchBatchSize, len(keys))], staleness)
			if err != nil {
				return rows, err
			}

			rows += n
		}
	}

	s.lru.clearMarks(marks)
	s.aux.clear(auxMarks)

	return rows, nil
}

// touchChunk issues ONE TouchObjectsAccessed. It is a write, so it goes through the
// Txer half like every other write in this package.
func (s *Service) touchChunk(
	ctx context.Context, g touchGroup, keys []string, staleness time.Duration,
) (int64, error) {
	var n int64

	err := s.tx.Tx(ctx, func(q *repository.Queries) error {
		s.qTouchAccessed.Inc()

		var err error

		n, err = q.TouchObjectsAccessed(ctx, repository.TouchObjectsAccessedParams{
			BackendID: g.backendID,
			Namespace: g.namespace,
			Keys:      keys,
			Staleness: pgtype.Interval{
				Microseconds: staleness.Microseconds(), Days: 0, Months: 0, Valid: true,
			},
		})
		if err != nil {
			return fmt.Errorf("touch objects accessed: %w", err)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return n, nil
}

// groupMarks folds both mark sources into one keys-per-(backend, namespace) map,
// deduped across the two: a key can hold an LRU mark and an aux mark at once (an AC
// hit named an output that a later CAS read also touched), and touching it twice in
// one statement is a wasted array slot.
func groupMarks(sets ...[]touchMark) map[touchGroup][]string {
	groups := map[touchGroup][]string{}
	seen := map[string]struct{}{}

	for _, set := range sets {
		for _, m := range set {
			if _, dup := seen[m.ck]; dup {
				continue
			}

			seen[m.ck] = struct{}{}

			backendID, namespace, key, ok := splitCacheKey(m.ck)
			if !ok || namespace == nsTags {
				continue
			}

			g := touchGroup{backendID: backendID, namespace: namespace}
			groups[g] = append(groups[g], key)
		}
	}

	return groups
}

// splitCacheKey inverts Ref.appendCacheKey: base36 backend id, NUL, namespace, NUL,
// key.
//
// The split is unambiguous because NEITHER SEPARATOR CAN OCCUR IN THE PAYLOAD: both
// namespace and key are Postgres `text`, and a text value cannot contain a NUL byte
// at all -- the wire protocol rejects it. So the first two NULs are always the two
// this encoding wrote.
func splitCacheKey(ck string) (backendID int64, namespace, key string, ok bool) {
	i := strings.IndexByte(ck, 0)
	if i < 0 {
		return 0, "", "", false
	}

	rest := ck[i+1:]

	j := strings.IndexByte(rest, 0)
	if j < 0 {
		return 0, "", "", false
	}

	id, err := strconv.ParseInt(ck[:i], 36, 64)
	if err != nil {
		return 0, "", "", false
	}

	return id, rest[:j], rest[j+1:], true
}
