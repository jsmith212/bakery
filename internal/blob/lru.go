package blob

import (
	"container/list"
	"hash/maphash"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jsmith212/bakery/internal/metrics"
)

// lruShards is 64, and it is not a round number picked for looks. A single-mutex
// LRU gets SLOWER as parallelism rises -- measured 170 ns/op at -cpu 8 degrading to
// 246 ns/op at -cpu 64, the signature of lock collapse. That is exactly backwards
// under a BB_NUMBER_THREADS-parallel HEAD storm, which is the only workload this
// cache exists for. Sharding at 64 measured 59.8 ns/op at -cpu 64 -- 4.2x -- and
// scales positively. 256 shards buys a further ~8% and costs 4x the memory floor.
const lruShards = 64

// lruCache is a sharded, capacity-bounded LRU over object metadata.
//
// IT CACHES NEGATIVE RESULTS, and that is not an optimisation -- it is the whole
// point. On the first build against an empty cache EVERY HEAD is a miss. A cache
// that only stores hits sends the entire setscene graph to Postgres on every build,
// and no test that pre-populates the repository will ever show it. Meta.Exists is
// therefore a real field, distinct from "not cached".
//
// The negative entries are only sound because bakery refuses to start a second
// instance without --allow-multi-instance: this process is the only writer, so it
// can invalidate its own cache exactly. Two writers and the negative cache is a
// stale-read generator.
type lruCache struct {
	shards [lruShards]lruShard
	seed   maphash.Seed

	// nowNano is the toucher's clock, injectable so the flusher tests can advance
	// time instead of sleeping through a one-hour staleness window. It is read on the
	// hot path ONLY when an entry is unmarked, so the indirect call is paid once per
	// key per flush period, never per lookup.
	nowNano func() int64

	hit     prometheus.Counter
	miss    prometheus.Counter
	add     prometheus.Counter
	evict   prometheus.Counter
	entries prometheus.Gauge
	bytes   prometheus.Gauge
}

type lruShard struct {
	mu sync.Mutex
	m  map[string]*list.Element
	ll *list.List // front = most recently used
	// cap is per shard. Keys hash uniformly, so a shard holding cap entries while
	// its neighbours are half empty is a hash failure, not a capacity failure.
	cap int
	// gen is a monotonic per-shard write generation, bumped under mu by every
	// AUTHORITATIVE mutation -- put and del, which are how Put/Delete publish and
	// invalidate. It is the ordering guard the negative cache needs: a statDB fill
	// reads the generation BEFORE its Postgres round-trip (lruCache.seq) and only
	// lands the result if the generation has not moved since (putIfUnchanged). That
	// is what stops a stale DB read from clobbering a fresh Put/Delete -- a lookup
	// that read "absent" cannot overwrite a concurrent Put's positive entry, and a
	// lookup that read an old digest cannot overwrite an overwrite's new one. Without
	// it the negative cache permanently 404s an object that exists, and the /ac/
	// stale-positive pins a digest the GC then reaps into ErrDanglingMetadata.
	gen uint64
}

type lruEntry struct {
	key  string
	meta Meta

	// markedAt is THE TOUCHER, and it is a FIELD ON THIS ENTRY rather than a second
	// 64-shard pending map on purpose (M6 spec 6.1, finding 6): a separate map would
	// double the hot path's lock acquisitions and force a string allocation on the one
	// path that must allocate nothing. Here the stamp rides the shard mutex the LRU hit
	// is ALREADY holding, and costs a compare against zero.
	//
	// Unix nanoseconds; 0 means "no unflushed read". IT IS STAMPED ONLY WHEN ZERO --
	// the value is "when the oldest unflushed read of this key happened", not "when the
	// last read happened". That is what makes N reads within the staleness window T
	// collapse into ONE UPDATE (TestToucherFlushIsCoalesced): the flusher takes entries
	// whose mark is older than T, and a re-stamping hot key would otherwise never grow
	// old enough to be flushed at all.
	markedAt int64
}

// entryOverhead is a rough per-entry footprint (map bucket + list element + entry
// struct + string header). bakery_lru_bytes is an ESTIMATE OF THE CACHE'S OWN
// MEMORY, not of the size of the cached objects -- caching a 4 GB sstate tarball's
// metadata costs the same as caching an empty file's.
const entryOverhead = 96

func newLRU(m *metrics.Metrics, capacity int) *lruCache {
	perShard := max(capacity/lruShards, 1)

	c := &lruCache{
		seed:    maphash.MakeSeed(),
		nowNano: func() int64 { return time.Now().UnixNano() },
		hit:     m.LRUEvents.WithLabelValues(metrics.EventHit),
		miss:    m.LRUEvents.WithLabelValues(metrics.EventMiss),
		add:     m.LRUEvents.WithLabelValues(metrics.EventAdd),
		evict:   m.LRUEvents.WithLabelValues(metrics.EventEvict),
		entries: m.LRUEntries,
		bytes:   m.LRUBytes,
	}

	for i := range c.shards {
		c.shards[i].m = make(map[string]*list.Element, perShard)
		c.shards[i].ll = list.New()
		c.shards[i].cap = perShard
	}

	return c
}

func (c *lruCache) shard(key []byte) *lruShard {
	return &c.shards[maphash.Bytes(c.seed, key)&(lruShards-1)]
}

// shardIndex is the string-keyed twin of shard, for the COLD paths (the toucher
// flusher and delBatch) that already hold a string. maphash.String is defined to
// equal maphash.Bytes over the same content, so the two agree on placement.
func (c *lruCache) shardIndex(key string) int {
	return int(maphash.String(c.seed, key) & (lruShards - 1))
}

// get is the sstate HEAD hot path.
//
// key is a []byte and stays one: `s.m[string(key)]` is the compiler's no-copy map
// lookup, so a hit allocates ZERO bytes. Taking a string parameter instead would
// allocate on every lookup, on the one path where that is not affordable.
func (c *lruCache) get(key []byte) (Meta, bool) {
	s := c.shard(key)

	s.mu.Lock()

	el, ok := s.m[string(key)]
	if !ok {
		s.mu.Unlock()
		c.miss.Inc()

		return Meta{}, false
	}

	s.ll.MoveToFront(el)

	e, ok := el.Value.(*lruEntry)
	if !ok {
		s.mu.Unlock()
		c.miss.Inc()

		return Meta{}, false
	}

	meta := e.meta

	// THE TOUCH, and this is the whole of it on the hot path: one compare and, at most
	// once per key per flush period, one clock read. See lruEntry.markedAt for why the
	// stamp is conditional rather than unconditional.
	//
	// A negative entry is marked too and it costs nothing to let it: the flusher's
	// UPDATE is keyed on rows that exist, so a mark for an absent key matches nothing.
	// Branching on meta.Exists here would put a second load on the hot path to save a
	// row in an array on a path that is already batched.
	if e.markedAt == 0 {
		e.markedAt = c.nowNano()
	}

	s.mu.Unlock()
	c.hit.Inc()

	return meta, true
}

// seq reads the current write generation of key's shard. A statDB fill reads it
// BEFORE its Postgres round-trip and hands it to putIfUnchanged; if an authoritative
// put/del bumped the generation in between, the fill is dropped. This is the whole
// mechanism that orders a stale read behind a fresh write.
func (c *lruCache) seq(key []byte) uint64 {
	s := c.shard(key)

	s.mu.Lock()
	g := s.gen
	s.mu.Unlock()

	return g
}

// insertLocked inserts or refreshes an entry; the caller holds s.mu. It does NOT
// touch s.gen -- generation bumping is the AUTHORITATIVE caller's decision (put/del
// bump, a statDB fill via putIfUnchanged does not, so one fill never masks another as
// a spurious invalidation). Returns whether a new entry was added and whether that
// insert evicted the LRU tail.
// markedAt is the toucher stamp to carry onto the entry, or 0 for "this write is not
// a read and must not claim one". An existing entry's stamp is never overwritten:
// markedAt means "oldest unflushed read", so the earlier value is the correct one.
func (s *lruShard) insertLocked(key []byte, meta Meta, markedAt int64) (added, evicted bool) {
	if el, ok := s.m[string(key)]; ok {
		if e, ok := el.Value.(*lruEntry); ok {
			e.meta = meta

			if e.markedAt == 0 {
				e.markedAt = markedAt
			}
		}

		s.ll.MoveToFront(el)

		return false, false
	}

	k := string(key)
	s.m[k] = s.ll.PushFront(&lruEntry{key: k, meta: meta, markedAt: markedAt})

	if s.ll.Len() > s.cap {
		if el := s.ll.Back(); el != nil {
			s.ll.Remove(el)

			if e, ok := el.Value.(*lruEntry); ok {
				delete(s.m, e.key)

				return true, true
			}
		}
	}

	return true, false
}

// recordInsert emits the LRU metrics for an insertLocked result, outside the shard
// lock. An update (added == false) is silent, matching the original behaviour.
func (c *lruCache) recordInsert(key []byte, added, evicted bool) {
	if !added {
		return
	}

	c.add.Inc()

	if evicted {
		c.evict.Inc()

		return
	}

	c.entries.Inc()
	c.bytes.Add(float64(len(key) + entryOverhead))
}

// put inserts or refreshes an entry. key is copied; the caller may reuse its buffer.
//
// It is an AUTHORITATIVE write -- Put's positive publish and Delete's negative
// publish both land here -- so it bumps the shard generation, which invalidates any
// statDB fill that read the metadata store before this write committed.
func (c *lruCache) put(key []byte, meta Meta) {
	s := c.shard(key)

	s.mu.Lock()
	s.gen++
	// No stamp: a PUT is a write, and accessed_at records reads. A row created by this
	// PUT has created_at = now(), which coalesce(accessed_at, created_at) already reads
	// as fresh, so claiming a read here would be both a lie and redundant.
	added, evicted := s.insertLocked(key, meta, 0)
	s.mu.Unlock()

	c.recordInsert(key, added, evicted)
}

// putIfUnchanged is the statDB fill. It lands meta ONLY if key's shard generation
// still equals seq -- the value seq had when statDB began its Postgres probe. If an
// authoritative put/del moved the generation while the probe was in flight, the fill
// is dropped and the authoritative entry stands. Returns whether the fill landed.
func (c *lruCache) putIfUnchanged(key []byte, meta Meta, seq uint64) bool {
	s := c.shard(key)

	// A POSITIVE fill IS A READ -- it is the answer to a cold lookup that found the row
	// -- so it carries a stamp. Without this, an object that a build fetches exactly
	// once per run (the sstate norm: one HEAD, one GET, then never again from this
	// process) would leave accessed_at NULL forever and age out on created_at alone
	// while being used every week. A NEGATIVE fill stamps nothing: there is no row to
	// touch and no liveness to record. The clock is read OUTSIDE the shard lock.
	var markedAt int64
	if meta.Exists {
		markedAt = c.nowNano()
	}

	s.mu.Lock()

	if s.gen != seq {
		s.mu.Unlock()

		return false
	}

	added, evicted := s.insertLocked(key, meta, markedAt)
	s.mu.Unlock()

	c.recordInsert(key, added, evicted)

	return true
}

// del drops an entry. Called after every write, because a stale POSITIVE entry
// serves the wrong digest and a stale NEGATIVE entry 404s an object that exists.
//
// Like put it is authoritative and bumps the generation UNCONDITIONALLY -- even when
// the key is not currently cached -- so an in-flight statDB fill for this key is
// invalidated regardless of whether there was an entry to remove.
func (c *lruCache) del(key []byte) {
	s := c.shard(key)

	s.mu.Lock()
	s.gen++

	el, ok := s.m[string(key)]
	if !ok {
		s.mu.Unlock()

		return
	}

	s.ll.Remove(el)
	delete(s.m, string(key))
	s.mu.Unlock()

	c.entries.Dec()
	c.bytes.Sub(float64(len(key) + entryOverhead))
}

// --- the toucher's view of the LRU (M6 spec 6.1, 6.2) -----------------------

// touchMark is one pending accessed_at write: the composed cache key (which encodes
// backend_id, namespace and key -- see Ref.appendCacheKey) and the stamp that was
// observed. The stamp travels so the clear pass can tell "the mark I flushed" from
// "a mark that appeared after I read it".
type touchMark struct {
	ck       string
	markedAt int64
}

// collectMarks appends every marked entry whose stamp is at or before cutoff, WITHOUT
// clearing it. Takes each shard lock once.
//
// COLLECT AND CLEAR ARE DELIBERATELY SEPARATE, and the gap between them is where the
// veto (spec 6.2) has to keep working: a mark that is cleared the instant it is
// collected is invisible to PendingTouch for the whole duration of the UPDATE, and a
// sweep probing in that window would delete a row whose accessed_at write is still in
// flight. So the mark stays visible until the write has committed, and clearMarks runs
// after it.
func (c *lruCache) collectMarks(cutoff int64, dst []touchMark) []touchMark {
	for i := range c.shards {
		s := &c.shards[i]

		s.mu.Lock()

		for el := s.ll.Front(); el != nil; el = el.Next() {
			e, ok := el.Value.(*lruEntry)
			if !ok || e.markedAt == 0 || e.markedAt > cutoff {
				continue
			}

			dst = append(dst, touchMark{ck: e.key, markedAt: e.markedAt})
		}

		s.mu.Unlock()
	}

	return dst
}

// clearMarks unmarks exactly the entries collectMarks reported, and only if their
// stamp is unchanged. An entry re-marked since (which can only happen after a flush
// cleared it, because a mark is stamped only when zero) keeps its newer mark.
func (c *lruCache) clearMarks(marks []touchMark) {
	var byShard [lruShards][]touchMark

	for _, m := range marks {
		i := c.shardIndex(m.ck)
		byShard[i] = append(byShard[i], m)
	}

	for i := range byShard {
		if len(byShard[i]) == 0 {
			continue
		}

		s := &c.shards[i]

		s.mu.Lock()

		for _, m := range byShard[i] {
			el, ok := s.m[m.ck]
			if !ok {
				continue
			}

			if e, ok := el.Value.(*lruEntry); ok && e.markedAt == m.markedAt {
				e.markedAt = 0
			}
		}

		s.mu.Unlock()
	}
}

// pendingMark answers "does this key hold an unflushed read?" -- the LRU half of the
// GC's veto (spec 6.2). See Service.PendingTouch for why an in-memory answer is
// sufficient.
func (c *lruCache) pendingMark(ck string) bool {
	s := &c.shards[c.shardIndex(ck)]

	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.m[ck]
	if !ok {
		return false
	}

	e, ok := el.Value.(*lruEntry)

	return ok && e.markedAt != 0
}

// delBatch is the GC's LRU invalidation (spec 9.2, finding 16). Keys are grouped by
// shard, each shard's mutex is taken ONCE, and the generation is bumped ONCE PER SHARD
// rather than once per key.
//
// THE GENERATION BUMP IS PER SHARD ON PURPOSE. A bump invalidates every statDB fill
// that read Postgres before it, so bumping once per key spreads a thousand
// invalidation points across the whole chunk: a cold HEAD whose probe starts after the
// shard's keys are gone still loses its fill to the NEXT key's bump, and keeps losing
// it for as long as the chunk runs -- suppressing the negative cache process-wide
// exactly when a build is stampeding. One bump at the instant the shard's slice is
// applied invalidates the fills that raced this delete and nothing else.
//
// IT INVALIDATES; IT DOES NOT DISPLACE, which is where it diverges from
// Service.Delete's negative fill (see the comment there). Delete removes ONE key a
// client just asked about, so caching the absence answers the HEAD that is about to
// arrive. The GC removes thousands of keys nobody has mentioned in ninety days;
// publishing negative entries for them would evict the working set of a running build
// and replace it with tombstones for objects that will never be requested again.
func (c *lruCache) delBatch(keys []string) {
	var byShard [lruShards][]string

	for _, k := range keys {
		i := c.shardIndex(k)
		byShard[i] = append(byShard[i], k)
	}

	var (
		removed int
		freed   int
	)

	for i := range byShard {
		if len(byShard[i]) == 0 {
			continue
		}

		s := &c.shards[i]

		s.mu.Lock()
		s.gen++

		for _, k := range byShard[i] {
			el, ok := s.m[k]
			if !ok {
				continue
			}

			s.ll.Remove(el)
			delete(s.m, k)

			removed++
			freed += len(k) + entryOverhead
		}

		s.mu.Unlock()
	}

	if removed > 0 {
		c.entries.Sub(float64(removed))
		c.bytes.Sub(float64(freed))
	}
}

// len is for tests and diagnostics; it takes every shard lock.
func (c *lruCache) len() int {
	n := 0

	for i := range c.shards {
		s := &c.shards[i]

		s.mu.Lock()
		n += s.ll.Len()
		s.mu.Unlock()
	}

	return n
}
