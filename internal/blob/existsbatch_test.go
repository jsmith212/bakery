package blob

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE BATCH HEAD PATH.
//
// REAPI FindMissingBlobs asks "which of these N digests do you have?" in ONE RPC,
// and Bazel/moon repeat a digest within a single request. ExistsBatch answers all N
// with ONE query, returns a result POSITIONALLY ALIGNED with the input, and holds
// the same two properties Exists does: an LRU hit issues zero queries, and a miss is
// negative-cached. Like headpath_test.go, the metric that matters is DB QUERIES PER
// BATCH, not nanoseconds -- the fake counts StatObjectsBatch as ONE call regardless
// of how many keys it carries.

// A warm LRU must send NOTHING to Postgres, exactly as it must for the single-key
// HEAD storm. This is the batch analog of TestExists_LRUHitIssuesZeroQueries.
func TestExistsBatch_WarmLRUIssuesZeroQueries(t *testing.T) {
	const keys = 512

	repo := newFakeReader()
	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(1000+i))
	}

	svc := newTestService(t, repo, 4096)

	refs := make([]Ref, keys)
	for i := range keys {
		refs[i] = testRef(sstateKey(i))
	}

	// Warm every key. Cold, this is exactly one batch query.
	out, err := svc.ExistsBatch(t.Context(), refs)
	if err != nil {
		t.Fatalf("warm ExistsBatch() error = %v", err)
	}

	for i, ok := range out {
		if !ok {
			t.Fatalf("warm ExistsBatch()[%d] = false; want true", i)
		}
	}

	if got := repo.queries.Load(); got != 1 {
		t.Fatalf("warming issued %d queries, want 1 (one batch)", got)
	}

	warm := repo.queries.Load()

	// Now the LRU is hot: repeated batches must add zero queries.
	for range 50 {
		out, err := svc.ExistsBatch(t.Context(), refs)
		if err != nil {
			t.Fatalf("warm ExistsBatch() error = %v", err)
		}

		for i, ok := range out {
			if !ok {
				t.Fatalf("warm ExistsBatch()[%d] = false; want true", i)
			}
		}
	}

	if got := repo.queries.Load() - warm; got != 0 {
		t.Errorf("%d Postgres queries on a warm LRU, want 0", got)
	}
}

// The residue is ONE query. 500 refs, 250 already warm in the LRU; the remaining 250
// misses must collapse into exactly one StatObjectsBatch, not 250 StatObject probes.
func TestExistsBatch_OneQueryForTheResidue(t *testing.T) {
	const total = 500

	repo := newFakeReader()
	for i := range total {
		repo.add(sstateKey(i), digestOf(i), int64(1000+i))
	}

	svc := newTestService(t, repo, 4096)

	// Warm the first 250 individually (one query each).
	for i := range total / 2 {
		if _, err := svc.Exists(t.Context(), testRef(sstateKey(i))); err != nil {
			t.Fatalf("warm Exists(%d) error = %v", i, err)
		}
	}

	if got := repo.queries.Load(); got != total/2 {
		t.Fatalf("warming issued %d queries, want %d", got, total/2)
	}

	before := repo.queries.Load()

	// A batch over all 500: 250 are warm hits, 250 are the residue.
	refs := make([]Ref, total)
	for i := range total {
		refs[i] = testRef(sstateKey(i))
	}

	out, err := svc.ExistsBatch(t.Context(), refs)
	if err != nil {
		t.Fatalf("ExistsBatch() error = %v", err)
	}

	for i, ok := range out {
		if !ok {
			t.Fatalf("ExistsBatch()[%d] = false; want true", i)
		}
	}

	if got := repo.queries.Load() - before; got != 1 {
		t.Errorf("residue of 250 misses issued %d queries, want exactly 1", got)
	}
}

// A cold, all-miss batch must negative-cache every key: a repeat batch adds zero
// queries. A moon build's FindMissingBlobs is all-miss on the first build, so a
// positive-only fill would re-query every digest on every request.
func TestExistsBatch_CachesNegatives(t *testing.T) {
	const keys = 300

	repo := newFakeReader() // empty: every key is a miss
	svc := newTestService(t, repo, 4096)

	refs := make([]Ref, keys)
	for i := range keys {
		refs[i] = testRef(sstateKey(i))
	}

	for round := range 5 {
		out, err := svc.ExistsBatch(t.Context(), refs)
		if err != nil {
			t.Fatalf("round %d ExistsBatch() error = %v", round, err)
		}

		for i, ok := range out {
			if ok {
				t.Fatalf("round %d ExistsBatch()[%d] = true for an absent key", round, i)
			}
		}
	}

	if got := repo.queries.Load(); got != 1 {
		t.Errorf("5 all-miss batches issued %d queries, want 1 -- negatives are not cached", got)
	}
}

// The result is positionally aligned with the input, and duplicates -- which Bazel
// and moon DO send within one request -- are answered at every position, not deduped
// out of the result. Dedup is internal (one query, one fill); the output slice keeps
// its shape.
func TestExistsBatch_PositionalAlignmentWithDuplicates(t *testing.T) {
	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 10) // exists
	repo.add(sstateKey(3), digestOf(3), 30) // exists
	// sstateKey(2) is absent.

	svc := newTestService(t, repo, 1024)

	// Interleave present/absent AND repeat keys.
	refs := []Ref{
		testRef(sstateKey(1)), // T
		testRef(sstateKey(2)), // F
		testRef(sstateKey(1)), // T (dup)
		testRef(sstateKey(3)), // T
		testRef(sstateKey(2)), // F (dup)
		testRef(sstateKey(1)), // T (dup)
	}
	want := []bool{true, false, true, true, false, true}

	out, err := svc.ExistsBatch(t.Context(), refs)
	if err != nil {
		t.Fatalf("ExistsBatch() error = %v", err)
	}

	if len(out) != len(want) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(want))
	}

	for i := range want {
		if out[i] != want[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], want[i])
		}
	}

	// Three distinct keys, one namespace/backend => exactly one batch query.
	if got := repo.queries.Load(); got != 1 {
		t.Errorf("issued %d queries, want 1 (duplicates dedup internally)", got)
	}
}

// THE GUARD MOST LIKELY TO BITE.
//
// A batch probe reads seq BEFORE its round trip; if an authoritative Put/Delete
// bumps the shard generation while the query is in flight, the batch fill MUST be
// dropped -- landing a stale "absent" on top of a committed Put is a permanent 404
// served from memory. This drives that exact race: the batch is gated mid-query, a
// Put publishes a positive entry for the same key, then the query completes. The
// subsequent Stat must see the Put's entry, never the batch's stale miss.
func TestExistsBatch_DoesNotClobberAConcurrentPut(t *testing.T) {
	const key = 42

	repo := newFakeReader()
	// The row is absent in the fake: the batch will read "miss" and try to
	// negative-cache it. Meanwhile a Put publishes a positive LRU entry directly.
	repo.entered = make(chan struct{})
	repo.gate = make(chan struct{})

	svc := newTestService(t, repo, 1024)
	ref := testRef(sstateKey(key))

	var (
		out []bool
		err error
		wg  sync.WaitGroup
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		out, err = svc.ExistsBatch(t.Context(), []Ref{ref})
	}()

	// Wait until the batch query is genuinely in flight (seq already captured).
	<-repo.entered

	// An authoritative Put lands a positive entry for the same key, bumping the
	// shard generation past the seq the batch captured.
	var buf [512]byte

	ck := ref.appendCacheKey(buf[:0])
	svc.lru.put(ck, Meta{Exists: true, Digest: Digest{}, Size: 99, UpdatedAt: time.Now()})

	// Release the batch query; its stale "absent" fill must be dropped.
	close(repo.gate)
	wg.Wait()

	if err != nil {
		t.Fatalf("ExistsBatch() error = %v", err)
	}

	if len(out) != 1 || out[0] {
		// The batch's own return value reflects the DB it read (absent) -- that is a
		// benign stale positive/negative at the RPC boundary. What must NOT happen is
		// the LRU getting clobbered.
		t.Logf("batch returned %v (from the DB it read); the LRU is what matters", out)
	}

	// The load-bearing assertion: the LRU still holds the Put's POSITIVE entry, so a
	// later lookup is a hit for an object that exists -- not a permanent 404.
	meta, ok := svc.lru.get(ck)
	if !ok {
		t.Fatal("LRU entry for the key vanished; the batch fill or Put did not land")
	}

	if !meta.Exists {
		t.Fatal("batch's stale 'absent' clobbered the concurrent Put's positive entry -- permanent 404 from memory")
	}

	if meta.Size != 99 {
		t.Errorf("LRU size = %d, want 99 (the Put's entry)", meta.Size)
	}
}

// --- benchmark --------------------------------------------------------------

// BenchmarkExistsBatch_Cold reports db/batch: the number that proves ONE query
// answers a whole FindMissingBlobs, no matter how many digests it carries. Not
// db/op -- the whole point of the batch is that "op" is the wrong denominator.
func BenchmarkExistsBatch_Cold(b *testing.B) {
	const keys = 1000

	repo := newFakeReader()
	for i := range keys {
		repo.add(sstateKey(i), digestOf(i), int64(i))
	}

	refs := make([]Ref, keys)
	for i := range keys {
		refs[i] = testRef(sstateKey(i))
	}

	// One entry per shard: every batch is fully cold and re-queries its residue.
	svc := newTestService(b, repo, lruShards)

	before := repo.queries.Load()

	b.ResetTimer()
	b.ReportAllocs()

	var batches atomic.Int64

	for range b.N {
		if _, err := svc.ExistsBatch(b.Context(), refs); err != nil {
			b.Fatalf("ExistsBatch() error = %v", err)
		}

		batches.Add(1)
	}

	b.StopTimer()
	b.ReportMetric(float64(repo.queries.Load()-before)/float64(batches.Load()), "db/batch")
}
