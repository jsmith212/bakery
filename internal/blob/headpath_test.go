package blob

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// THE HEAD PATH.
//
// BitBake fires a BB_NUMBER_THREADS-parallel HEAD storm over the whole setscene
// graph at the start of EVERY build. HEAD -- not GET -- is therefore the hot path,
// and DESIGN.md is explicit that it gets benchmarked in M1, before any backend
// exists to hide the cost.
//
// The metric that matters is DB QUERIES PER LOOKUP, not nanoseconds. An LRU hit that
// still touches Postgres is exactly the failure this file exists to catch, and it is
// invisible to a test that only asserts on the returned value: the answer is right,
// the build is slow, and nobody knows why. Benchmarks do not fail builds, so the
// three properties below are TESTS. The benchmarks report db/op alongside ns/op so a
// regression is visible in the numbers too.

func testRef(key string) Ref {
	return Ref{
		BackendID: 1,
		Org:       "acme",
		Project:   "widget",
		Backend:   metrics.BackendSstate,
		Kind:      "object",
		Namespace: "",
		Key:       key,
	}
}

// sstateKey looks like a real one: BitBake's keys are ~120-160 bytes and embed the
// unihash. A 5-byte key would make the cache look faster than it is.
func sstateKey(i int) string {
	return fmt.Sprintf(
		"universal/%02x/%02x/sstate:busybox:core2-64-poky-linux:1.36.1:r0:core2-64:3:%064x_package_write_rpm.tar.zst",
		i&0xff, (i>>8)&0xff, i,
	)
}

func digestOf(i int) []byte {
	d := sha256.Sum256([]byte(strconv.Itoa(i)))

	return d[:]
}

// newTestService wires a Service over the query-counting fake and a real local store
// (never touched on the stat path; New requires it).
func newTestService(tb testing.TB, r *fakeReader, cacheSize int) *Service {
	tb.Helper()

	st, err := storage.NewLocal(tb.TempDir())
	if err != nil {
		tb.Fatalf("NewLocal() error = %v", err)
	}

	svc, err := New(Config{
		Reader:    r,
		Tx:        nil, // read path only
		Storage:   st,
		Metrics:   metrics.New(),
		CacheSize: cacheSize,
	})
	if err != nil {
		tb.Fatalf("New() error = %v", err)
	}

	return svc
}

// THE SINGLE MOST IMPORTANT TEST IN M1.
//
// 512 warm keys, then 20,000 lookups across 64 goroutines, and Postgres must see
// EXACTLY ZERO additional queries. If this ever goes red, every bitbake HEAD storm is
// hitting the database and the cache is decoration.
func TestExists_LRUHitIssuesZeroQueries(t *testing.T) {
	const (
		keys       = 512
		goroutines = 64
		lookups    = 20_000
	)

	repo := newFakeReader()
	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(1000+i))
	}

	svc := newTestService(t, repo, 4096)

	// Warm: one query per key, and no more.
	for i := range keys {
		ok, err := svc.Exists(t.Context(), testRef(sstateKey(i)))
		if err != nil || !ok {
			t.Fatalf("warm Exists(%d) = %v, %v; want true, nil", i, ok, err)
		}
	}

	if got := repo.queries.Load(); got != keys {
		t.Fatalf("warming issued %d queries, want %d (one per key)", got, keys)
	}

	warm := repo.queries.Load()

	var wg sync.WaitGroup

	for g := range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range lookups / goroutines {
				ok, err := svc.Exists(t.Context(), testRef(sstateKey((i*7+g)%keys)))
				if err != nil || !ok {
					t.Errorf("Exists() = %v, %v; want true, nil", ok, err)

					return
				}
			}
		}()
	}

	wg.Wait()

	if got := repo.queries.Load() - warm; got != 0 {
		t.Errorf("%d Postgres queries on a warm LRU, want 0 -- the HEAD storm is hitting the database", got)
	}
}

// The storm, collapsed. 256 goroutines released simultaneously onto ONE cold key must
// produce exactly ONE query.
//
// The 20 ms fake latency is load-bearing: with a zero-latency fake the race window is
// too small to observe and this test passes even with singleflight deleted. A gate
// that cannot fail is decoration.
func TestExists_SingleflightCollapsesTheStorm(t *testing.T) {
	const goroutines = 256

	repo := newFakeReader()
	repo.latency = 20 * time.Millisecond
	repo.add(sstateKey(1), digestOf(1), 4096)

	svc := newTestService(t, repo, 1024)

	start := make(chan struct{})

	var wg sync.WaitGroup

	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			ok, err := svc.Exists(t.Context(), testRef(sstateKey(1)))
			if err != nil || !ok {
				t.Errorf("Exists() = %v, %v; want true, nil", ok, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if got := repo.queries.Load(); got != 1 {
		t.Errorf("%d goroutines on one cold key issued %d queries, want 1", goroutines, got)
	}
}

// THE TRAP NOBODY SEES COMING.
//
// On the first build against an EMPTY cache, every HEAD is a MISS. A cache that only
// stores positive results therefore sends the entire setscene graph to Postgres on
// every build -- and this never shows up in a test that pre-populates the repository,
// because in such a test nothing is ever absent.
//
// Meta.Exists is an explicit field for exactly this reason: "absent" is a value the
// cache holds, not the absence of a value.
func TestExists_NegativeResultIsCached(t *testing.T) {
	repo := newFakeReader() // empty: every key is a miss
	svc := newTestService(t, repo, 1024)

	for range 100 {
		ok, err := svc.Exists(t.Context(), testRef(sstateKey(42)))
		if err != nil {
			t.Fatalf("Exists() error = %v", err)
		}

		if ok {
			t.Fatal("Exists() = true for an absent key")
		}
	}

	if got := repo.queries.Load(); got != 1 {
		t.Errorf("100 lookups of an absent key issued %d queries, want 1 -- negative results are not cached", got)
	}
}

// A miss is (false, nil), NOT an error. sstate misses render as 404, never 403 and
// never a 500: BitBake retries a 403 as a full-body GET, and a 500 fails the build.
func TestStat_MissIsNotAnError(t *testing.T) {
	svc := newTestService(t, newFakeReader(), 16)

	meta, err := svc.Stat(t.Context(), testRef(sstateKey(1)))
	if err != nil {
		t.Fatalf("Stat() on a miss returned an error: %v", err)
	}

	if meta.Exists {
		t.Error("Stat() reported Exists on an empty repository")
	}
}

// The LRU/singleflight key is namespaced by backend_id. Two projects hold the same
// sstate key constantly (identical recipe, identical unihash); a cache keyed on the
// bare key would serve one project's bytes to the other. That is cross-tenant cache
// poisoning, from a one-line cache-key bug.
func TestCacheKeyIsNamespacedByBackend(t *testing.T) {
	repo := newFakeReader()
	repo.add(sstateKey(7), digestOf(7), 1234)

	svc := newTestService(t, repo, 1024)

	a := testRef(sstateKey(7))
	a.BackendID = 1

	b := testRef(sstateKey(7))
	b.BackendID = 2

	if string(a.appendCacheKey(nil)) == string(b.appendCacheKey(nil)) {
		t.Fatal("two backends produced the same cache key")
	}

	if _, err := svc.Exists(t.Context(), a); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	// The fake keys on Key alone, so it would answer "exists" for backend 2 as well --
	// what is being asserted is that the LRU does NOT serve backend 2 from backend 1's
	// entry, i.e. that a second query is issued.
	if _, err := svc.Exists(t.Context(), b); err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	if got := repo.queries.Load(); got != 2 {
		t.Errorf("queries = %d, want 2 -- backend 2 was served from backend 1's cache entry", got)
	}
}

// --- benchmarks -------------------------------------------------------------
//
// db/op is the number that matters. ns/op is measured against a FAKE repository, so
// it is the ceiling the code can hit, not end-to-end request latency.

func BenchmarkExists_LRUHot(b *testing.B) {
	const keys = 512

	repo := newFakeReader()

	refs := make([]Ref, keys)

	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(i))

		refs[i] = testRef(sstateKey(i)) // precomputed: the benchmark must not measure fmt.Sprintf
	}

	svc := newTestService(b, repo, 4096)

	for i := range keys {
		if _, err := svc.Exists(b.Context(), refs[i]); err != nil {
			b.Fatalf("warm: %v", err)
		}
	}

	before := repo.queries.Load()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0

		for pb.Next() {
			if _, err := svc.Exists(b.Context(), refs[i%keys]); err != nil {
				b.Error(err)

				return
			}

			i++
		}
	})

	b.StopTimer()
	b.ReportMetric(float64(repo.queries.Load()-before)/float64(b.N), "db/op")
}

func BenchmarkExists_LRUCold(b *testing.B) {
	const keys = 1 << 16

	repo := newFakeReader()

	refs := make([]Ref, keys)
	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(i))

		refs[i] = testRef(sstateKey(i))
	}

	// One entry per shard: every lookup evicts and every lookup is a miss.
	svc := newTestService(b, repo, lruShards)

	before := repo.queries.Load()

	b.ResetTimer()
	b.ReportAllocs()

	var n atomic.Int64

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)

			if _, err := svc.Exists(b.Context(), refs[i%keys]); err != nil {
				b.Error(err)

				return
			}
		}
	})

	b.StopTimer()
	b.ReportMetric(float64(repo.queries.Load()-before)/float64(b.N), "db/op")
}

// The storm itself: every goroutine asking for the SAME key at the same time.
func BenchmarkExists_SingleflightContention(b *testing.B) {
	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 4096)

	svc := newTestService(b, repo, 1024)
	ref := testRef(sstateKey(1))

	before := repo.queries.Load()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := svc.Exists(b.Context(), ref); err != nil {
				b.Error(err)

				return
			}
		}
	})

	b.StopTimer()
	b.ReportMetric(float64(repo.queries.Load()-before)/float64(b.N), "db/op")
}
