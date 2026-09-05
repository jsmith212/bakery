package gc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// markDeleting stamps cache_backends.deleting_at, which is the whole of what
// DELETE /backends/{kind} does to a backend that still holds objects.
func (f *fixture) markDeleting(backendID int64) {
	f.t.Helper()

	tag, err := f.pool.Exec(f.t.Context(),
		`UPDATE cache_backends SET deleting_at = now(), enabled = false WHERE id = $1`, backendID)
	if err != nil {
		f.t.Fatalf("mark backend %d deleting: %v", backendID, err)
	}

	if tag.RowsAffected() != 1 {
		f.t.Fatalf("mark backend %d deleting touched %d rows, want 1", backendID, tag.RowsAffected())
	}
}

// markProjectDeleting is the project half of the same mark.
func (f *fixture) markProjectDeleting() {
	f.t.Helper()

	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE projects SET deleting_at = now() WHERE id = $1`, f.projectID); err != nil {
		f.t.Fatalf("mark project deleting: %v", err)
	}
}

func (f *fixture) backendExists(backendID int64) bool {
	f.t.Helper()

	var n int64
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM cache_backends WHERE id = $1`, backendID).Scan(&n); err != nil {
		f.t.Fatalf("backendExists(%d): %v", backendID, err)
	}

	return n > 0
}

func (f *fixture) projectExists() bool {
	f.t.Helper()

	var n int64
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM projects WHERE id = $1`, f.projectID).Scan(&n); err != nil {
		f.t.Fatalf("projectExists: %v", err)
	}

	return n > 0
}

func (f *fixture) refcount(digest [32]byte) int64 {
	f.t.Helper()

	var n int64
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT refcount FROM blobs WHERE digest = $1`, digest[:]).Scan(&n); err != nil {
		f.t.Fatalf("refcount: %v", err)
	}

	return n
}

// TestTeardownEmptiesTheBackendAndRemovesTheRow is the headline: a marked backend
// full of objects is gone -- rows, refcounts and row -- after one tick, and a
// sibling backend in the same project is untouched.
//
// Every object here is YOUNG (Put stamps created_at = now()) and the backends carry
// the 90-day sstate default, so nothing in this test is age-eligible for anything.
// That is the point: the teardown stage has no age predicate at all, and a test that
// backdated its rows would pass even if the teardown did nothing and ordinary
// retention did the work.
func TestTeardownEmptiesTheBackendAndRemovesTheRow(t *testing.T) {
	f := newFixture(t, testConfig())

	doomed := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	sibling := f.backend(repository.BackendKindBazel, backendOpts{window: 30 * 24 * time.Hour})

	// Distinct content per key: identical bytes would dedup onto one blob row and the
	// refcount assertion below would prove nothing about the per-object accounting.
	digests := make([][32]byte, 0, 5)

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		digests = append(digests, f.put(doomed, nsDefault, "sstate:"+k, "doomed-"+k))
	}

	keptDigest := f.put(sibling, nsCAS, "cas-key", "kept")

	f.markDeleting(doomed)

	sum := f.run()

	if f.backendExists(doomed) {
		t.Error("the marked backend still exists after a tick that emptied it")
	}

	if got := f.keys(doomed, nsDefault); len(got) != 0 {
		t.Errorf("surviving keys = %v, want none", got)
	}

	if sum.ObjectsDeleted < 5 {
		t.Errorf("ObjectsDeleted = %d, want at least the 5 torn-down objects", sum.ObjectsDeleted)
	}

	// THE REFCOUNT IS THE PROOF THAT THIS WENT THROUGH blob.Service.DeleteBatch. A raw
	// DELETE (or the forbidden DeleteObjectsChunk) would also empty cache_objects, and
	// this is the assertion that tells them apart: the refcount trigger fires only on
	// the sanctioned path, and a blob nothing names is what Layer B is then allowed to
	// reap. Layer B runs later in the SAME tick, so with a zero grace period the rows
	// are gone entirely -- which is itself the end-to-end statement that the bytes were
	// released, not merely orphaned.
	for _, d := range digests {
		var n int64
		if err := f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM blobs WHERE digest = $1`, d[:]).Scan(&n); err != nil {
			t.Fatalf("count blob: %v", err)
		}

		if n != 0 {
			t.Errorf("a torn-down object's blob survived Layer B with refcount %d",
				f.refcount(d))
		}
	}

	// THE SIBLING IS UNTOUCHED. Teardown is scoped by deleting_at and by nothing else;
	// a stage that walked the project rather than the marked backend would take this
	// with it and no other assertion here would notice.
	if !f.backendExists(sibling) {
		t.Fatal("the sibling backend was deleted: teardown is not scoped to the marked row")
	}

	if !f.exists(sibling, nsCAS, "cas-key") {
		t.Error("the sibling backend's object was swept")
	}

	if got := f.blobState(keptDigest); got != "live" {
		t.Errorf("the sibling's blob state = %q, want live", got)
	}
}

// TestTeardownRunsUnderDisabledRetention pins the one place a teardown deliberately
// ignores the brake.
//
// --gc-disable-retention halts every retention stage AND Layer B's mark, because the
// incident it serves is "the policy deleted things we wanted" and those bytes are
// still inside the grace window. A teardown is not the policy: it is one explicit
// instruction a human issued through the API, and stalling it would mean a backend
// that cannot be deleted for as long as the brake is on -- which is exactly the state
// 000018 exists to end.
func TestTeardownRunsUnderDisabledRetention(t *testing.T) {
	cfg := testConfig()
	cfg.DisableRetention = true

	f := newFixture(t, cfg)

	doomed := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	digest := f.put(doomed, nsDefault, "sstate:only", "doomed")
	f.markDeleting(doomed)

	f.run()

	if f.backendExists(doomed) {
		t.Error("the marked backend survived a run under --gc-disable-retention")
	}

	// The brake still does its job: Layer B's MARK is halted, so the now-unreferenced
	// blob is still 'live' with a zero refcount and its bytes are still recoverable.
	// That is the whole point of the brake, and the teardown does not defeat it.
	if got := f.blobState(digest); got != "live" {
		t.Errorf("blob state = %q, want live: --gc-disable-retention must still halt Layer B's mark", got)
	}

	if got := f.refcount(digest); got != 0 {
		t.Errorf("refcount = %d, want 0", got)
	}
}

// TestTeardownEmptiesAHashservBackend covers the kind whose RESTRICT comes from
// somewhere else entirely.
//
// hashserv owns no cache_objects rows at all -- the retention sweep gives it no
// stages, and the object purge above would find nothing and declare it empty. Its
// foreign keys are on hashserv_unihashes and hashserv_outhashes (000010), so without
// the dedicated purge the row's delete fails with a 23503 on every tick, forever, and
// the backend can never be torn down.
func TestTeardownEmptiesAHashservBackend(t *testing.T) {
	f := newFixture(t, testConfig())

	doomed := f.backend(repository.BackendKindHashserv, backendOpts{window: 90 * 24 * time.Hour})

	// Young rows, again on purpose: the hashserv retention stages sweep on age and
	// would leave every one of these alone.
	now := time.Now()
	for _, task := range []string{"t1", "t2", "t3"} {
		f.unihash(doomed, task, "deadbeef"+task, now)
	}

	f.markDeleting(doomed)

	sum := f.run()

	if f.backendExists(doomed) {
		t.Error("the marked hashserv backend still exists")
	}

	for _, task := range []string{"t1", "t2", "t3"} {
		if f.unihashExists(doomed, task) {
			t.Errorf("unihash %q survived the teardown", task)
		}
	}

	if sum.HashservRows < 3 {
		t.Errorf("HashservRows = %d, want at least the 3 purged unihashes", sum.HashservRows)
	}
}

// TestTeardownDeletesTheProjectAfterItsLastBackend is the ordering rule one level up
// the ownership tree: projects -> cache_backends is ON DELETE RESTRICT, so the
// project row can only go after its last backend row has -- the same shape as "tags
// before manifests before blobs".
func TestTeardownDeletesTheProjectAfterItsLastBackend(t *testing.T) {
	f := newFixture(t, testConfig())

	one := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	two := f.backend(repository.BackendKindBazel, backendOpts{window: 30 * 24 * time.Hour})

	f.put(one, nsDefault, "sstate:a", "a")
	f.put(two, nsCAS, "cas-a", "b")

	// What handleDeleteProject writes: every backend marked, the project marked, all
	// in one transaction.
	f.markDeleting(one)
	f.markDeleting(two)
	f.markProjectDeleting()

	f.run()

	if f.backendExists(one) || f.backendExists(two) {
		t.Fatal("a marked backend survived the tick")
	}

	if f.projectExists() {
		t.Error("the marked project survived the tick that removed its last backend")
	}
}

// TestTeardownHealsABackendCreatedUnderADeletingProject is the STRANDED PROJECT, and
// it is the failure that has no way out once it happens.
//
// handleDeleteProject marks the project's backends and then the project, under READ
// COMMITTED. Its row lock on the project did not conflict with a concurrent
// POST .../backends -- a foreign key takes KEY SHARE on the parent, which NO KEY
// UPDATE does not block -- so a backend could land UNMARKED under a project that was
// already marked. That backend was in no worklist (the teardown listing asked
// `cb.deleting_at IS NOT NULL` alone) and ListProjectsForTeardown's anti-join could
// therefore never see zero backends. The project stayed in `deleting`: hidden from
// every listing, holding its slug, un-unmarkable by any endpoint, forever.
//
// The state below is exactly that one -- a marked project, an UNMARKED backend holding
// objects -- and the fix is two independent halves, both exercised here: the stage
// marks the straggler (MarkBackendsOfDeletingProjects) and the listing finds it either
// way (`OR p.deleting_at IS NOT NULL`).
func TestTeardownHealsABackendCreatedUnderADeletingProject(t *testing.T) {
	f := newFixture(t, testConfig())

	straggler := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	f.put(straggler, nsDefault, "sstate:a", "a")

	// No markDeleting on the backend: this is the row the race creates.
	f.markProjectDeleting()

	f.run()

	if f.backendExists(straggler) {
		t.Error("a backend created under a project already being torn down survived the tick")
	}

	if f.projectExists() {
		t.Error("the project stayed stranded in `deleting` with its slug held")
	}
}

// TestTeardownLeavesALiveProjectAlone is the other side of that disjunction. The
// listing now asks about the PROJECT as well as the backend, so the test that matters
// is that an unmarked backend under an unmarked project is still untouchable -- a
// predicate that over-reached here would tear down live caches.
func TestTeardownLeavesALiveProjectAlone(t *testing.T) {
	f := newFixture(t, testConfig())

	live := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	f.put(live, nsDefault, "sstate:a", "a")

	f.run()

	if !f.backendExists(live) {
		t.Error("a live backend under a live project was torn down")
	}

	if !f.projectExists() {
		t.Error("a live project was deleted")
	}
}

// errStuckBackend is one backend's delete failing for a reason the sweep cannot fix --
// a storage error, a lock timeout, a constraint nothing else explains.
var errStuckBackend = errors.New("teardown: this backend cannot be emptied")

// seedTeardownBackend puts one MARKED backend, with n objects, in front of the
// teardown stage. The unit fakes are the right harness for these two: the subject is
// the stage's LOOP -- whether one backend's failure ends the tick, and whether one
// backend's size owns it -- and neither is a property of the schema.
func seedTeardownBackend(q *fakeQueries, id int64, n int) {
	q.teardown = append(q.teardown, repository.ListBackendsForTeardownRow{
		ID: id, Kind: repository.BackendKindSstate, ProjectID: uuidOf(1),
		ProjectSlug: "widget", OrgSlug: "acme",
	})

	cold := pgtype.Timestamptz{Time: q.startedAt.Add(-time.Hour), InfinityModifier: 0, Valid: true}

	for i := range n {
		q.addObject(id, repository.ScanObjectsForGCRow{
			Namespace: nsDefault,
			Key:       fmt.Sprintf("object-%05d", i),
			Digest:    make([]byte, 32),
			SizeBytes: 10,
			CreatedAt: cold,
			AccessedAt: pgtype.Timestamptz{
				Time: time.Time{}, InfinityModifier: 0, Valid: false,
			},
			UpdatedAt:   cold,
			ContentType: pgtype.Text{String: "", Valid: false},
		})
	}
}

// TestTeardownStuckBackendDoesNotStarveItsSiblings pins the log-and-continue rule.
//
// The stage used to return on the first backend that errored, and it runs FIRST in
// every tick -- so one backend whose objects could not be deleted aborted the sweep
// before stage 1, every tick, indefinitely. Every other teardown, the project
// finisher, and all nine retention stages were starved by one row. The error must
// still surface (the run lands `failed`), it just must not decide what the rest of the
// tick gets to do.
func TestTeardownStuckBackendDoesNotStarveItsSiblings(t *testing.T) {
	eng, q, b := fakeEngine(t, testConfig())

	seedTeardownBackend(q, 1, 2)
	seedTeardownBackend(q, 2, 2)

	b.deleteErr[1] = errStuckBackend

	sum, err := eng.Run(t.Context(), TriggerAPI, false)
	if err == nil {
		t.Fatal("Run() error = nil, want the stuck backend's failure to surface")
	}

	if !errors.Is(err, errStuckBackend) {
		t.Errorf("Run() error = %v, want it to wrap the stuck backend's error", err)
	}

	if len(q.dropped) != 1 || q.dropped[0] != 2 {
		t.Errorf("dropped backends = %v, want only the healthy sibling (2)", q.dropped)
	}

	if q.count("ListProjectsForTeardown") != 1 {
		t.Errorf("ListProjectsForTeardown ran %d times, want 1: the project finisher must "+
			"still run after a backend fails", q.count("ListProjectsForTeardown"))
	}

	if sum.ObjectsDeleted != 2 {
		t.Errorf("ObjectsDeleted = %d, want 2 (the sibling's objects)", sum.ObjectsDeleted)
	}
}

// TestTeardownBudgetStopsAndResumes pins the per-tick object budget.
//
// purgeObjects used to loop until the backend was empty. On a ten-million-object
// backend that is thousands of pages plus a --gc-batch-pause between each: one
// teardown owning the whole --gc-interval while every other backend waits. The budget
// caps it at teardownMaxPages pages, and the property that makes that safe is that the
// backend RESUMES -- the keyset restarts from the sentinel over the rows that are
// left, so nothing is skipped and the row is dropped only once it is really empty.
func TestTeardownBudgetStopsAndResumes(t *testing.T) {
	cfg := testConfig()
	cfg.BatchSize = 2

	eng, q, b := fakeEngine(t, cfg)

	const total = 2*teardownMaxPages + 4

	seedTeardownBackend(q, 1, total)

	// The corpus has to shrink as the deletes land, the way the real scan's does.
	b.onDelete = func(refs []blob.DeleteRef) { q.removeObjects(refs) }

	first, err := eng.Run(t.Context(), TriggerAPI, false)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}

	if first.ObjectsDeleted != int64(cfg.BatchSize*teardownMaxPages) {
		t.Errorf("first tick deleted %d objects, want the budget (%d)",
			first.ObjectsDeleted, cfg.BatchSize*teardownMaxPages)
	}

	if len(q.dropped) != 0 {
		t.Errorf("dropped = %v, want none: the backend is not empty yet", q.dropped)
	}

	second, err := eng.Run(t.Context(), TriggerAPI, false)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	if second.ObjectsDeleted != int64(total-cfg.BatchSize*teardownMaxPages) {
		t.Errorf("second tick deleted %d objects, want the remainder (%d)",
			second.ObjectsDeleted, total-cfg.BatchSize*teardownMaxPages)
	}

	if len(q.dropped) != 1 || q.dropped[0] != 1 {
		t.Errorf("dropped = %v, want the backend row removed once it was really empty", q.dropped)
	}
}
