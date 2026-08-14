package gc

import (
	"time"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// QUOTAS (spec §8).
//
// Evict-to-quota plus an advisory badge. Hard-reject is a NON-GOAL and the reason
// is a client failure mode, not a preference: a PUT rejected for quota latches
// unverified in every one of these clients -- bitbake swallows an sstate upload
// failure, ccache treats a write error as a cache miss forever, Bazel's uploader
// gives up on the build. The enforcement point, if it is ever wanted, is
// blob.Service.put after w.Digest().
//
// ATTRIBUTION IS LOGICAL BYTES, FULL CHARGE TO EVERY NAMER, SCOPED PER BACKEND.
// A blob three backends name counts three times, once in each. That is
// order-independent and locally computable -- the same property that makes the
// refcount trigger correct -- and it is why the quota is a cap on what a backend is
// CHARGED for and not on what the instance stores. Evicting to quota frees logical
// bytes; disk falls only after the grace period and the reap, and under cross-project
// dedup possibly not at all. bakery_storage_physical_bytes is the number to alert on
// for disk, and it is deliberately backend-blind.

// quotaTargetRatio is where eviction stops: 90% of the cap, not 100%. Driving
// exactly to the cap makes the next write immediately over quota again, so every
// sweep evicts and the backend lives permanently at its ceiling.
const quotaTargetRatio = 0.9

// ageBuckets are the eviction histogram's lower bounds, OLDEST FIRST. A row falls in
// the first bucket whose bound its age meets.
//
// A histogram rather than a sort: the eviction cutoff has to come out of the SAME
// cursor pass that measures usage, and a backend can hold tens of millions of rows.
// Bucketing costs one comparison ladder per row and bounded memory; sorting the
// candidate set costs the candidate set. The price is that the cutoff lands on a
// bucket boundary, so a pass frees AT LEAST the target and may overshoot into the
// next boundary -- which is the safe direction for a cap.
var ageBuckets = []time.Duration{
	365 * 24 * time.Hour,
	180 * 24 * time.Hour,
	90 * 24 * time.Hour,
	60 * 24 * time.Hour,
	30 * 24 * time.Hour,
	14 * 24 * time.Hour,
	7 * 24 * time.Hour,
	3 * 24 * time.Hour,
	24 * time.Hour,
	12 * time.Hour,
	6 * time.Hour,
	time.Hour,
	0,
}

// stageUsage is one stage's share of a backend's usage plus its age histogram.
type stageUsage struct {
	objects int64
	bytes   int64

	// bucketBytes is parallel to ageBuckets.
	bucketBytes []int64
}

// usage is a backend's measured state after a sweep pass: what SURVIVED it.
type usage struct {
	objects int64
	bytes   int64

	// nullAccessed counts rows that have never been touched. It feeds the toucher's
	// T ramp (spec §6.4) -- see Engine.TouchStaleness.
	nullAccessed int64

	stages []stageUsage
}

func newUsage(stages int) *usage {
	u := &usage{objects: 0, bytes: 0, nullAccessed: 0, stages: make([]stageUsage, stages)}

	for i := range u.stages {
		u.stages[i] = stageUsage{objects: 0, bytes: 0, bucketBytes: make([]int64, len(ageBuckets))}
	}

	return u
}

// observe records one SURVIVING row against its stage.
//
// Rows that the pass deletes are not observed: the usage this produces is the
// backend's state AFTER the sweep, which is what the quota decision and the storage
// gauges both want. A count taken before the sweep would put a backend over quota on
// the strength of rows the same run had already removed.
func (u *usage) observe(stageIdx int, row repository.ScanObjectsForGCRow, now time.Time) {
	u.objects++
	u.bytes += row.SizeBytes

	if !row.AccessedAt.Valid {
		u.nullAccessed++
	}

	if stageIdx < 0 || stageIdx >= len(u.stages) {
		return
	}

	s := &u.stages[stageIdx]
	s.objects++
	s.bytes += row.SizeBytes
	s.bucketBytes[bucketOf(now.Sub(livenessOf(row)))] += row.SizeBytes
}

// livenessOf is THE liveness column, in Go, and it is the same rule the SQL carries
// verbatim: coalesce(accessed_at, created_at).
//
// A bare accessed_at would make every pre-upgrade row -- all NULL on day one --
// immortal, and would make quota eviction non-terminating: a backend over quota
// whose rows all read NULL would evict nothing, forever, while reporting itself over
// cap. created_at is used ONLY when there is no read record at all, which is
// genuinely cold. This is not the CAS trap CLAUDE.md warns about, because the rule
// still prefers the read timestamp whenever one exists -- and §6.3's reachability
// touch is what makes one exist for AC-hit CAS blobs.
func livenessOf(row repository.ScanObjectsForGCRow) time.Time {
	if row.AccessedAt.Valid {
		return row.AccessedAt.Time
	}

	return row.CreatedAt.Time
}

// bucketOf places an age in the histogram. A negative age (a clock skew, or an
// accessed_at written by a transaction whose now() ran after the run's started_at)
// lands in the youngest bucket, which is the conservative end.
func bucketOf(age time.Duration) int {
	for i, bound := range ageBuckets {
		if age >= bound {
			return i
		}
	}

	return len(ageBuckets) - 1
}

// evictionPlan is what the histogram yields: which stages go entirely, and where the
// cutoff falls in the one stage that goes partially.
//
// THE STAGED SHAPE IS THE POINT (spec §3): eviction exhausts a stage's candidates
// before touching its successor, and it is never a global LRU across the backend.
// For a bazel backend that means every ActionResult goes before the first CAS blob
// does -- losing an AC entry costs a cache miss, losing a CAS blob an AC entry still
// names costs Bazel a build abort and rewind.
type evictionPlan struct {
	// fullStages: stages [0, fullStages) are evicted in their entirety.
	fullStages int

	// partial is the index of the one stage evicted by age, or -1.
	partial int

	// minAge is the cutoff inside the partial stage: rows at least this old go.
	minAge time.Duration
}

// empty reports a plan that deletes nothing.
func (p evictionPlan) empty() bool { return p.fullStages == 0 && p.partial < 0 }

// doomed reports whether a row in stage stageIdx is evicted by this plan.
//
// The retention window is NOT consulted: a cap is a cap, and a backend over quota
// sheds its oldest data whether or not the window has elapsed.
func (p evictionPlan) doomed(stageIdx int, row repository.ScanObjectsForGCRow, now time.Time) bool {
	if stageIdx < p.fullStages {
		return true
	}

	if stageIdx != p.partial {
		return false
	}

	return now.Sub(livenessOf(row)) >= p.minAge
}

// planEviction turns a measured usage and a quota into a plan, or an empty plan when
// the backend is inside its cap.
func planEviction(u *usage, quota int64) evictionPlan {
	none := evictionPlan{fullStages: 0, partial: -1, minAge: 0}

	if quota <= 0 || u.bytes <= quota {
		return none
	}

	target := u.bytes - int64(float64(quota)*quotaTargetRatio)
	if target <= 0 {
		return none
	}

	plan := none

	for i := range u.stages {
		s := &u.stages[i]

		if s.bytes <= target {
			// The whole stage still does not free enough: take it entirely and move on to
			// the successor, which is the only order this is allowed to happen in.
			plan.fullStages = i + 1
			target -= s.bytes

			if target <= 0 {
				return plan
			}

			continue
		}

		var freed int64

		for b, bytes := range s.bucketBytes {
			freed += bytes

			if freed >= target {
				plan.partial = i
				plan.minAge = ageBuckets[b]

				return plan
			}
		}

		// Unreachable while the bucket totals sum to s.bytes, which they do by
		// construction; falling through would silently under-evict, so take the stage.
		plan.fullStages = i + 1
		target -= s.bytes
	}

	return plan
}

// snapshot is one backend's last published measurement, kept so a gauge reset does
// not make an unmeasured backend look like a deleted one.
type snapshot struct {
	objects int64
	bytes   int64
	at      time.Time
}
