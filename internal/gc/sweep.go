package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Summary is what one sweep did. It is both the gc_runs row's contents and what the
// API trigger reports back.
type Summary struct {
	RunID   int64
	Trigger Trigger
	DryRun  bool

	ObjectsDeleted int64
	HashservRows   int64
	BlobsMarked    int64
	BlobsDeleted   int64
	BytesReclaimed int64

	// BackendsRefused counts sstate backends the coverage guard would not sweep
	// (spec §5). Non-zero is an operator-visible condition, not an error.
	BackendsRefused int
}

// coverageProbePages bounds the sstate coverage guard's look-ahead (spec §5).
//
// The guard must decide BEFORE deleting anything, and coverage is only known after
// scanning -- so it is evaluated over a bounded PREFIX of the backend's keys rather
// than over the whole namespace, which would mean scanning sstate twice per run.
// A backend whose first coverageProbePages x --gc-batch-size keys resolve NOTHING
// while its paired hashserv holds rows is refused: that combination means the
// derivation broke, and refusing is the direction that cannot delete a live cache.
// The coverage GAUGE is still computed over the full scan, so the number an
// operator sees is not the probe's.
const coverageProbePages = 8

// Run executes one sweep, start to terminal gc_runs status, and BLOCKS until it
// finishes. It is what the interval loop and a dry run use, both of which want the
// final Summary synchronously. POST /api/v1/gc/run uses TriggerAsync instead (spec
// §9.10): a sweep can legitimately run for hours, and an HTTP request must not
// block on one.
//
// THE RUN ROW IS INSERTED AND COMMITTED FIRST, before any scanning: started_at and
// snapshot are frozen as of that transaction and every sweep statement afterwards
// filters against them by run id. That pair IS the write barrier, and it is why a
// build that began before this run and commits during it is spared.
func (e *Engine) Run(ctx context.Context, trigger Trigger, dryRun bool) (Summary, error) {
	if !dryRun {
		if !e.running.CompareAndSwap(false, true) {
			return Summary{}, ErrAlreadyRunning //nolint:exhaustruct // a refused run has no results
		}

		defer e.running.Store(false)
	}

	run, err := e.db.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: interval(e.cfg.GracePeriod),
		Trigger:     string(trigger),
		DryRun:      dryRun,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("start gc run: %w", err) //nolint:exhaustruct // no run exists
	}

	return e.runFrom(ctx, run, trigger, dryRun)
}

// TriggerAsync starts a sweep and returns its run id AS SOON AS the gc_runs row
// exists, without waiting for the sweep itself to finish (spec §9.10). It exists
// for POST /api/v1/gc/run, which must answer 202 immediately with an id an
// operator can poll GET .../runs/{id} for -- not block the request on a pass that
// can legitimately run for hours.
//
// The already-running decision is made HERE, synchronously, by the SAME
// CompareAndSwap Run uses: the caller never observes a 202 that turns out to have
// started nothing, and ErrAlreadyRunning renders 409 exactly as it does from Run.
//
// The sweep itself runs under context.WithoutCancel(ctx): the caller is an HTTP
// handler whose request context dies with the response, on some servers the
// instant this function returns, and a sweep tied to it would be cancelled before
// its first chunk. If this process exits before an async sweep finishes, it leaves
// its gc_runs row 'running' -- exactly the state the boot reaper
// (MarkOrphanedGCRunsFailed / ReapOrphanedRuns) exists to clean up on next start,
// the same as a sweep killed by a hard process exit ever would.
func (e *Engine) TriggerAsync(ctx context.Context, trigger Trigger, dryRun bool) (int64, error) {
	if !dryRun {
		if !e.running.CompareAndSwap(false, true) {
			return 0, ErrAlreadyRunning
		}
	}

	run, err := e.db.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: interval(e.cfg.GracePeriod),
		Trigger:     string(trigger),
		DryRun:      dryRun,
	})
	if err != nil {
		if !dryRun {
			e.running.Store(false)
		}

		return 0, fmt.Errorf("start gc run: %w", err)
	}

	bg := context.WithoutCancel(ctx)

	go func() {
		if !dryRun {
			defer e.running.Store(false)
		}

		if _, runErr := e.runFrom(bg, run, trigger, dryRun); runErr != nil &&
			!errors.Is(runErr, context.Canceled) {
			e.log.ErrorContext(bg, "gc run failed",
				slog.Int64("run", run.ID), slog.Any("error", runErr))
		}
	}()

	return run.ID, nil
}

// runFrom drives an ALREADY-STARTED run's sweep through to a terminal gc_runs
// status. Both Run (synchronous) and TriggerAsync's goroutine (detached) share
// this: the started-run bookkeeping below -- the Summary, the log lines, the
// terminal write, the metrics -- must happen exactly once per run regardless of
// which entry point started it.
func (e *Engine) runFrom(
	ctx context.Context, run repository.StartGCRunRow, trigger Trigger, dryRun bool,
) (Summary, error) {
	started := time.Now()

	sum := Summary{
		RunID: run.ID, Trigger: trigger, DryRun: dryRun,
		ObjectsDeleted: 0, HashservRows: 0, BlobsMarked: 0, BlobsDeleted: 0,
		BytesReclaimed: 0, BackendsRefused: 0,
	}

	e.log.InfoContext(ctx, "gc run started",
		slog.Int64("run", run.ID), slog.String("trigger", string(trigger)),
		slog.Bool("dry_run", dryRun), slog.Bool("retention", !e.cfg.DisableRetention))

	sweepErr := e.sweep(ctx, run, &sum)

	// The finisher runs under WithoutCancel because the ordinary way to reach it with
	// an error is that ctx was cancelled by shutdown. A run left 'running' is not a
	// cosmetic problem: it holds the partial unique index's active slot and every
	// later real run collides with it until the boot reaper clears it.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	status := repository.GcRunStatusSucceeded
	msg := ""

	if sweepErr != nil {
		status = repository.GcRunStatusFailed
		msg = runError(sweepErr)
	}

	e.finish(finishCtx, run.ID, status, msg, sum)
	e.rec.RunFinished(string(status), string(trigger), time.Since(started), time.Now())

	e.log.InfoContext(finishCtx, "gc run finished",
		slog.Int64("run", run.ID), slog.String("status", string(status)),
		slog.Int64("objects", sum.ObjectsDeleted), slog.Int64("hashserv_rows", sum.HashservRows),
		slog.Int64("blobs_marked", sum.BlobsMarked), slog.Int64("blobs_deleted", sum.BlobsDeleted),
		slog.Int64("bytes_reclaimed", sum.BytesReclaimed), slog.Int("refused", sum.BackendsRefused),
		slog.Duration("took", time.Since(started)))

	if sweepErr != nil {
		return sum, sweepErr
	}

	return sum, nil
}

// finish writes the terminal gc_runs row. A zero row count means this call LOST a
// race -- the run already reached a terminal state -- which is a normal outcome for
// the boot reaper racing a shutdown finisher and must not be reported as a failure.
func (e *Engine) finish(
	ctx context.Context, runID int64, status repository.GcRunStatus, msg string, sum Summary,
) {
	rows, err := e.db.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID:                  runID,
		Status:              status,
		Error:               text(msg),
		ObjectsDeleted:      sum.ObjectsDeleted,
		BlobsMarked:         sum.BlobsMarked,
		BlobsDeleted:        sum.BlobsDeleted,
		BytesReclaimed:      sum.BytesReclaimed,
		HashservRowsDeleted: sum.HashservRows,
	})
	if err != nil {
		e.log.ErrorContext(ctx, "finishing gc run", slog.Int64("run", runID), slog.Any("error", err))

		return
	}

	if rows == 0 {
		e.log.WarnContext(ctx, "gc run was already finished by another writer", slog.Int64("run", runID))
	}
}

// sweep is the staged pass, in spec §3's order.
func (e *Engine) sweep(ctx context.Context, run repository.StartGCRunRow, sum *Summary) error {
	// STAGE 0 -- the crash re-drive, and it runs even under --gc-disable-retention
	// (spec §9.6): a blob already in pending_delete is past recovery, so stalling it
	// reclaims nothing and protects nothing.
	if !sum.DryRun {
		if err := e.reapPending(ctx, sum); err != nil {
			return err
		}
	}

	if e.cfg.DisableRetention {
		e.log.WarnContext(ctx, "retention is disabled: stages 1-9 and Layer B's mark are halted",
			slog.Int64("run", run.ID))

		return e.publishPhysical(ctx)
	}

	rows, err := e.db.ListBackendsForGC(ctx)
	if err != nil {
		return fmt.Errorf("list backends: %w", err)
	}

	plans := buildPlans(rows)
	now := run.StartedAt.Time

	// STAGES 1-2 -- hashserv, THE GC ROOT, before anything that is reachable from it.
	for _, p := range plans {
		if p.kind != repository.BackendKindHashserv {
			continue
		}

		if err := e.sweepHashserv(ctx, run.ID, p, sum); err != nil {
			return err
		}
	}

	// STAGES 3-9 -- cache_objects, one backend at a time. Backends are independent of
	// each other; the order that matters is WITHIN a backend and buildPlans has
	// already fixed it.
	//
	// The gauge vecs are RESET first so a backend that has been deleted since the last
	// sweep stops exporting its last value forever -- a gauge vec is a map, not a
	// snapshot. Everything this pass will NOT measure (hashserv, which owns no
	// cache_objects rows, and inert backends it declines to scan) is immediately
	// republished from the last measurement, because "not measured this pass" and
	// "gone" must not look the same on a dashboard.
	if !sum.DryRun {
		e.rec.ResetUsage()
		e.republishCached(plans)
	}

	for _, p := range plans {
		if err := e.sweepBackend(ctx, run.ID, now, p, sum); err != nil {
			return err
		}
	}

	// STAGE 10 -- Layer B. Global, namespace-blind, last, and it only ever sees
	// digests Postgres has already agreed nothing names.
	if !sum.DryRun {
		if err := e.markLayerB(ctx, run.ID, sum); err != nil {
			return err
		}

		if err := e.reapPending(ctx, sum); err != nil {
			return err
		}
	}

	return e.publishPhysical(ctx)
}

// sweepHashserv runs stages 1 and 2: the unihash root, then the orphaned outhashes
// and the siginfo nulling.
//
// Both are self-contained SQL sweeps carrying the write barrier themselves. Nothing
// here goes through blob.Service: hashserv is the one backend that owns no
// cache_objects rows and no blobs, so there is no LRU to invalidate and no byte to
// reclaim.
func (e *Engine) sweepHashserv(ctx context.Context, runID int64, p backendPlan, sum *Summary) error {
	if !p.hasWindow {
		return nil
	}

	w := interval(p.window)
	siginfo := interval(p.window * siginfoWindowFactor)

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	var unihashes, outhashes, siginfos int64

	var err error

	if sum.DryRun {
		unihashes, err = e.db.DryRunSweepUnihashes(stmtCtx, repository.DryRunSweepUnihashesParams{
			BackendID: p.id, RetentionWindow: w, RunID: runID,
		})
		if err == nil {
			outhashes, err = e.db.DryRunSweepOrphanedOuthashes(stmtCtx,
				repository.DryRunSweepOrphanedOuthashesParams{BackendID: p.id, RetentionWindow: w, RunID: runID})
		}

		if err == nil {
			siginfos, err = e.db.DryRunNullOrphanSiginfo(stmtCtx,
				repository.DryRunNullOrphanSiginfoParams{BackendID: p.id, RetentionWindow: siginfo, RunID: runID})
		}
	} else {
		unihashes, err = e.db.SweepUnihashes(stmtCtx, repository.SweepUnihashesParams{
			BackendID: p.id, RetentionWindow: w, RunID: runID,
		})
		if err == nil {
			outhashes, err = e.db.SweepOrphanedOuthashes(stmtCtx,
				repository.SweepOrphanedOuthashesParams{BackendID: p.id, RetentionWindow: w, RunID: runID})
		}

		if err == nil {
			siginfos, err = e.db.NullOrphanSiginfo(stmtCtx,
				repository.NullOrphanSiginfoParams{BackendID: p.id, RetentionWindow: siginfo, RunID: runID})
		}
	}

	if err != nil {
		return fmt.Errorf("sweep hashserv backend %d: %w", p.id, err)
	}

	sum.HashservRows += unihashes + outhashes

	if unihashes+outhashes+siginfos > 0 {
		e.log.InfoContext(ctx, "swept hashserv",
			slog.String("org", p.org), slog.String("project", p.project),
			slog.Int64("unihashes", unihashes), slog.Int64("outhashes", outhashes),
			slog.Int64("siginfo_cleared", siginfos), slog.Bool("dry_run", sum.DryRun))
	}

	return nil
}

// sweepBackend runs one backend's cache_objects stages, publishes its usage, and
// then -- if it is over quota -- runs the eviction pass.
func (e *Engine) sweepBackend(
	ctx context.Context, runID int64, now time.Time, p backendPlan, sum *Summary,
) error {
	if len(p.stages) == 0 {
		return nil
	}

	// Inert configuration costs nothing (spec §8, finding 7a): no window and no quota
	// means there is nothing this pass could decide, so the rows are not read at all.
	// Their usage is measured by the lightweight pass instead.
	if !p.managed() {
		return nil
	}

	refused, err := e.sstateRefused(ctx, runID, &p)
	if err != nil {
		return err
	}

	if refused {
		sum.BackendsRefused++
		e.rec.SstateCoverage(p.org, p.project, 0)

		return nil
	}

	u := newUsage(len(p.stages))
	cov := coverage{scanned: 0, resolved: 0}

	// tagDigests is stage 8's anti-join, built by stage 7 as it runs: every tag that
	// SURVIVED the tag sweep protects the manifest it names. It is a Go-side join
	// rather than the SQL NOT EXISTS because a manifest must be deleted through
	// blob.Service.DeleteBatch -- manifests are read through the LRU (oci.serveManifest
	// goes through Blobs.Get), so a raw SQL delete would leave a positive cache entry
	// for an object that no longer exists.
	tagDigests := map[string]struct{}{}

	for i, st := range p.stages {
		decide := e.deciderFor(&p, st, now, &cov, tagDigests)

		if err := e.runStage(ctx, runID, &p, st, i, now, decide, u, sum); err != nil {
			return err
		}
	}

	if p.kind == repository.BackendKindSstate {
		e.rec.SstateCoverage(p.org, p.project, cov.fraction())
	}

	e.publishUsage(ctx, p, u, sum.DryRun)

	return e.evictToQuota(ctx, runID, now, &p, u, sum)
}

// decider decides which rows of one scan page are doomed. It is per PAGE rather than
// per row because the sstate rule needs a batch existence probe: one round trip per
// page instead of one per key.
type decider func(ctx context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error)

// deciderFor builds a stage's retention rule. Each is spec §3's third predicate --
// the write barrier's two halves are already applied by ScanObjectsForGC.
func (e *Engine) deciderFor(
	p *backendPlan, st stage, now time.Time, cov *coverage, tagDigests map[string]struct{},
) decider {
	cutoff := now.Add(-st.window)
	live := st.window > 0 && p.hasWindow

	switch {
	case p.kind == repository.BackendKindSstate:
		return e.sstateDecider(p, cutoff, live, cov)

	case st.namespace == nsAC, st.namespace == nsACGRPC, st.namespace == nsSccache:
		// greatest(created_at, coalesce(accessed_at, created_at)): the /ac namespaces
		// are the only OVERWRITABLE ones, and an overwrite refreshes created_at. Using
		// the coalesce rule alone would let a rewritten entry inherit the age of the
		// read record it replaced.
		return rowRule(func(row repository.ScanObjectsForGCRow) bool {
			return live && row.CreatedAt.Time.Before(cutoff) && livenessOf(row).Before(cutoff)
		})

	case st.namespace == nsTags:
		// updated_at, not accessed_at: the stale-while-revalidate refresh already
		// maintains it, and tags deliberately bypass the LRU in both directions, so
		// accessed_at on a tag is always NULL and would be a second, disagreeing answer.
		return func(_ context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error) {
			mask := make([]bool, len(rows))

			for i, row := range rows {
				mask[i] = live && row.UpdatedAt.Time.Before(cutoff)

				if !mask[i] {
					tagDigests[string(row.Digest)] = struct{}{}
				}
			}

			return mask, nil
		}

	case st.namespace == nsManifests:
		return rowRule(func(row repository.ScanObjectsForGCRow) bool {
			if !live || !livenessOf(row).Before(cutoff) {
				return false
			}

			_, tagged := tagDigests[string(row.Digest)]

			return !tagged
		})

	default:
		return rowRule(func(row repository.ScanObjectsForGCRow) bool {
			return live && livenessOf(row).Before(cutoff)
		})
	}
}

// rowRule lifts a per-row predicate into a decider.
func rowRule(fn rule) decider {
	return func(_ context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error) {
		mask := make([]bool, len(rows))
		for i, row := range rows {
			mask[i] = fn(row)
		}

		return mask, nil
	}
}

// sstateDecider is spec §5's conjunctive rule: dead iff the window elapsed AND no
// surviving unihash on the paired hashserv backend names it.
//
// An unparseable key resolves to no unihash and is therefore UNREACHABLE -- it dies
// on age alone. That is only safe because the rule is conjunctive: a swspec object
// (do_populate_lic, empty arch fields) is legal and common, and treating it as
// immediately deletable rather than merely age-eligible would delete a live cache.
func (e *Engine) sstateDecider(p *backendPlan, cutoff time.Time, live bool, cov *coverage) decider {
	return func(ctx context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error) {
		mask := make([]bool, len(rows))
		derived := make([]string, len(rows))

		probe := make([]string, 0, len(rows))
		seen := map[string]struct{}{}

		for i, row := range rows {
			u, ok := deriveUnihash(row.Key)
			if !ok {
				continue
			}

			derived[i] = u

			if _, dup := seen[u]; !dup {
				seen[u] = struct{}{}
				probe = append(probe, u)
			}
		}

		alive := map[string]struct{}{}

		if p.hashserv != 0 && len(probe) > 0 {
			stmtCtx, cancel := chunkCtx(ctx)

			found, err := e.db.UnihashesExistBatch(stmtCtx, repository.UnihashesExistBatchParams{
				BackendID: p.hashserv, Unihashes: probe,
			})

			cancel()

			if err != nil {
				return nil, fmt.Errorf("probe unihashes for backend %d: %w", p.id, err)
			}

			for _, u := range found {
				alive[u] = struct{}{}
			}
		}

		cov.scanned += int64(len(rows))

		for i, row := range rows {
			_, reachable := alive[derived[i]]

			if derived[i] != "" && reachable {
				cov.resolved++
			}

			mask[i] = live && !reachable && livenessOf(row).Before(cutoff)
		}

		return mask, nil
	}
}

// sstateRefused is the coverage guard (spec §5, finding 12).
//
// It answers only for an sstate backend whose paired hashserv HOLDS ROWS: with no
// hashserv data the policy legitimately collapses to age-only retention, which is
// the correct behaviour for an rsync'd mirror or a BB_HASHSERVE=auto deployment and
// must not be refused. When there IS hashserv data and a bounded prefix of the
// backend's keys resolves NOTHING, the derivation is broken rather than the objects
// being garbage, and the sweep declines the whole backend.
func (e *Engine) sstateRefused(ctx context.Context, runID int64, p *backendPlan) (bool, error) {
	if p.kind != repository.BackendKindSstate || p.hashserv == 0 || !p.hasWindow {
		return false, nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	hasRows, err := e.db.HashservBackendHasUnihashes(stmtCtx, p.hashserv)

	cancel()

	if err != nil {
		return false, fmt.Errorf("probe paired hashserv backend %d: %w", p.hashserv, err)
	}

	if !hasRows {
		return false, nil
	}

	probe := coverage{scanned: 0, resolved: 0}
	decide := e.sstateDecider(p, time.Time{}, false, &probe)
	afterKey := ""

	for range coverageProbePages {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		rows, err := e.scan(ctx, runID, p.id, nsDefault, afterKey)
		if err != nil {
			return false, err
		}

		if len(rows) == 0 {
			break
		}

		if _, err := decide(ctx, rows); err != nil {
			return false, err
		}

		if probe.resolved > 0 {
			return false, nil
		}

		afterKey = rows[len(rows)-1].Key

		if len(rows) < e.cfg.BatchSize {
			break
		}
	}

	if probe.scanned == 0 || probe.resolved > 0 {
		return false, nil
	}

	e.log.ErrorContext(ctx, "refusing to sweep sstate: no scanned key resolved to a unihash "+
		"while the paired hashserv backend holds rows -- the root derivation is broken, not the cache",
		slog.String("org", p.org), slog.String("project", p.project),
		slog.Int64("backend", p.id), slog.Int64("scanned", probe.scanned))

	return true, nil
}

// runStage is the chunk -> pause -> chunk pass over ONE namespace of ONE backend.
//
// The cursor is a KEYSET over cache_objects_pkey (backend_id, namespace, key), so a
// namespace is a contiguous range of that btree and the pass stops the moment a page
// crosses out of it. ctx is checked BETWEEN chunks and each statement carries its own
// timeout: a sweep legitimately runs for hours, so the deadline that means "something
// is wrong" is a per-chunk one.
func (e *Engine) runStage(
	ctx context.Context,
	runID int64,
	p *backendPlan,
	st stage,
	index int,
	now time.Time,
	decide decider,
	u *usage,
	sum *Summary,
) error {
	afterKey := ""

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		rows, err := e.scan(ctx, runID, p.id, st.namespace, afterKey)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			return nil
		}

		page := rows
		last := false

		for i, row := range rows {
			if row.Namespace != st.namespace {
				page, last = rows[:i], true

				break
			}
		}

		if len(page) > 0 {
			afterKey = page[len(page)-1].Key

			if err := e.processPage(ctx, p, st, index, now, decide, page, u, sum); err != nil {
				return err
			}
		}

		if last || len(rows) < e.cfg.BatchSize {
			return nil
		}

		if err := e.pause(ctx); err != nil {
			return err
		}
	}
}

// processPage decides, vetoes, deletes and accounts one page.
func (e *Engine) processPage(
	ctx context.Context,
	p *backendPlan,
	st stage,
	index int,
	now time.Time,
	decide decider,
	page []repository.ScanObjectsForGCRow,
	u *usage,
	sum *Summary,
) error {
	mask, err := decide(ctx, page)
	if err != nil {
		return err
	}

	batch := make([]blob.DeleteRef, 0, len(page))

	for i, row := range page {
		// THE VETO (spec §6.2). A pending, unflushed touch is INVISIBLE to the scan's
		// SELECT -- the row still carries its old accessed_at -- so a key this process
		// answered "present" for seconds ago can be selected as ninety days cold, and
		// FindMissingBlobs answering "present" is a RESERVATION the client will act on.
		// The only complete record of those reads is in this process's memory, and it is
		// complete only because GC refuses to run multi-instance.
		if !mask[i] || e.blobs.PendingTouch(p.id, row.Namespace, row.Key) {
			u.observe(index, row, now)

			continue
		}

		batch = append(batch, deleteRef(p, st, row))
	}

	if len(batch) == 0 {
		return nil
	}

	if sum.DryRun {
		sum.ObjectsDeleted += int64(len(batch))
		e.rec.ObjectsDeleted(p.backend, st.namespace, st.reason, int64(len(batch)))

		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	n, err := e.blobs.DeleteBatch(stmtCtx, batch)

	cancel()

	if err != nil {
		return fmt.Errorf("delete %s/%s chunk: %w", p.project, st.namespace, err)
	}

	sum.ObjectsDeleted += n
	e.rec.ObjectsDeleted(p.backend, st.namespace, st.reason, n)

	return nil
}

// deleteRef builds the DeleteRef for one doomed row. The digest travels from the
// scan rather than being re-read: DeleteObjectsByKeys orders its driving set by
// digest to keep every writer taking blobs row locks in one global order, and
// re-reading it would be a second round trip per chunk for a value we already have.
func deleteRef(p *backendPlan, st stage, row repository.ScanObjectsForGCRow) blob.DeleteRef {
	var d blob.Digest

	copy(d[:], row.Digest)

	return blob.DeleteRef{
		Ref: blob.Ref{
			BackendID: p.id,
			Org:       p.org,
			Project:   p.project,
			Backend:   p.backend,
			Kind:      st.kindLabel,
			Namespace: row.Namespace,
			Key:       row.Key,
		},
		Digest: d,
	}
}

// scan reads one page of the keyset cursor.
func (e *Engine) scan(
	ctx context.Context, runID, backendID int64, namespace, afterKey string,
) ([]repository.ScanObjectsForGCRow, error) {
	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	rows, err := e.db.ScanObjectsForGC(stmtCtx, repository.ScanObjectsForGCParams{
		BackendID:      backendID,
		AfterNamespace: namespace,
		AfterKey:       afterKey,
		ScanLimit:      int32(e.cfg.BatchSize), //nolint:gosec // BatchSize is a bounded config knob
		RunID:          runID,
	})
	if err != nil {
		return nil, fmt.Errorf("scan backend %d namespace %q: %w", backendID, namespace, err)
	}

	return rows, nil
}

// evictToQuota is quota-pressure eviction, and it runs INSIDE the staged pass: the
// same stage order, exhausting each stage's candidates before touching its successor.
func (e *Engine) evictToQuota(
	ctx context.Context, runID int64, now time.Time, p *backendPlan, u *usage, sum *Summary,
) error {
	plan := planEviction(u, p.quota)
	if plan.empty() {
		return nil
	}

	e.log.WarnContext(ctx, "backend is over quota: evicting",
		slog.String("org", p.org), slog.String("project", p.project),
		slog.Int64("bytes", u.bytes), slog.Int64("quota", p.quota), slog.Bool("dry_run", sum.DryRun))

	evicted := newUsage(len(p.stages))

	for i, st := range p.stages {
		quotaStage := st
		quotaStage.reason = metrics.GCReasonQuota

		decide := rowRule(func(row repository.ScanObjectsForGCRow) bool {
			return plan.doomed(i, row, now)
		})

		if err := e.runStage(ctx, runID, p, quotaStage, i, now, decide, evicted, sum); err != nil {
			return err
		}
	}

	// Republish: the gauges must describe what is left, not what the retention pass
	// found before the cap took another bite out of it.
	e.publishUsage(ctx, *p, evicted, sum.DryRun)

	return nil
}

// publishUsage writes the measurement row and the gauges for one backend.
//
// A DRY RUN PUBLISHES NOTHING. Its numbers describe the corpus as it WOULD be after
// a sweep that is not going to happen, and a gauge that quietly answers a
// hypothetical is worse than one that is a few hours stale.
func (e *Engine) publishUsage(ctx context.Context, p backendPlan, u *usage, dryRun bool) {
	if dryRun {
		return
	}

	at := time.Now()

	e.rec.Usage(p.org, p.project, p.backend, u.objects, u.bytes, p.quota, at)
	e.noteTouchRamp(u)

	e.measuredMu.Lock()
	e.lastUsage[p.id] = snapshot{objects: u.objects, bytes: u.bytes, at: at}
	e.measuredMu.Unlock()

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	if err := e.db.UpsertBackendUsage(stmtCtx, repository.UpsertBackendUsageParams{
		BackendID: p.id, ObjectsCount: u.objects, LogicalBytes: u.bytes,
	}); err != nil {
		e.log.ErrorContext(ctx, "recording backend usage",
			slog.Int64("backend", p.id), slog.Any("error", err))

		return
	}

	e.measuredMu.Lock()
	e.measured[p.id] = time.Now()
	e.measuredMu.Unlock()
}

// republishCached re-publishes the last known usage of every backend this pass will
// not measure, immediately after the reset that dropped it.
func (e *Engine) republishCached(plans []backendPlan) {
	e.measuredMu.Lock()
	defer e.measuredMu.Unlock()

	for _, p := range plans {
		if len(p.stages) > 0 && p.managed() {
			continue
		}

		snap, ok := e.lastUsage[p.id]
		if !ok {
			continue
		}

		e.rec.Usage(p.org, p.project, p.backend, snap.objects, snap.bytes, p.quota, snap.at)
	}
}

// markLayerB is stage 10's mark: every unreferenced blob past the grace period,
// oldest first, in batches.
//
// It is halted by --gc-disable-retention along with Layer A (spec §9.6), because the
// incident that flag serves is "we deleted things we wanted" and those bytes are
// still sitting inside the grace window. Leaving the mark running would convert a
// recoverable window into permanent loss at maximum speed.
func (e *Engine) markLayerB(ctx context.Context, runID int64, sum *Summary) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		stmtCtx, cancel := chunkCtx(ctx)

		marked, err := e.db.MarkBlobsPendingDelete(stmtCtx, repository.MarkBlobsPendingDeleteParams{
			ID: runID, Limit: int32(e.cfg.BatchSize), //nolint:gosec // BatchSize is a bounded config knob
		})

		cancel()

		if err != nil {
			return fmt.Errorf("mark blobs pending delete: %w", err)
		}

		sum.BlobsMarked += int64(len(marked))
		e.rec.BlobsMarked(int64(len(marked)))

		if len(marked) < e.cfg.BatchSize {
			return nil
		}

		if err := e.pause(ctx); err != nil {
			return err
		}
	}
}

// reapPending drives every pending_delete blob through blob.Service.ReapDigest.
//
// It is stage 0 (crash re-drive) and the tail of stage 10, and it is the same loop
// both times: a pending_delete row is durable precisely so a process that died
// between the mark and the unlink leaves work rather than orphaned bytes.
//
// TERMINATION is on PROGRESS, not on the queue emptying: a blob whose bytes cannot be
// unlinked stays pending and comes straight back on the next page, so a round that
// reaps nothing stops the loop instead of spinning on it forever.
func (e *Engine) reapPending(ctx context.Context, sum *Summary) error {
	var backlog int64

	for range reapRounds {
		if err := ctx.Err(); err != nil {
			return err
		}

		stmtCtx, cancel := chunkCtx(ctx)

		//nolint:gosec // BatchSize is a bounded config knob
		pending, err := e.db.ListPendingDeleteBlobs(stmtCtx, int32(e.cfg.BatchSize))

		cancel()

		if err != nil {
			return fmt.Errorf("list pending delete blobs: %w", err)
		}

		if len(pending) == 0 {
			e.rec.PendingBacklog(0)

			return nil
		}

		backlog = int64(len(pending))
		progress := int64(0)

		var lastErr error

		for _, row := range pending {
			var digest blob.Digest

			copy(digest[:], row.Digest)

			reapCtx, cancel := chunkCtx(ctx)
			reaped, err := e.blobs.ReapDigest(reapCtx, digest)

			cancel()

			switch {
			case err != nil:
				lastErr = err
			case reaped:
				progress++
				sum.BlobsDeleted++
				sum.BytesReclaimed += row.SizeBytes
				e.rec.BlobsReaped(1, row.SizeBytes)
			default:
				// Revived by a concurrent PUT, or already reaped. Both are normal outcomes
				// and both mean the row is gone from this queue.
				progress++
			}
		}

		if progress == 0 {
			e.rec.PendingBacklog(backlog)

			if lastErr != nil {
				return fmt.Errorf("reap pending blobs: %w", lastErr)
			}

			return nil
		}

		if lastErr != nil {
			e.log.WarnContext(ctx, "some blobs could not be reaped this round", slog.Any("error", lastErr))
		}

		if err := e.pause(ctx); err != nil {
			return err
		}
	}

	e.rec.PendingBacklog(backlog)

	return nil
}

// publishPhysical publishes the instance-wide live byte count.
func (e *Engine) publishPhysical(ctx context.Context) error {
	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	n, err := e.db.InstancePhysicalBytes(stmtCtx)
	if err != nil {
		return fmt.Errorf("read physical bytes: %w", err)
	}

	e.rec.PhysicalBytes(n)

	return nil
}
