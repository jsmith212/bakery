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
