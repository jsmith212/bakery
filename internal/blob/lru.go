package blob

import (
	"container/list"
	"hash/maphash"
	"sync"

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
func (s *lruShard) insertLocked(key []byte, meta Meta) (added, evicted bool) {
	if el, ok := s.m[string(key)]; ok {
		if e, ok := el.Value.(*lruEntry); ok {
			e.meta = meta
		}

		s.ll.MoveToFront(el)

		return false, false
	}

	k := string(key)
	s.m[k] = s.ll.PushFront(&lruEntry{key: k, meta: meta})

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
	added, evicted := s.insertLocked(key, meta)
	s.mu.Unlock()

	c.recordInsert(key, added, evicted)
}

// putIfUnchanged is the statDB fill. It lands meta ONLY if key's shard generation
// still equals seq -- the value seq had when statDB began its Postgres probe. If an
// authoritative put/del moved the generation while the probe was in flight, the fill
// is dropped and the authoritative entry stands. Returns whether the fill landed.
func (c *lruCache) putIfUnchanged(key []byte, meta Meta, seq uint64) bool {
	s := c.shard(key)

	s.mu.Lock()

	if s.gen != seq {
		s.mu.Unlock()

		return false
	}

	added, evicted := s.insertLocked(key, meta)
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
