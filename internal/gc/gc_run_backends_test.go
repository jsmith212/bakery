package gc

import (
	"testing"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestGCRunBackendsWrittenOnlyForSweptBackends is the B7 engine test
// (docs/design/specs/2026-08-15-spa-api-wiring.md, migration 000013): a REAL run
// writes exactly one gc_run_backends row per backend it actually swept -- ZEROS
// INCLUDED, so "swept, nothing eligible" is a row and "not swept this run" is no
// row at all. It covers three backends in one run, each landing in a different
// bucket:
//
//   - sstate, managed (has a window) but nothing eligible to delete: a row with
//     0/0.
//   - hashserv, managed and one unihash aged past its window: a row with a
//     nonzero objects_deleted and bytes_freed = 0 (hashserv owns no bytes).
//   - downloads, unmanaged (no window, no quota -- the shipped default for the
//     kind): NO row at all, because sweepBackend's managed() guard returns
//     before this backend's own summary point is ever reached.
func TestGCRunBackendsWrittenOnlyForSweptBackends(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())

	sstateBackend := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})
	hashservBackend := f.backend(repository.BackendKindHashserv,
		backendOpts{window: day(90), quota: 0, disabled: false})
	downloadsBackend := f.backend(repository.BackendKindDownloads, backendOpts{window: 0, quota: 0, disabled: false})

	// One unihash, well past its 90-day window, so sweepHashserv has something real
	// to delete.
	f.unihash(hashservBackend, "task-one", "unihash-one", ago(120))

	sum := f.run()

	// sstate: SWEPT (it is managed), nothing eligible -- a row, at 0/0.
	objects, bytes, ok := f.runBackendRow(sum.RunID, sstateBackend)
	if !ok {
		t.Fatal("sstate backend has no gc_run_backends row; a managed backend the sweep " +
			"visited must always get one, even at zero")
	}

	if objects != 0 || bytes != 0 {
		t.Errorf("sstate row = objects:%d bytes:%d, want 0/0 (nothing was eligible)", objects, bytes)
	}

	// hashserv: SWEPT, one unihash deleted, no bytes (hashserv owns none).
	objects, bytes, ok = f.runBackendRow(sum.RunID, hashservBackend)
	if !ok {
		t.Fatal("hashserv backend has no gc_run_backends row")
	}

	if objects != 1 {
		t.Errorf("hashserv row objects_deleted = %d, want 1 (the aged-out unihash)", objects)
	}

	if bytes != 0 {
		t.Errorf("hashserv row bytes_freed = %d, want 0: hashserv owns no bytes to free", bytes)
	}

	// downloads: an ARCHIVE with no window and no quota under the shipped
	// defaults -- managed() is false, sweepBackend never reaches its own summary
	// point, and NO row is written.
	if _, _, ok := f.runBackendRow(sum.RunID, downloadsBackend); ok {
		t.Error("an unmanaged (no window, no quota) downloads backend got a gc_run_backends row; " +
			"it was never swept and must be indistinguishable from a backend absent this run")
	}
}

// TestGCRunBackendsCountsDeletedObjectBytes pins that bytes_freed is the LOGICAL
// size of what this run actually deleted from cache_objects, not the survivors'
// size (that is cache_backend_usage's job) and not a physical Layer-B reclaim
// figure.
func TestGCRunBackendsCountsDeletedObjectBytes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())

	backend := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})

	const content = "sstate object bytes, twenty" // 28 bytes

	f.put(backend, nsDefault, "cold-key", content)
	f.age(backend, nsDefault, "cold-key", ago(120), ago(120))

	// A survivor too, so the assertion is genuinely about what was DELETED, not
	// merely "everything the backend ever held".
	f.put(backend, nsDefault, "hot-key", "recently written, survives")
	f.age(backend, nsDefault, "hot-key", ago(1), ago(1))

	sum := f.run()

	if f.exists(backend, nsDefault, "cold-key") {
		t.Fatal("the cold key was not swept; fix the fixture before trusting the byte count")
	}

	objects, bytes, ok := f.runBackendRow(sum.RunID, backend)
	if !ok {
		t.Fatal("sstate backend has no gc_run_backends row")
	}

	if objects != 1 {
		t.Errorf("objects_deleted = %d, want 1 (only cold-key)", objects)
	}

	if bytes != int64(len(content)) {
		t.Errorf("bytes_freed = %d, want %d (len(%q))", bytes, len(content), content)
	}
}

// TestGCRunBackendsSkipsDryRunsAndUsageRuns is B7's other half: the table is
// written for REAL runs only. A dry run deletes nothing and a usage-only run
// (MeasureUsage) never calls sweepBackend/sweepHashserv at all -- see
// publishRunBackend's own doc comment.
func TestGCRunBackendsSkipsDryRunsAndUsageRuns(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())

	backend := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})

	f.put(backend, nsDefault, "cold-key", "will be reported doomed, never deleted")
	f.age(backend, nsDefault, "cold-key", ago(120), ago(120))

	dry, err := f.eng.Run(f.t.Context(), TriggerInterval, true)
	if err != nil {
		f.t.Fatalf("dry Run() error = %v", err)
	}

	if dry.ObjectsDeleted == 0 {
		f.t.Fatal("the dry run reported nothing doomed; fix the fixture before trusting the negative below")
	}

	if _, _, ok := f.runBackendRow(dry.RunID, backend); ok {
		t.Error("a DRY RUN wrote a gc_run_backends row; dry runs delete nothing and must not " +
			"be reported as if they had")
	}

	if !f.exists(backend, nsDefault, "cold-key") {
		f.t.Fatal("the dry run deleted the object; it must not have")
	}

	if err := f.eng.MeasureUsage(f.t.Context()); err != nil {
		f.t.Fatalf("MeasureUsage() error = %v", err)
	}

	usageRunID := f.latestGCRunID(TriggerUsage)

	if _, _, ok := f.runBackendRow(usageRunID, backend); ok {
		t.Error("a USAGE-ONLY run wrote a gc_run_backends row; MeasureUsage never sweeps a " +
			"backend at all and must never be reported as if it had")
	}
}

// TestGCRunBackendsSkipsARefusedSstateBackend is the sstate coverage guard's own
// case: sstateRefused returns before sweepBackend's own summary point, so a
// refused backend gets no row -- the same "declined, not swept" shape as an
// unmanaged backend, for a different reason (a broken root derivation rather
// than no configuration at all).
func TestGCRunBackendsSkipsARefusedSstateBackend(t *testing.T) {
	t.Parallel()

	f := newFixture(t, testConfig())

	hashservBackend := f.backend(repository.BackendKindHashserv,
		backendOpts{window: day(90), quota: 0, disabled: false})
	sstateBackend := f.backend(repository.BackendKindSstate, backendOpts{window: day(90), quota: 0, disabled: false})

	// The paired hashserv backend HOLDS ROWS, but no sstate key in this backend
	// will ever resolve to one of them (the keys are not sstate-shaped hashes at
	// all) -- exactly sstateRefused's trigger condition.
	f.unihash(hashservBackend, "task-one", "unihash-one", ago(1))
	f.put(sstateBackend, nsDefault, "not-an-sstate-filename", "orphaned-looking content")
	f.age(sstateBackend, nsDefault, "not-an-sstate-filename", ago(120), ago(120))

	sum := f.run()

	if sum.BackendsRefused == 0 {
		f.t.Fatal("the sstate backend was not refused; fix the fixture before trusting the negative below")
	}

	if _, _, ok := f.runBackendRow(sum.RunID, sstateBackend); ok {
		t.Error("a REFUSED sstate backend got a gc_run_backends row; the coverage guard " +
			"declined it before sweepBackend's own summary point, and it must read as " +
			"'not swept', not 'swept, nothing eligible'")
	}
}
