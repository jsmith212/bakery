package gc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// QUOTA EVICTION IS STAGED, NOT A GLOBAL LRU (spec §3, §8).
//
// The backend below is over cap with YOUNG ActionResults and OLD CAS blobs, which is
// the ordinary steady state of a busy Bazel cache: an /ac entry is rewritten on every
// build, a deduped CAS blob is uploaded once and then never again because
// FindMissingBlobs means nobody re-uploads it. A pure oldest-first eviction would
// therefore shed exactly the CAS blobs the live ActionResults name -- and Bazel
// answers a missing input with a build abort and rewind, not a clean miss. Exhausting
// the ac family first costs a cache miss instead.
//
// There is no retention window here at all: this is the cap acting alone.
func TestQuotaEvictionExhaustsACBeforeCAS(t *testing.T) {
	t.Parallel()

	const (
		objects = 4
		size    = 1000
		quota   = 5000
	)

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel,
		backendOpts{window: 0, quota: quota, disabled: false})

	body := strings.Repeat("x", size)

	for i := range objects {
		ac := fmt.Sprintf("action-%02d", i)
		cas := fmt.Sprintf("%064x", i)

		// Distinct bodies: identical content would dedup onto one blob and the LOGICAL
		// accounting under test would be measuring something else.
		f.put(backend, nsAC, ac, body+ac)
		f.put(backend, nsCAS, cas, body+cas)

		// The ac entries are DAYS old and the cas blobs are MONTHS old, so oldest-first
		// across the backend would take the cas blobs and leave the ac entries.
		f.age(backend, nsAC, ac, ago(2), ago(2))
		f.age(backend, nsCAS, cas, ago(90), ago(90))
	}

	f.run()

	if got := f.keys(backend, nsAC); len(got) != 0 {
		t.Errorf("ac survivors = %v, want none: the ac stage is exhausted before cas is touched", got)
	}

	cas := f.keys(backend, nsCAS)
	if len(cas) != objects {
		t.Errorf("cas survivors = %d, want %d -- eviction reached the successor stage while the "+
			"predecessor still had candidates", len(cas), objects)
	}
}

// USAGE IS LOGICAL AND EVERY NAMER PAYS IN FULL (spec §8).
//
// One blob named by two backends counts once on disk and twice in the quota
// accounting. That is deliberate and it is the same property the refcount trigger
// has: order-independent and locally computable, so a backend's charge does not
// depend on which OTHER backend happened to upload the bytes first. It is also why
// quota is documented as a cap on what a backend is CHARGED for, and why
// bakery_storage_physical_bytes exists separately as the number to alert on for disk.
func TestUsageIsLogicalNotPhysicalUnderDedup(t *testing.T) {
	t.Parallel()

	const shared = "the very same bytes, uploaded by two projects"

	f := newFixture(t, testConfig())
	sstate := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})
	archive := f.backend(repository.BackendKindDownloads, backendOpts{window: 0, quota: 0, disabled: false})

	first := f.put(sstate, nsDefault, "ab/cd/sstate:zlib:::1.3.1:r0::14:abcdef_populate_lic.tar.zst", shared)
	second := f.put(archive, nsDefault, "zlib-1.3.1.tar.gz", shared)

	if first != second {
		t.Fatalf("the two puts produced different digests (%x, %x): dedup did not happen and this "+
			"test is measuring nothing", first, second)
	}

	if err := f.eng.MeasureUsage(t.Context()); err != nil {
		t.Fatalf("MeasureUsage() error = %v", err)
	}

	want := int64(len(shared))

	for _, backend := range []int64{sstate, archive} {
		objects, bytes := f.usageRow(backend)
		if objects != 1 || bytes != want {
			t.Errorf("backend %d usage = %d objects / %d bytes, want 1 / %d -- every namer is "+
				"charged in full", backend, objects, bytes, want)
		}
	}

	physical, err := f.store.InstancePhysicalBytes(t.Context())
	if err != nil {
		t.Fatalf("InstancePhysicalBytes() error = %v", err)
	}

	if physical != want {
		t.Errorf("physical bytes = %d, want %d: the instance stores ONE copy however many "+
			"backends name it", physical, want)
	}
}

// The usage pass runs even with retention disabled, and it does not re-measure a
// backend a sweep has just measured (spec §8, findings 7b/13).
func TestUsagePassRunsWithRetentionDisabled(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.DisableRetention = true

	f := newFixture(t, cfg)
	backend := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})

	f.put(backend, nsDefault, "ab/cd/object", "bytes")

	if err := f.eng.MeasureUsage(t.Context()); err != nil {
		t.Fatalf("MeasureUsage() error = %v", err)
	}

	if objects, _ := f.usageRow(backend); objects != 1 {
		t.Errorf("usage objects = %d, want 1: a dashboard that goes stale exactly when somebody "+
			"reaches for the brake is a dashboard that lies during the incident", objects)
	}
}

// planEviction's arithmetic, without a database in the way.
func TestPlanEvictionDrivesToNinetyPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stageBytes []int64
		ages       []int // one age (days) per stage; every row in a stage shares it
		quota      int64
		wantFull   int
		wantPart   int
	}{
		{
			name: "inside the cap plans nothing", stageBytes: []int64{100, 100}, ages: []int{1, 1},
			quota: 1000, wantFull: 0, wantPart: -1,
		},
		{
			name:       "a little over takes the first stage partially",
			stageBytes: []int64{1000, 1000}, ages: []int{1, 400},
			quota: 1900, wantFull: 0, wantPart: 0,
		},
		{
			name:       "far over exhausts the first stage before the second",
			stageBytes: []int64{1000, 1000}, ages: []int{1, 400},
			quota: 500, wantFull: 1, wantPart: 1,
		},
		{
			name: "no quota plans nothing", stageBytes: []int64{5000}, ages: []int{400},
			quota: 0, wantFull: 0, wantPart: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u := newUsage(len(tc.stageBytes))

			for i, bytes := range tc.stageBytes {
				u.bytes += bytes
				u.objects++
				u.stages[i].bytes = bytes
				u.stages[i].objects = 1
				u.stages[i].bucketBytes[bucketOf(day(tc.ages[i]))] = bytes
			}

			plan := planEviction(u, tc.quota)
			if plan.fullStages != tc.wantFull || plan.partial != tc.wantPart {
				t.Errorf("planEviction() = full %d, partial %d; want full %d, partial %d",
					plan.fullStages, plan.partial, tc.wantFull, tc.wantPart)
			}
		})
	}
}

// bucketOf must place every age somewhere, including the negative one a clock skew
// produces -- a row that fell through the ladder would be invisible to the histogram
// and its bytes would never be offered for eviction, which is how a quota stops
// terminating.
func TestBucketOfCoversEveryAge(t *testing.T) {
	t.Parallel()

	ages := []int{-5, 0, 1, 2, 5, 20, 45, 100, 200, 500}
	seen := map[int]struct{}{}

	for _, a := range ages {
		i := bucketOf(day(a))
		if i < 0 || i >= len(ageBuckets) {
			t.Fatalf("bucketOf(%dd) = %d, out of range", a, i)
		}

		seen[i] = struct{}{}
	}

	if len(seen) < 2 {
		t.Errorf("every age landed in %d bucket(s): the histogram is not discriminating",
			len(seen))
	}

	// OLDEST FIRST is the invariant the eviction walk depends on: it accumulates bytes
	// from bucket 0 upward and stops at the first boundary that frees enough, so a
	// ladder that was not descending would evict the YOUNGEST rows first.
	for i := 1; i < len(ageBuckets); i++ {
		if ageBuckets[i] >= ageBuckets[i-1] {
			t.Fatalf("ageBuckets[%d] = %v is not older than [%d] = %v", i-1, ageBuckets[i-1], i, ageBuckets[i])
		}
	}
}
