package blob

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/metrics"
)

// The LRU is SHARDED (64 shards) because a single-mutex LRU gets SLOWER as
// parallelism rises -- exactly backwards under a BB_NUMBER_THREADS HEAD storm. These
// tests pin the behaviour the sharding must not change.
func TestLRU_PutGetDelete(t *testing.T) {
	c := newLRU(metrics.New(), 1024)

	key := []byte("backend\x00\x00sstate:busybox")
	want := Meta{Exists: true, Digest: Digest{1, 2, 3}, Size: 4096, UpdatedAt: time.Time{}}

	if _, ok := c.get(key); ok {
		t.Fatal("get() on an empty cache returned a hit")
	}

	c.put(key, want)

	got, ok := c.get(key)
	if !ok {
		t.Fatal("get() missed a key that was just put")
	}

	if got != want {
		t.Errorf("get() = %+v, want %+v", got, want)
	}

	// A NEGATIVE entry is a value, not an absence: it must be served as a hit.
	c.put(key, Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}})

	got, ok = c.get(key)
	if !ok {
		t.Fatal("a cached negative result was not served from the cache")
	}

	if got.Exists {
		t.Error("the negative entry did not overwrite the positive one")
	}

	c.del(key)

	if _, ok := c.get(key); ok {
		t.Error("get() returned a hit after del()")
	}
}

// putIfUnchanged is the ordering guard: a statDB fill lands ONLY if no authoritative
// put/del touched the key's shard since the generation was read. This pins the exact
// contract statDB relies on.
func TestLRU_PutIfUnchanged(t *testing.T) {
	c := newLRU(metrics.New(), 1024)

	key := []byte("backend\x00\x00sstate:busybox")
	positive := Meta{Exists: true, Digest: Digest{9}, Size: 42, UpdatedAt: time.Time{}}
	stale := Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}}

	// No intervening write: the fill lands.
	seq := c.seq(key)

	if !c.putIfUnchanged(key, positive, seq) {
		t.Fatal("putIfUnchanged dropped a fill with an unchanged generation")
	}

	if got, ok := c.get(key); !ok || got != positive {
		t.Fatalf("get() = %+v, %v; want %+v, true", got, ok, positive)
	}

	// A stale reader captured the generation BEFORE an authoritative put; its later
	// fill must be dropped and the authoritative entry must stand.
	staleSeq := c.seq(key)

	c.put(key, positive) // authoritative write bumps the generation

	if c.putIfUnchanged(key, stale, staleSeq) {
		t.Fatal("putIfUnchanged landed a fill whose generation was stale -- it clobbered an authoritative write")
	}

	if got, ok := c.get(key); !ok || !got.Exists {
		t.Fatalf("get() = %+v, %v; want the authoritative positive entry", got, ok)
	}

	// del also bumps the generation, so a fill captured before a delete is dropped.
	beforeDel := c.seq(key)

	c.del(key)

	if c.putIfUnchanged(key, positive, beforeDel) {
		t.Fatal("putIfUnchanged landed a fill whose generation predated a del -- it would resurrect a deleted key")
	}

	if _, ok := c.get(key); ok {
		t.Error("get() returned a hit for a key whose stale fill should have been dropped after del")
	}
}

// Capacity is per shard, so a cache configured for N entries holds at most N. The
// point of the assertion is that it is BOUNDED: an unbounded metadata cache in front
// of a multi-million-object sstate mirror is a memory leak with a good reputation.
func TestLRU_IsBounded(t *testing.T) {
	const capacity = lruShards * 4

	c := newLRU(metrics.New(), capacity)

	for i := range 10_000 {
		c.put([]byte(fmt.Sprintf("k%d", i)), Meta{Exists: true, Digest: Digest{}, Size: int64(i), UpdatedAt: time.Time{}})
	}

	if got := c.len(); got > capacity {
		t.Errorf("len() = %d, want <= %d -- the LRU is not bounded", got, capacity)
	}
}

// countMarked walks every shard and reports (entries actually carrying a mark, the sum
// of the shards' own counters). They must agree.
func (c *lruCache) countMarked() (actual, claimed int) {
	for i := range c.shards {
		s := &c.shards[i]

		s.mu.Lock()

		claimed += s.marked

		for el := s.ll.Front(); el != nil; el = el.Next() {
			if e, ok := el.Value.(*lruEntry); ok && e.markedAt != 0 {
				actual++
			}
		}

		s.mu.Unlock()
	}

	return actual, claimed
}

// THE MARK COUNTER MUST NOT DRIFT.
//
// collectMarks skips a shard whose counter reads zero and stops walking once it has
// seen that many marks, which is what keeps the flusher off an O(capacity) walk of a
// half-million-entry cache with the shard mutex held. That optimisation is only sound
// while the counter is exact: an UNDERCOUNT silently abandons real marks -- a lost
// accessed_at, then a row the sweep deletes as cold that a build read minutes ago --
// and it drifts from the paths that are easy to forget, which is why this exercises
// every one of them (stamp on hit, stamp on cold fill, clear on flush, eviction, del,
// delBatch) rather than just the happy path.
func TestLRU_MarkedCounterTracksEveryPath(t *testing.T) {
	// Roomy enough that the first phases cannot evict (the removals must act on LIVE,
	// marked entries), and small enough that the flood at the end certainly does.
	const capacity = lruShards * 16

	c := newLRU(metrics.New(), capacity)
	c.nowNano = func() int64 { return 1 }

	hit := Meta{Exists: true, Digest: Digest{9}, Size: 1, UpdatedAt: time.Time{}}
	miss := Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}}

	keys := make([][]byte, 64)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "marked-key-%03d", i)
	}

	assert := func(step string) {
		t.Helper()

		actual, claimed := c.countMarked()
		if actual != claimed {
			t.Fatalf("after %s: %d entries carry a mark but the shard counters claim %d", step, actual, claimed)
		}
	}

	// Stamped by a HIT (positive only), and by a cold POSITIVE fill.
	for _, k := range keys {
		c.put(k, hit)
		c.get(k)
	}

	assert("hits")

	for i, k := range keys {
		if i%2 == 0 {
			c.putIfUnchanged(k, hit, c.seq(k))
		} else {
			c.putIfUnchanged(k, miss, c.seq(k))
		}
	}

	assert("cold fills")

	// Cleared by a flush.
	c.clearMarks(c.collectMarks(1, nil))
	assert("clearMarks")

	if actual, _ := c.countMarked(); actual != 0 {
		t.Errorf("%d marks survived a flush that collected every one of them", actual)
	}

	// Re-marked, then REMOVED WHILE STILL MARKED -- which is the case each removal path
	// has to account for, and the one an unlucky test misses by deleting keys that were
	// already evicted.
	// The odd keys were re-filled NEGATIVE above and a negative entry is never marked,
	// so republish them positive first -- the subject here is the counter, not the
	// Exists guard (TestLRU_NegativeEntriesAreNotMarked owns that).
	for _, k := range keys {
		c.put(k, hit)
		c.get(k)
	}

	if actual, _ := c.countMarked(); actual != len(keys) {
		t.Fatalf("%d entries carry a mark before the removals, want %d", actual, len(keys))
	}

	for _, k := range keys[:len(keys)/2] {
		c.del(k)
	}

	assert("del")

	batch := make([]string, 0, len(keys)/2)
	for _, k := range keys[len(keys)/2:] {
		batch = append(batch, string(k))
	}

	c.delBatch(batch)
	assert("delBatch")

	if _, claimed := c.countMarked(); claimed != 0 {
		t.Fatalf("the shard counters claim %d marks after every marked entry was removed", claimed)
	}

	// Eviction: each shard holds capacity/lruShards entries, so this overflows all of
	// them with marked entries, and every evicted mark must leave the counter with it.
	for i := range 20 * capacity {
		k := fmt.Appendf(nil, "flood-%04d", i)
		c.put(k, hit)
		c.get(k)
	}

	assert("eviction")

	if actual, _ := c.countMarked(); actual == 0 {
		t.Error("the eviction flood left no marks at all -- the step asserted nothing")
	}
}

// A NEGATIVE ENTRY NAMES NO ROW, so it must never be marked (R8#4). On the first build
// against an empty cache EVERY entry is negative, so a toucher that flushed them would
// spend its entire first hour issuing UPDATEs guaranteed to match zero rows -- one
// index probe per absent key, on the database the HEAD storm is already saturating.
func TestLRU_NegativeEntriesAreNotMarked(t *testing.T) {
	c := newLRU(metrics.New(), 1024)
	c.nowNano = func() int64 { return 1 }

	absent := []byte("1\x00\x00absent")
	present := []byte("1\x00\x00present")

	c.put(absent, Meta{Exists: false, Digest: Digest{}, Size: 0, UpdatedAt: time.Time{}})
	c.put(present, Meta{Exists: true, Digest: Digest{1}, Size: 1, UpdatedAt: time.Time{}})

	for range 5 {
		c.get(absent)
		c.get(present)
	}

	marks := c.collectMarks(1, nil)
	if len(marks) != 1 {
		t.Fatalf("collectMarks() returned %d marks, want 1 (the positive entry only)", len(marks))
	}

	if marks[0].ck != string(present) {
		t.Errorf("collectMarks() marked %q, want the positive entry %q", marks[0].ck, present)
	}
}

func TestLRU_ConcurrentAccessIsRaceFree(t *testing.T) {
	c := newLRU(metrics.New(), 4096)

	var wg sync.WaitGroup

	for g := range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range 500 {
				key := fmt.Appendf(nil, "g%d-k%d", g, i%64)

				c.put(key, Meta{Exists: true, Digest: Digest{}, Size: int64(i), UpdatedAt: time.Time{}})
				c.get(key)

				if i%7 == 0 {
					c.del(key)
				}
			}
		}()
	}

	wg.Wait()
}
