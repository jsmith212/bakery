package gc

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Hand-written fakes, same as every other package here: no testify, no gomock. They
// exist for the tests whose subject is the ENGINE's behaviour -- pacing, the
// multi-instance refusal, the terminal run row -- where a real Postgres would only
// add latency and hide the assertion. Everything whose subject is a PREDICATE runs
// against a real database instead, because the predicate lives in SQL.

// fakeQueries is an in-memory Queries.
type fakeQueries struct {
	mu sync.Mutex

	backends []repository.ListBackendsForGCRow

	// objects is the scannable corpus, keyed by backend id and kept sorted by
	// (namespace, key) so the keyset cursor behaves as the real index does.
	objects map[int64][]repository.ScanObjectsForGCRow

	pending []repository.ListPendingDeleteBlobsRow
	marked  []repository.MarkBlobsPendingDeleteRow

	unihashes map[int64]map[string]struct{}

	nextRun   int64
	startedAt time.Time

	calls      map[string]int
	scanLimits []int32
	scanAt     []time.Time
	finished   []repository.FinishGCRunParams
	usage      []repository.UpsertBackendUsageParams
	// runBackends records every RecordGCRunBackend call (B7, 000013), in call
	// order, so a test can assert exactly which backends got a row and with what
	// totals for a given run -- including that a declined or refused backend, or
	// a dry/usage run, produced NONE.
	runBackends []repository.RecordGCRunBackendParams

	startErr error
	scanErr  error

	// rampUntil/rampErr drive GetGCState, the touch-staleness ramp clock.
	rampUntil time.Time
	rampErr   error

	// onScan runs inside ScanObjectsForGC, before it answers.
	onScan func(page int)

	// The teardown stage's worklists. teardown is what ListBackendsForTeardown
	// answers; dropped records every DeleteTornDownBackend that reached a row, which
	// is the only observable difference between "this backend finished" and "this
	// backend used its per-tick budget and will continue".
	teardown []repository.ListBackendsForTeardownRow
	dropped  []int64

	// measureErr fails the read-triggered aggregate, which is how a test builds the
	// project whose measurement never succeeds -- the one that used to be re-measured
	// on every dashboard load, forever.
	measureErr error
}

func newFakeQueries() *fakeQueries {
	return &fakeQueries{
		mu: sync.Mutex{}, backends: nil,
		objects:   map[int64][]repository.ScanObjectsForGCRow{},
		pending:   nil,
		marked:    nil,
		unihashes: map[int64]map[string]struct{}{},
		nextRun:   0, startedAt: time.Now(),
		calls: map[string]int{}, scanLimits: nil, scanAt: nil, finished: nil, usage: nil,
		runBackends: nil,
		startErr:    nil, scanErr: nil, rampUntil: time.Time{}, rampErr: nil, onScan: nil,
		teardown: nil, dropped: nil,
	}
}

// removeObjects drops the rows a DeleteBatch reported deleting, so a second tick sees
// the corpus the first one left behind. The real ScanObjectsForGC gets this for free
// from the delete; the fake's corpus is a slice and has to be told.
func (f *fakeQueries) removeObjects(refs []blob.DeleteRef) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, ref := range refs {
		rows := f.objects[ref.BackendID]

		for i := range rows {
			if rows[i].Namespace == ref.Namespace && rows[i].Key == ref.Key {
				f.objects[ref.BackendID] = append(rows[:i], rows[i+1:]...)

				break
			}
		}
	}
}

func (f *fakeQueries) note(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls[name]++
}

func (f *fakeQueries) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[name]
}

func (f *fakeQueries) addObject(backendID int64, row repository.ScanObjectsForGCRow) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rows := append(f.objects[backendID], row)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}

		return rows[i].Key < rows[j].Key
	})

	f.objects[backendID] = rows
}

func (f *fakeQueries) StartGCRun(
	_ context.Context, arg repository.StartGCRunParams,
) (repository.StartGCRunRow, error) {
	f.note("StartGCRun")

	if f.startErr != nil {
		return repository.StartGCRunRow{ID: 0, StartedAt: pgtype.Timestamptz{}}, f.startErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextRun++
	_ = arg

	return repository.StartGCRunRow{
		ID:        f.nextRun,
		StartedAt: pgtype.Timestamptz{Time: f.startedAt, InfinityModifier: 0, Valid: true},
	}, nil
}

func (f *fakeQueries) FinishGCRun(_ context.Context, arg repository.FinishGCRunParams) (int64, error) {
	f.note("FinishGCRun")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.finished = append(f.finished, arg)

	return 1, nil
}

func (f *fakeQueries) MarkOrphanedGCRunsFailed(_ context.Context) (int64, error) {
	f.note("MarkOrphanedGCRunsFailed")

	return 0, nil
}

// MeasureProjectUsage: the read-triggered refresh's one statement. Returns nothing
// here -- the measurement's behaviour is DB-backed (measure_test.go), because its
// whole subject is a grouped aggregate over real rows -- while the singleflight and
// the error path are exercised through the real engine.
func (f *fakeQueries) MeasureProjectUsage(
	_ context.Context, _ pgtype.UUID,
) ([]repository.MeasureProjectUsageRow, error) {
	f.note("MeasureProjectUsage")

	f.mu.Lock()
	err := f.measureErr
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}

	return nil, nil
}

// The teardown surface (000018). A fake with no marked backends: every unit test in
// this package drives the retention stages, and the teardown stage's own behaviour is
// DB-backed (internal/db/gc_teardown_test.go) because its whole subject -- a RESTRICT
// foreign key refusing a delete until the last row is gone -- is a property of the
// schema and not of any Go code a fake could stand in for.
func (f *fakeQueries) MarkBackendsOfDeletingProjects(_ context.Context) (int64, error) {
	f.note("MarkBackendsOfDeletingProjects")

	return 0, nil
}

func (f *fakeQueries) ListBackendsForTeardown(
	_ context.Context,
) ([]repository.ListBackendsForTeardownRow, error) {
	f.note("ListBackendsForTeardown")

	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]repository.ListBackendsForTeardownRow(nil), f.teardown...), nil
}

func (f *fakeQueries) DeleteTornDownBackend(_ context.Context, id int64) (int64, error) {
	f.note("DeleteTornDownBackend")

	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.teardown {
		if f.teardown[i].ID == id {
			f.teardown = append(f.teardown[:i], f.teardown[i+1:]...)
			f.dropped = append(f.dropped, id)

			return 1, nil
		}
	}

	return 0, nil
}

func (f *fakeQueries) PurgeHashservUnihashesChunk(
	_ context.Context, _ repository.PurgeHashservUnihashesChunkParams,
) (int64, error) {
	f.note("PurgeHashservUnihashesChunk")

	return 0, nil
}

func (f *fakeQueries) PurgeHashservOuthashesChunk(
	_ context.Context, _ repository.PurgeHashservOuthashesChunkParams,
) (int64, error) {
	f.note("PurgeHashservOuthashesChunk")

	return 0, nil
}

func (f *fakeQueries) ListProjectsForTeardown(
	_ context.Context,
) ([]repository.ListProjectsForTeardownRow, error) {
	f.note("ListProjectsForTeardown")

	return nil, nil
}

func (f *fakeQueries) DeleteTornDownProject(_ context.Context, _ pgtype.UUID) (int64, error) {
	f.note("DeleteTornDownProject")

	return 0, nil
}

func (f *fakeQueries) ListBackendsForGC(_ context.Context) ([]repository.ListBackendsForGCRow, error) {
	f.note("ListBackendsForGC")

	return f.backends, nil
}

func (f *fakeQueries) ScanObjectsForGC(
	_ context.Context, arg repository.ScanObjectsForGCParams,
) ([]repository.ScanObjectsForGCRow, error) {
	f.note("ScanObjectsForGC")

	f.mu.Lock()
	page := len(f.scanLimits)
	f.scanLimits = append(f.scanLimits, arg.ScanLimit)
	f.scanAt = append(f.scanAt, time.Now())
	rows := f.objects[arg.BackendID]
	onScan := f.onScan
	err := f.scanErr
	f.mu.Unlock()

	if onScan != nil {
		onScan(page)
	}

	if err != nil {
		return nil, err
	}

	out := make([]repository.ScanObjectsForGCRow, 0, arg.ScanLimit)

	for _, row := range rows {
		if row.Namespace < arg.AfterNamespace {
			continue
		}

		if row.Namespace == arg.AfterNamespace && row.Key <= arg.AfterKey {
			continue
		}

		out = append(out, row)

		if len(out) == int(arg.ScanLimit) {
			break
		}
	}

	return out, nil
}

func (f *fakeQueries) UnihashesExistBatch(
	_ context.Context, arg repository.UnihashesExistBatchParams,
) ([]string, error) {
	f.note("UnihashesExistBatch")

	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for _, u := range arg.Unihashes {
		if _, ok := f.unihashes[arg.BackendID][u]; ok {
			out = append(out, u)
		}
	}

	return out, nil
}

func (f *fakeQueries) HashservBackendHasUnihashes(_ context.Context, backendID int64) (bool, error) {
	f.note("HashservBackendHasUnihashes")

	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.unihashes[backendID]) > 0, nil
}

func (f *fakeQueries) SweepUnihashes(_ context.Context, _ repository.SweepUnihashesParams) (int64, error) {
	f.note("SweepUnihashes")

	return 0, nil
}

func (f *fakeQueries) DryRunSweepUnihashes(
	_ context.Context, _ repository.DryRunSweepUnihashesParams,
) (int64, error) {
	f.note("DryRunSweepUnihashes")

	return 0, nil
}

func (f *fakeQueries) SweepOrphanedOuthashes(
	_ context.Context, _ repository.SweepOrphanedOuthashesParams,
) (int64, error) {
	f.note("SweepOrphanedOuthashes")

	return 0, nil
}

func (f *fakeQueries) DryRunSweepOrphanedOuthashes(
	_ context.Context, _ repository.DryRunSweepOrphanedOuthashesParams,
) (int64, error) {
	f.note("DryRunSweepOrphanedOuthashes")

	return 0, nil
}

func (f *fakeQueries) NullOrphanSiginfo(
	_ context.Context, _ repository.NullOrphanSiginfoParams,
) (int64, error) {
	f.note("NullOrphanSiginfo")

	return 0, nil
}

func (f *fakeQueries) DryRunNullOrphanSiginfo(
	_ context.Context, _ repository.DryRunNullOrphanSiginfoParams,
) (int64, error) {
	f.note("DryRunNullOrphanSiginfo")

	return 0, nil
}

func (f *fakeQueries) MarkBlobsPendingDelete(
	_ context.Context, _ repository.MarkBlobsPendingDeleteParams,
) ([]repository.MarkBlobsPendingDeleteRow, error) {
	f.note("MarkBlobsPendingDelete")

	f.mu.Lock()
	defer f.mu.Unlock()

	out := f.marked
	f.marked = nil

	return out, nil
}

func (f *fakeQueries) ListPendingDeleteBlobs(
	_ context.Context, _ int32,
) ([]repository.ListPendingDeleteBlobsRow, error) {
	f.note("ListPendingDeleteBlobs")

	f.mu.Lock()
	defer f.mu.Unlock()

	out := f.pending
	f.pending = nil

	return out, nil
}

func (f *fakeQueries) UpsertBackendUsage(_ context.Context, arg repository.UpsertBackendUsageParams) error {
	f.note("UpsertBackendUsage")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.usage = append(f.usage, arg)

	return nil
}

func (f *fakeQueries) InstancePhysicalBytes(_ context.Context) (int64, error) {
	f.note("InstancePhysicalBytes")

	return 0, nil
}

func (f *fakeQueries) RecordGCRunBackend(
	_ context.Context, arg repository.RecordGCRunBackendParams,
) error {
	f.note("RecordGCRunBackend")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.runBackends = append(f.runBackends, arg)

	return nil
}

func (f *fakeQueries) SweepUnreferencedManifests(
	_ context.Context, _ repository.SweepUnreferencedManifestsParams,
) ([]string, error) {
	f.note("SweepUnreferencedManifests")

	return nil, nil
}

func (f *fakeQueries) DryRunSweepUnreferencedManifests(
	_ context.Context, _ repository.DryRunSweepUnreferencedManifestsParams,
) (int64, error) {
	f.note("DryRunSweepUnreferencedManifests")

	return 0, nil
}

func (f *fakeQueries) GetGCState(_ context.Context) (pgtype.Timestamptz, error) {
	f.note("GetGCState")

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.rampErr != nil {
		return pgtype.Timestamptz{}, f.rampErr
	}

	return pgtype.Timestamptz{Time: f.rampUntil, InfinityModifier: 0, Valid: !f.rampUntil.IsZero()}, nil
}

// fakeBlobs records what the engine asked it to delete and answers the veto from a
// set the test controls.
type fakeBlobs struct {
	mu sync.Mutex

	deleted []blob.DeleteRef
	// deleteRuns records the run id every DeleteBatch call carried: it is the write
	// barrier, and a zero here would mean the sweep deleted against no run at all.
	deleteRuns  []int64
	invalidated []string
	reaped      int
	pending     map[string]struct{}

	// deleteErr fails DeleteBatch for ONE backend id, which is how a test builds the
	// "one stuck backend" the teardown stage must not let starve its siblings.
	deleteErr map[int64]error

	// onDelete runs after a successful DeleteBatch, so a test can keep the queries
	// fake's corpus in step with what has been deleted across ticks.
	onDelete func(refs []blob.DeleteRef)
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{
		mu: sync.Mutex{}, deleted: nil, deleteRuns: nil, invalidated: nil,
		reaped: 0, pending: map[string]struct{}{},
		deleteErr: map[int64]error{}, onDelete: nil,
	}
}

func (f *fakeBlobs) DeleteBatch(_ context.Context, runID int64, refs []blob.DeleteRef) (int64, error) {
	f.mu.Lock()

	if len(refs) > 0 {
		if err := f.deleteErr[refs[0].BackendID]; err != nil {
			f.mu.Unlock()

			return 0, err
		}
	}

	f.deleted = append(f.deleted, refs...)
	f.deleteRuns = append(f.deleteRuns, runID)
	onDelete := f.onDelete
	f.mu.Unlock()

	if onDelete != nil {
		onDelete(refs)
	}

	return int64(len(refs)), nil
}

func (f *fakeBlobs) InvalidateKeys(_ int64, _ string, keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.invalidated = append(f.invalidated, keys...)
}

func (f *fakeBlobs) ReapDigest(_ context.Context, _ blob.Digest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reaped++

	return true, nil
}

func (f *fakeBlobs) PendingTouch(_ int64, namespace, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.pending[namespace+"\x00"+key]

	return ok
}

func (f *fakeBlobs) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.deleted))
	for _, r := range f.deleted {
		out = append(out, r.Key)
	}

	return out
}
