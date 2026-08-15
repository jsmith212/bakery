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

	// marked is how many of this shard's entries currently carry a non-zero
	// markedAt. It exists so collectMarks is O(marks) instead of O(capacity):
	// without it the flusher walks EVERY entry of EVERY shard once a minute with
	// the shard mutex held -- measured at 725us-1.6ms per shard on a full
	// 500k-entry cache, i.e. tens of milliseconds of lock time per tick spread
	// across the 64 shards the HEAD storm is trying to use. With it, a shard that
	// holds no marks is skipped after one integer read, and a shard that holds a
	// few stops walking as soon as it has seen them all.
	//
	// IT IS MAINTAINED BY EVERY PATH THAT CREATES OR DESTROYS A MARK, and it must
	// be: an undercount silently skips real marks (a lost touch, then a wrongly
	// swept row), an overcount only costs a full walk. The paths are get and
	// insertLocked (stamp), clearMarks (flush), and every removal -- the
	// insertLocked eviction, del and delBatch -- because an entry that leaves the
	// shard takes its mark with it.
	marked int
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
	// ONLY A POSITIVE ENTRY IS MARKED. A negative entry names NO ROW: there is nothing
	// for TouchObjectsAccessed to update, so flushing it costs an array slot, a
	// statement's worth of parameter space and a scan of the (backend, namespace, key)
	// index for a guaranteed zero-row result -- and on the first build against an empty
	// cache EVERY entry is negative, so that is the whole flush. The guard is FREE
	// rather than "a second load on the hot path" (what this comment used to claim):
	// e.meta was already loaded into meta on the line above for the return value, so
	// meta.Exists is a register test, not a memory access.
	if meta.Exists && e.markedAt == 0 {
		e.markedAt = c.nowNano()
		s.marked++
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

			if e.markedAt == 0 && markedAt != 0 {
				e.markedAt = markedAt
				s.marked++
			}
		}

		s.ll.MoveToFront(el)

		return false, false
	}

	k := string(key)
	s.m[k] = s.ll.PushFront(&lruEntry{key: k, meta: meta, markedAt: markedAt})

	if markedAt != 0 {
		s.marked++
	}

	if s.ll.Len() > s.cap {
		if el := s.ll.Back(); el != nil {
			s.ll.Remove(el)

			if e, ok := el.Value.(*lruEntry); ok {
				delete(s.m, e.key)

				// An evicted entry takes its pending mark with it -- that is the
				// bounded-staleness trade spec 6.1 accepts (a mark lost to eviction costs
				// a touch, never a wrong delete). The COUNTER must follow it out, or the
				// shard claims marks that no longer exist and collectMarks walks the whole
				// list looking for them.
				if e.markedAt != 0 {
					s.marked--
				}

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

	if e, ok := el.Value.(*lruEntry); ok && e.markedAt != 0 {
		s.marked--
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
//
// WHAT IT COSTS IS THE POINT. This runs every --gc-touch-interval (1m) against a cache
// the HEAD storm is hammering, so the work under each shard mutex is bounded three
// ways: a shard with no marks is skipped on an integer compare; the walk stops as soon
// as it has seen lruShard.marked marked entries rather than reaching the tail; and the
// results land in a per-shard slice sized from that same counter, appended to dst only
// AFTER the mutex is released -- growing the shared dst slice (a copy of everything
// collected so far, once per doubling) is not work to do while holding a lock that a
// bitbake HEAD is queued behind.
func (c *lruCache) collectMarks(cutoff int64, dst []touchMark) []touchMark {
	for i := range c.shards {
		s := &c.shards[i]

		s.mu.Lock()

		if s.marked == 0 {
			s.mu.Unlock()

			continue
		}

		local := make([]touchMark, 0, s.marked)

		// found counts marked entries SEEN, including ones too young to flush --
		// otherwise a shard whose marks are all inside the staleness window would walk
		// to the tail every tick, which is exactly the cost this is removing.
		found := 0

		for el := s.ll.Front(); el != nil && found < s.marked; el = el.Next() {
			e, ok := el.Value.(*lruEntry)
			if !ok || e.markedAt == 0 {
				continue
			}

			found++

			if e.markedAt > cutoff {
				continue
			}

			local = append(local, touchMark{ck: e.key, markedAt: e.markedAt})
		}

		s.mu.Unlock()

		dst = append(dst, local...)
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
				s.marked--
			}
		}

		s.mu.Unlock()
	}
}

// pendingMark answers "does this key hold an unflushed read?" -- the LRU half of the
// GC's veto (spec 6.2). See Service.PendingTouch for why an in-memory answer is
// sufficient.
//
// IT TAKES []byte, not string, for the same reason get does: `s.m[string(ck)]` is the
// compiler's no-copy map lookup, so the caller can compose the cache key into a stack
// buffer and this probe allocates nothing. The sweep calls it once per candidate ROW
// -- a 1000-row chunk, chunk after chunk, for every backend -- so a string conversion
// here is one heap allocation per doomed object, which is precisely the shape of
// garbage the veto has no reason to make.
func (c *lruCache) pendingMark(ck []byte) bool {
	s := c.shard(ck)

	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.m[string(ck)]
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

			if e, ok := el.Value.(*lruEntry); ok && e.markedAt != 0 {
				s.marked--
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
