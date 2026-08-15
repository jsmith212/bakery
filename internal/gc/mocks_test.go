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
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{
		mu: sync.Mutex{}, deleted: nil, deleteRuns: nil, invalidated: nil,
		reaped: 0, pending: map[string]struct{}{},
	}
}

func (f *fakeBlobs) DeleteBatch(_ context.Context, runID int64, refs []blob.DeleteRef) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deleted = append(f.deleted, refs...)
	f.deleteRuns = append(f.deleteRuns, runID)

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
