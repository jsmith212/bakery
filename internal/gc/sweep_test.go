package gc

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// THE LADDER, END TO END, AGAINST A REAL DATABASE (spec §6.3, §4).
//
// An ActionResult names CAS digests. Under --remote_download_minimal Bazel's AC hit
// never reads them -- the skip-download decision is purely local and skipped outputs
// are injected from ActionResult metadata with ZERO CAS contact -- so "hot AC, cold
// CAS" is the NORMAL steady state, not an edge case. If the two shared a window, the
// sweep would delete exactly the blobs a live ActionResult names, and Bazel answers a
// missing input not with a clean miss but with a build abort and rewind.
func TestCASOutlivesItsActionCacheEntry(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(30), quota: 0, disabled: false})

	const casKey = "3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea"

	f.put(backend, nsAC, "action-one", "action result bytes")
	f.put(backend, nsCAS, casKey, "output blob bytes")

	// Both are 45 days cold and neither has ever been touched, which is every row's
	// state the day migration 000012 lands.
	f.age(backend, nsAC, "action-one", ago(45), ago(45))
	f.age(backend, nsCAS, casKey, ago(45), ago(45))

	f.run()

	if f.exists(backend, nsAC, "action-one") {
		t.Error("the ActionResult survived its own 30d window")
	}

	if !f.exists(backend, nsCAS, casKey) {
		t.Error("the CAS blob was swept at 45d: W_cas must be 2 x W_ac, or an AC hit " +
			"under --remote_download_minimal loses its outputs")
	}
}

// The /ac namespaces are the only OVERWRITABLE ones, so their rule is
// greatest(created_at, coalesce(accessed_at, created_at)): an overwrite refreshes
// created_at, and inheriting the age of the read record it replaced would age out an
// entry that was rewritten yesterday.
func TestOverwrittenActionCacheEntryIsNotSweptOnAStaleRead(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(30), quota: 0, disabled: false})

	f.put(backend, nsAC, "rewritten", "fresh action result")
	f.age(backend, nsAC, "rewritten", ago(1), ago(90))

	f.run()

	if !f.exists(backend, nsAC, "rewritten") {
		t.Error("an entry rewritten yesterday was swept on the strength of a 90-day-old read record")
	}
}

// A MULTI-ARCH CHILD MANIFEST IS NAMED BY NOBODY THE SWEEP CAN SEE.
//
// A tag names the INDEX; the index names its per-architecture children by digest,
// inside manifest bytes this backend deliberately never parses (a re-serialization
// breaks Docker-Content-Digest, and manifest parsing is a recorded non-goal). So the
// tag/manifest anti-join finds no tag for a child and the only thing standing between
// it and deletion is the ladder: W_manifests is 2 x W_tags. A child that is older than
// the tag window is exactly the row a single shared window would eat, leaving a
// pullable index whose children 404.
func TestMultiArchChildManifestSurvivesItsIndexWindow(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindOci, backendOpts{window: day(30), quota: 0, disabled: false})

	const (
		index    = "index-manifest-bytes"
		child    = "linux/amd64 child manifest bytes"
		orphan   = "a manifest nothing has named in a year"
		indexKey = "sha256:index"
		childKey = "sha256:child"
	)

	indexDigest := f.put(backend, nsManifests, indexKey, index)
	f.put(backend, nsManifests, childKey, child)
	f.put(backend, nsManifests, "sha256:orphan", orphan)

	// The tag and the manifest are TWO ROWS sharing ONE BLOB: the tag's content is the
	// index manifest's bytes, which is what makes the anti-join a digest comparison.
	f.put(backend, nsTags, "docker.io/library/alpine:latest", index)

	if got := f.digestOf(backend, nsTags, "docker.io/library/alpine:latest"); got != indexDigest {
		t.Fatalf("tag digest %x != index manifest digest %x -- the anti-join is comparing nothing", got, indexDigest)
	}

	// The tag was revalidated a day ago; every manifest is 45 days old -- past
	// W_tags (30d) and inside W_manifests (60d).
	f.age(backend, nsTags, "docker.io/library/alpine:latest", ago(45), ago(45))
	f.refreshed(backend, nsTags, "docker.io/library/alpine:latest", ago(1))
	f.age(backend, nsManifests, indexKey, ago(45), ago(45))
	f.age(backend, nsManifests, childKey, ago(45), ago(45))
	f.age(backend, nsManifests, "sha256:orphan", ago(400), ago(400))

	f.run()

	if !f.exists(backend, nsTags, "docker.io/library/alpine:latest") {
		t.Error("a tag revalidated yesterday was swept: stage 7 reads updated_at, not created_at")
	}

	if !f.exists(backend, nsManifests, indexKey) {
		t.Error("the index manifest was swept while a live tag names its digest")
	}

	if !f.exists(backend, nsManifests, childKey) {
		t.Error("the multi-arch child manifest was swept at 45d: no tag names it and none ever " +
			"will, so W_manifests = 2 x W_tags is the only thing keeping the image pullable")
	}

	if f.exists(backend, nsManifests, "sha256:orphan") {
		t.Error("a 400-day-old manifest no tag names survived")
	}
}

// downloads IS AN ARCHIVE, NOT A CACHE (spec §1.2). A premirror tarball whose
// upstream died is unrecoverable; every other namespace's eviction is not. Its window
// is NULL by default and stays NULL even under the opinionated defaults, so age never
// sweeps it -- and the sstate backend beside it proves the sweep really ran.
func TestDownloadsRetainsForeverByDefault(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	archive := f.backend(repository.BackendKindDownloads, backendOpts{window: 0, quota: 0, disabled: false})
	sstate := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})

	f.put(archive, nsDefault, "zlib-1.3.1.tar.gz", "premirror tarball")
	f.put(sstate, nsDefault, "ab/cd/sstate:zlib:::1.3.1:r0::14:abcdef_populate_lic.tar.zst", "sstate object")

	f.age(archive, nsDefault, "zlib-1.3.1.tar.gz", ago(400), ago(400))
	f.age(sstate, nsDefault, "ab/cd/sstate:zlib:::1.3.1:r0::14:abcdef_populate_lic.tar.zst", ago(400), ago(400))

	f.run()

	if !f.exists(archive, nsDefault, "zlib-1.3.1.tar.gz") {
		t.Error("a 400-day-old premirror tarball was swept: downloads retains forever by default")
	}

	if f.exists(sstate, nsDefault, "ab/cd/sstate:zlib:::1.3.1:r0::14:abcdef_populate_lic.tar.zst") {
		t.Error("the sstate object beside it survived, so this test proved nothing about downloads")
	}
}

// DISABLED IS A STRONGER RETENTION SIGNAL, NOT AN EXEMPTION (spec §3, finding 10).
// A disabled backend serves no traffic, so nothing ever touches its accessed_at;
// skipping it would let its rows pin deduped digests forever against every live
// backend that has long stopped naming them.
func TestDisabledBackendIsStillSwept(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(90), quota: 0, disabled: true})

	f.put(backend, nsAC, "old", "past the clamp")
	f.put(backend, nsAC, "recent", "inside the clamp")
	f.age(backend, nsAC, "old", ago(45), ago(45))
	f.age(backend, nsAC, "recent", ago(20), ago(20))

	f.run()

	// 45 days is inside the CONFIGURED 90-day window and outside the 30-day ceiling a
	// disabled backend is clamped to, so this row is the whole assertion.
	if f.exists(backend, nsAC, "old") {
		t.Error("a disabled backend kept a 45-day-old row: least(configured, 30d) was not applied")
	}

	if !f.exists(backend, nsAC, "recent") {
		t.Error("the clamp swept a 20-day-old row: the ceiling is 30 days, not zero")
	}
}

// THE COVERAGE GUARD (spec §5, finding 12).
//
// Zero unihash coverage means one of two opposite things: a deployment with no
// hashserv data at all (an rsync'd mirror, BB_HASHSERVE=auto), where collapsing to
// age-only retention is CORRECT -- or a broken derivation, where age-only retention
// deletes a live cache. They are indistinguishable from coverage alone, so the guard
// also asks whether the paired hashserv holds any rows.
func TestSstateZeroCoverageRefusesToSweep(t *testing.T) {
	t.Parallel()

	const (
		liveUnihash = "9f3c8a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7"
		deadUnihash = "2b7e151628aed2a6abf7158809cf4f3c762e7160"
	)

	keyFor := func(unihash string) string {
		return fmt.Sprintf("universal/%s/%s/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:%s_populate_sysroot.tar.zst",
			unihash[0:2], unihash[2:4], unihash)
	}

	tests := []struct {
		name string

		// hashservRows seeds the paired backend with a unihash NOTHING in the sstate
		// corpus names, which is what a broken derivation looks like from outside.
		hashservRows bool
		// reachable seeds the unihash one of the two objects actually derives.
		reachable bool

		wantRefused int
		wantSurvive []string
	}{
		{
			name: "no hashserv data sweeps on age alone", hashservRows: false, reachable: false,
			wantRefused: 0, wantSurvive: nil,
		},
		{
			name:         "hashserv rows but nothing resolves refuses the backend",
			hashservRows: true, reachable: false,
			wantRefused: 1, wantSurvive: []string{keyFor(deadUnihash), keyFor(liveUnihash)},
		},
		{
			name:         "one resolving key is enough to proceed, and it is spared",
			hashservRows: true, reachable: true,
			wantRefused: 0, wantSurvive: []string{keyFor(liveUnihash)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, testConfig())
			hashserv := f.backend(repository.BackendKindHashserv,
				backendOpts{window: day(90), quota: 0, disabled: false})
			sstate := f.backend(repository.BackendKindSstate,
				backendOpts{window: day(90), quota: 0, disabled: false})

			switch {
			case tc.reachable:
				f.unihash(hashserv, "taskhash-live", liveUnihash, ago(1))
			case tc.hashservRows:
				f.unihash(hashserv, "taskhash-unrelated", "00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff", ago(1))
			}

			for _, u := range []string{liveUnihash, deadUnihash} {
				f.put(sstate, nsDefault, keyFor(u), "sstate object "+u)
				f.age(sstate, nsDefault, keyFor(u), ago(400), ago(400))
			}

			sum := f.run()

			if sum.BackendsRefused != tc.wantRefused {
				t.Errorf("BackendsRefused = %d, want %d", sum.BackendsRefused, tc.wantRefused)
			}

			got := f.keys(sstate, nsDefault)
			want := slices.Clone(tc.wantSurvive)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("surviving keys = %v, want %v", got, want)
			}
		})
	}
}

// THE PENDING READ SURVIVES THE SWEEP (spec §6.2). A pending, unflushed touch is
// invisible to the scan's SELECT -- the row still carries its old accessed_at -- so
// a key this process answered "present" for seconds ago is selected as ninety days
// cold. FindMissingBlobs answering "present" is a RESERVATION the client acts on.
//
// TWO MECHANISMS NOW STAND BETWEEN THAT MARK AND THE DELETE, and this test asserts
// the OUTCOME rather than which one produced it: the pre-sweep force-flush (§6.2
// mechanism 1, added by A: R6#1/R7#3) turns a mark taken before the sweep into a
// real accessed_at, and the per-chunk veto catches the ones taken during it. The
// second is the only one that can cover a read that lands mid-sweep, which is why
// both are here.
func TestSweepVetoesAKeyWithAnUnflushedRead(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(30), quota: 0, disabled: false})

	f.put(backend, nsAC, "read-just-now", "an action result")
	f.put(backend, nsAC, "genuinely-cold", "another action result")
	f.age(backend, nsAC, "read-just-now", ago(400), ago(400))
	f.age(backend, nsAC, "genuinely-cold", ago(400), ago(400))

	// A real read through the real service. It marks the LRU entry and nothing has
	// flushed it, so accessed_at in the database is still 400 days old.
	if ok, err := f.blobs.Exists(t.Context(), f.ref(backend, nsAC, "ac", "read-just-now")); err != nil || !ok {
		t.Fatalf("Exists() = %v, %v; want true, nil", ok, err)
	}

	if !f.blobs.PendingTouch(backend, nsAC, "read-just-now") {
		t.Fatal("the read left no pending touch, so this test cannot prove the veto")
	}

	f.run()

	if !f.exists(backend, nsAC, "read-just-now") {
		t.Error("a key with an unflushed read was swept: the sweep's SELECT cannot see the touch, " +
			"and only this process's memory can")
	}

	if f.exists(backend, nsAC, "genuinely-cold") {
		t.Error("the cold key survived, so the veto is not what spared the other one")
	}
}

// THE PRE-SWEEP FLUSH IS WHAT KEEPS A LIVE BUILD'S GC ROOT ALIVE (A: R6#1/R7#3,
// spec §6.2 mechanism 1).
//
// hashserv marks a unihash read in memory and writes accessed_at at most once per
// T. Stage 1 is a single self-contained SQL DELETE reading coalesce(accessed_at,
// created_at): there is no Go-side pass over its candidates, so the per-chunk
// PendingTouch veto that protects the cache_objects stages has no equivalent here.
// Until the mark is forced onto disk, a unihash a build read minutes ago is
// selected as ninety days cold -- and that is not a cache miss, it is the GC root
// disappearing from under every sstate object whose filename embeds it.
func TestSweepUnihashesVetoesAnUnflushedRead(t *testing.T) {
	t.Parallel()

	const (
		read      = "taskhash-read-just-now"
		untouched = "taskhash-genuinely-cold"
	)

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindHashserv, backendOpts{window: day(30), quota: 0, disabled: false})

	f.unihash(backend, read, "9f3c8a1b2c3d4e5f60718293a4b5c6d7e8f90a1b", ago(400))
	f.unihash(backend, untouched, "2b7e151628aed2a6abf7158809cf4f3c762e7160", ago(400))

	// The read a build just took. It is pending in this process's memory and NOT in
	// the database: accessed_at is still NULL, so the sweep's own SELECT sees a row
	// that has not been read in four hundred days.
	f.touch.mark(backend, "oe.sstatesig.OEOuthashBasic", read)

	if at := f.unihashAccessedAt(backend, read); at.Valid {
		t.Fatal("the mark reached the database before the sweep ran, so this test proves nothing")
	}

	f.run()

	if !f.unihashExists(backend, read) {
		t.Error("a unihash with an unflushed read was swept: the sweep must force the toucher's " +
			"pending marks to disk BEFORE stage 1, not after it and not never")
	}

	if f.unihashExists(backend, untouched) {
		t.Error("the genuinely cold unihash survived, so the flush is not what spared the other one")
	}
}

// A MANIFEST A LIVE TAG NAMES SURVIVES ITS OWN WINDOW, AND ITS TWIN DIES (E:
// R6#4/R7#9, R6#5 -- the protective direction).
//
// The anti-join is evaluated in SQL, in the same statement as the DELETE, against
// the tags namespace AS IT STANDS THEN. The Go-side tagDigests set this replaced
// answered a different question -- "was this digest tagged when the tags stage
// scanned past" -- and a tag written or revalidated in the gap (the ordinary case:
// SWR revalidation writes tags continuously) left a live tag pointing at a manifest
// the sweep had already condemned.
func TestTaggedManifestSurvivesAnyAgeAndItsUntaggedTwinDies(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindOci, backendOpts{window: day(30), quota: 0, disabled: false})

	const (
		tagged   = "sha256:tagged"
		untagged = "sha256:untagged"
		bytes    = "the very same manifest bytes"
	)

	f.put(backend, nsManifests, tagged, bytes)
	f.put(backend, nsManifests, untagged, "different manifest bytes")

	// One tag, revalidated an hour ago, naming the first manifest's digest.
	f.put(backend, nsTags, "docker.io/library/alpine:latest", bytes)
	f.refreshed(backend, nsTags, "docker.io/library/alpine:latest", time.Now().Add(-time.Hour))

	// Both manifests are 400 days old -- far past W_manifests (60d), so the ladder
	// cannot be what spares either of them.
	f.age(backend, nsManifests, tagged, ago(400), ago(400))
	f.age(backend, nsManifests, untagged, ago(400), ago(400))

	f.run()

	if !f.exists(backend, nsManifests, tagged) {
		t.Error("a 400-day-old manifest a live tag names was swept: the anti-join is not being " +
			"evaluated at delete time")
	}

	if f.exists(backend, nsManifests, untagged) {
		t.Error("a 400-day-old manifest no tag names survived, so the tagged one's survival " +
			"proves nothing about the anti-join")
	}
}

// QUOTA EVICTION RESPECTS REACHABILITY (R6#9). A cap is a cap, so the retention
// WINDOW is ignored -- but the stage's own reachability rule is not: evicting an
// sstate object whose unihash is still alive is not shedding cold data, it is
// deleting an object a running build is about to ask for by name.
//
// Both objects here are two days old and both are far inside the 90-day window, so
// retention cannot be what deletes either of them; only the cap can. One has a live
// unihash on the paired hashserv backend and the other does not, and that is the
// whole difference between them.
func TestQuotaEvictionSparesAReachableSstateObject(t *testing.T) {
	t.Parallel()

	const (
		liveUnihash = "9f3c8a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"
		deadUnihash = "2b7e151628aed2a6abf7158809cf4f3c762e7160"
	)

	keyFor := func(unihash string) string {
		return fmt.Sprintf(
			"universal/%s/%s/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:%s_populate_sysroot.tar.zst",
			unihash[0:2], unihash[2:4], unihash)
	}

	f := newFixture(t, testConfig())
	hashserv := f.backend(repository.BackendKindHashserv,
		backendOpts{window: day(90), quota: 0, disabled: false})
	sstate := f.backend(repository.BackendKindSstate,
		backendOpts{window: day(90), quota: 8, disabled: false})

	f.unihash(hashserv, "taskhash-live", liveUnihash, ago(1))

	for _, u := range []string{liveUnihash, deadUnihash} {
		f.put(sstate, nsDefault, keyFor(u), "sstate object "+u)
		f.age(sstate, nsDefault, keyFor(u), ago(2), ago(2))
	}

	f.run()

	if f.exists(sstate, nsDefault, keyFor(deadUnihash)) {
		t.Error("the over-quota backend evicted nothing: a cap ignores the retention window, " +
			"and both objects are well inside it")
	}

	if !f.exists(sstate, nsDefault, keyFor(liveUnihash)) {
		t.Error("quota eviction took an object whose unihash is still alive: a cap ignores the " +
			"WINDOW, never the stage's reachability rule")
	}
}

// QUOTA EVICTION OF MANIFESTS GOES THROUGH THE ANTI-JOIN QUERY (R6#9), which is the
// only thing that deletes a manifest at all now that the Go-side decider is
// accounting-only. Forget to wire the quota path to it and manifests become
// silently immune to the cap -- an OCI backend that reports itself over quota
// forever while evicting nothing.
//
// A NOTE ON WHAT THIS CANNOT SHOW, because it is a property of the staged order
// rather than an omission: eviction exhausts a stage before touching its successor,
// and tags are stage 7 to manifests' stage 8. A backend under enough pressure to
// reach the manifests stage has therefore already shed EVERY tag, so within one
// quota pass "tagged" is not a state a manifest can still be in. The anti-join in
// the quota path is defence for the rows that survive stage 7 anyway -- a tag
// vetoed by a pending read, or one the write barrier spares -- and the protective
// direction is proven against the retention path in
// TestTaggedManifestSurvivesAnyAgeAndItsUntaggedTwinDies.
func TestQuotaEvictionReachesManifests(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindOci, backendOpts{window: day(30), quota: 8, disabled: false})

	// (The API refuses to SET an OCI quota -- a pull-through proxy is bounded by its
	// retention window, spec §1.3 -- but the COLUMN allows one and the CHECK does
	// not forbid it, so the engine has to evict correctly for any row that carries
	// one.)
	const doomed = "sha256:untagged"

	f.put(backend, nsManifests, doomed, "an untagged manifest nothing points at")
	f.age(backend, nsManifests, doomed, ago(2), ago(2))

	f.run()

	if f.exists(backend, nsManifests, doomed) {
		t.Error("an over-quota OCI backend evicted no manifest: quota eviction must drive the " +
			"same SweepUnreferencedManifests statement retention does, with the histogram " +
			"cutoff standing in for the window")
	}
}

// A REFUSED sstate BACKEND MUST NOT VANISH FROM THE STORAGE GAUGES (P: R6#8).
//
// The sweep RESETS every per-backend gauge before republishing them, because a vec
// is a map and a deleted backend would otherwise export its last value forever.
// A backend the coverage guard refuses is measured by neither the reset's
// republish (it looks like a backend about to be measured) nor the sweep itself --
// so the one backend whose retention has visibly stopped working is also the one
// whose size disappears from the dashboard.
func TestSstateZeroCoverageKeepsItsStorageGauges(t *testing.T) {
	t.Parallel()

	const key = "universal/9f/3c/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:" +
		"9f3c8a1b2c3d4e5f60718293a4b5c6d7e8f90a1b_populate_sysroot.tar.zst"

	f := newFixture(t, testConfig())
	hashserv := f.backend(repository.BackendKindHashserv,
		backendOpts{window: day(90), quota: 0, disabled: false})
	sstate := f.backend(repository.BackendKindSstate,
		backendOpts{window: day(90), quota: 0, disabled: false})

	// A unihash nothing in the sstate corpus names: the shape of a broken derivation.
	f.unihash(hashserv, "taskhash-unrelated", "00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff", ago(1))

	f.put(sstate, nsDefault, key, "an sstate object")
	f.age(sstate, nsDefault, key, ago(400), ago(400))

	// The lightweight usage pass measures every backend, refused or not -- this is
	// the measurement the sweep's reset is about to drop.
	if err := f.eng.MeasureUsage(t.Context()); err != nil {
		t.Fatalf("MeasureUsage() error = %v", err)
	}

	if got := f.storageObjects(metrics.BackendSstate); got != 1 {
		t.Fatalf("bakery_storage_objects after the usage pass = %v, want 1", got)
	}

	sum := f.run()

	if sum.BackendsRefused != 1 {
		t.Fatalf("BackendsRefused = %d, want 1 -- this test needs the guard to actually refuse",
			sum.BackendsRefused)
	}

	if got := f.storageObjects(metrics.BackendSstate); got != 1 {
		t.Errorf("bakery_storage_objects after a refused sweep = %v, want 1: the refusal reset "+
			"the gauge and never republished it", got)
	}
}

// A DRY RUN WRITES NOTHING ANYWHERE, and that is what makes it the upgrade-notes
// rail for an opinionated retention default: the operator can see the first sweep's
// blast radius before it happens.
func TestDryRunDeletesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(30), quota: 0, disabled: false})

	f.put(backend, nsAC, "doomed", "an action result")
	f.age(backend, nsAC, "doomed", ago(400), ago(400))

	sum, err := f.eng.Run(t.Context(), TriggerAPI, true)
	if err != nil {
		t.Fatalf("dry Run() error = %v", err)
	}

	if sum.ObjectsDeleted != 1 {
		t.Errorf("dry run reported %d doomed objects, want 1", sum.ObjectsDeleted)
	}

	if !f.exists(backend, nsAC, "doomed") {
		t.Fatal("a DRY run deleted a row")
	}

	if sum.BlobsMarked != 0 || sum.BlobsDeleted != 0 {
		t.Errorf("dry run marked %d and reaped %d blobs, want 0 and 0", sum.BlobsMarked, sum.BlobsDeleted)
	}

	// It is auditable and it does NOT hold the active slot, so a second one may
	// overlap it freely.
	if _, err := f.eng.Run(t.Context(), TriggerAPI, true); err != nil {
		t.Fatalf("second dry Run() error = %v", err)
	}
}

// --gc-disable-retention HALTS LAYER B's MARK AS WELL AS EVERY RETENTION STAGE
// (spec §9.6). The incident it serves is "we deleted things we wanted", and those
// bytes are still sitting inside the grace window: leaving the mark running would
// convert a recoverable window into permanent loss at maximum speed.
//
// Stage 0 keeps running, because a blob ALREADY in pending_delete is past recovery
// and stalling it reclaims nothing.
func TestDisableRetentionHaltsTheMark(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.DisableRetention = true

	f := newFixture(t, cfg)
	backend := f.backend(repository.BackendKindBazel, backendOpts{window: day(30), quota: 0, disabled: false})

	digest := f.put(backend, nsAC, "doomed", "an action result")
	f.age(backend, nsAC, "doomed", ago(400), ago(400))

	// A separate object whose metadata is already gone: its blob is unreferenced and
	// is exactly what the mark would take.
	orphan := f.put(backend, nsAC, "already-deleted", "orphaned bytes")

	if _, err := f.blobs.Delete(t.Context(), f.ref(backend, nsAC, "ac", "already-deleted")); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	sum := f.run()

	if sum.ObjectsDeleted != 0 {
		t.Errorf("retention deleted %d objects while disabled", sum.ObjectsDeleted)
	}

	if sum.BlobsMarked != 0 {
		t.Errorf("Layer B marked %d blobs while retention is disabled -- the brake must stop the "+
			"mark, or the recovery window it exists to protect is gone", sum.BlobsMarked)
	}

	if state := f.blobState(orphan); state != "live" {
		t.Errorf("orphan blob state = %q, want live", state)
	}

	if !f.exists(backend, nsAC, "doomed") {
		t.Error("a 400-day-old row was swept while retention is disabled")
	}

	_ = digest

	// Stage 0 still re-drives what a previous process already committed to deleting.
	f.markPendingDelete(orphan)

	sum = f.run()

	if sum.BlobsDeleted != 1 {
		t.Errorf("stage 0 reaped %d blobs, want 1: an already-marked blob is past recovery and "+
			"stalling it reclaims nothing", sum.BlobsDeleted)
	}
}

// THE N1 REGRESSION (verify pass, 2026-08-14): a managed()-but-WINDOWLESS OCI backend
// -- quota set, retention_window NULL -- must not retention-sweep its manifests. The
// fix wave moved stage 8's deletion into SweepUnreferencedManifests and lost the
// hasWindow guard on the way: the stage window is 0 without a configured window, 0
// reaches SQL as `< now()`, and every untagged manifest dies on a backend whose
// operator asked for a cap, not a clock. NULL means retain forever, here as everywhere.
func TestWindowlessQuotaBackendRetainsItsManifests(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())
	// Quota far above usage: managed() is true (the quota alone manages it), the
	// backend is nowhere near eviction, and there is no window. Nothing may delete.
	backend := f.backend(repository.BackendKindOci, backendOpts{window: 0, quota: 1 << 30, disabled: false})

	const survivor = "sha256:untagged-but-unwindowed"

	f.put(backend, nsManifests, survivor, "an untagged manifest on a windowless backend")
	f.age(backend, nsManifests, survivor, ago(400), ago(400))

	sum := f.run()

	if !f.exists(backend, nsManifests, survivor) {
		t.Fatal("a windowless (retention_window IS NULL) quota-carrying OCI backend " +
			"retention-swept an untagged manifest: window 0 reached SQL as `< now()` -- " +
			"NULL must mean retain forever")
	}

	if sum.ObjectsDeleted != 0 {
		t.Errorf("sweep deleted %d objects on a backend with no window and headroom under "+
			"its quota; want 0", sum.ObjectsDeleted)
	}
}
