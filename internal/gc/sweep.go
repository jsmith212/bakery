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

	// LogicalBytesFreed is a RUNNING TOTAL, across every backend this run has
	// swept so far, of size_bytes for cache_objects rows this run has deleted --
	// the SPA->API wiring wave's B7 attribution (000013, gc_run_backends).
	//
	// It is NOT BytesReclaimed. BytesReclaimed is Layer B's PHYSICAL reap --
	// instance-wide, deduped, delayed behind the grace period -- and cannot be
	// attributed to one backend at all: two backends can name the same blob, and
	// the byte only actually leaves disk when the last namer's row is gone.
	// LogicalBytesFreed is the SAME "logical" convention cache_backend_usage
	// (000012) and the quota histogram already use: full charge, to every
	// backend whose row named the bytes, the instant that row is deleted --
	// which is knowable immediately, from the scan the delete already read.
	//
	// publishRunBackend reads a BEFORE/AFTER delta of this field around one
	// backend's own sweepBackend/sweepHashserv call to get that backend's share,
	// which is sound because the sweep visits backends strictly one at a time
	// (spec CLAUDE.md: GC is single-writer by construction).
	//
	// stage 8 (SweepUnreferencedManifests, sweepManifests in this file) does NOT
	// add to this: that statement deletes without reading size_bytes back (its
	// own comment explains why touching its write-barrier-critical RETURNING
	// clause for a reporting figure is not worth the risk), so an OCI backend's
	// manifest deletions are undercounted here BY DESIGN. Manifests are small
	// JSON documents; the OCI blob layers they name go through the ordinary
	// per-page path below and ARE counted.
	LogicalBytesFreed int64
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
	if err := e.mayRun(); err != nil {
		return Summary{}, err //nolint:exhaustruct // a refused run has no results
	}

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
// SO ARE THE DISABLED AND MULTI-INSTANCE REFUSALS (R7#1). Loop's refusal is not
// the invariant it looks like: this method is reachable from an HTTP handler
// without going through Loop at all, so before this gate an operator running
// --allow-multi-instance could start by hand precisely the sweep the loop refuses
// to schedule -- process-local LRU invalidation against another instance's cache,
// a boot reaper that fails a live run, and a pending-touch veto that cannot see
// the other instance's reads.
//
// The sweep itself runs under the ENGINE'S LIFETIME context, not the request's and
// not context.WithoutCancel(request) (R7#8). The request's context dies with the
// response -- on some servers the instant this function returns -- so it cannot
// carry the sweep; WithoutCancel fixed that and broke shutdown, producing a sweep
// nothing could cancel and nothing waited for, still deleting rows through a pool
// that was closing. The lifetime context gives both: cancellable at shutdown,
// deaf to the client hanging up. asyncDone() is how Boot waits for the terminal
// FinishGCRun that cancellation triggers.
func (e *Engine) TriggerAsync(ctx context.Context, trigger Trigger, dryRun bool) (int64, error) {
	if err := e.mayRun(); err != nil {
		return 0, err
	}

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

	bg := e.lifetime

	e.async.Add(1)

	go func() {
		defer e.async.Done()

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

// mayRun is the configuration gate both entry points share (R7#1): a sweep is
// refused outright when GC is disabled or when this process cannot prove it is the
// only writer. Keeping it in ONE place is the point -- two guards that could
// disagree is how the loop came to refuse what the API happily started.
func (e *Engine) mayRun() error {
	switch {
	case !e.cfg.Enabled:
		return ErrDisabled
	case e.cfg.AllowMultiInstance:
		return ErrMultiInstance
	default:
		return nil
	}
}

// AsyncDone reports when every detached (TriggerAsync) sweep has finished,
// including the terminal gc_runs write each one makes under context.WithoutCancel
// after the lifetime context is cancelled.
//
// CALL IT AFTER THE LISTENERS HAVE DRAINED, never before: it snapshots the wait
// group as it stands, so a channel taken while nothing is in flight closes
// immediately and would wait for nothing at all. Boot takes it once its
// server.Run has returned, which is the point after which no new HTTP request can
// trigger one.
func (e *Engine) AsyncDone() <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		e.async.Wait()
	}()

	return done
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
		BytesReclaimed: 0, BackendsRefused: 0, LogicalBytesFreed: 0,
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
	if err := e.flushUnihashMarks(ctx, sum.DryRun); err != nil {
		return err
	}

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

	if err := e.flushAccessMarks(ctx, sum.DryRun); err != nil {
		return err
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

// flushUnihashMarks is §6.2 mechanism 1 for the GC ROOT, and it runs IMMEDIATELY
// BEFORE STAGE 1 (A: R6#1/R7#3).
//
// WHY BEFORE, AND WHAT WINDOW IS LEFT. hashserv marks a unihash read in memory and
// writes accessed_at at most once per T (--gc-touch-staleness, 1h steady, 24h
// ramped); until that write lands, SweepUnihashes' `coalesce(accessed_at,
// created_at)` sees only the OLD value, so a unihash a build read minutes ago is
// selected as ninety days cold -- and deleting it is not a cache miss, it is the
// GC root going away under the sstate objects it names. Stage 1 is a single
// self-contained SQL DELETE: there is no Go-side pass over its candidate rows, so
// the per-chunk PendingTouch veto that protects the cache_objects stages has no
// equivalent here and cannot be given one without reading every candidate back
// into Go. Forcing the flush first replaces an up-to-T exposure with one bounded
// by the STAGE'S OWN DURATION: a mark that arrives after this returns and before
// the DELETE commits is still invisible. That residual is seconds to minutes
// against windows of ninety days, and it is the same shape the spec accepts for
// the cache_objects stages.
//
// A FAILURE FAILS THE RUN. It is a database error like any other, and the
// alternative -- sweep anyway -- is precisely "delete the GC root on the strength
// of a read record we know we did not write".
//
// A DRY RUN FLUSHES NOTHING: it is genuinely read-only (spec §9.7), and
// accessed_at is a write. Its counts are therefore computed against the same
// slightly-stale accessed_at a sweep would have seen without this mechanism, which
// is the conservative direction for a report (it over-reports what would be
// deleted, never under-reports what would survive).
func (e *Engine) flushUnihashMarks(ctx context.Context, dryRun bool) error {
	if dryRun || e.unihash == nil {
		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	rows, err := e.unihash.FlushNow(stmtCtx)
	if err != nil {
		return fmt.Errorf("pre-sweep unihash accessed_at flush: %w", err)
	}

	if rows > 0 {
		e.log.InfoContext(ctx, "flushed pending unihash reads before the hashserv sweep",
			slog.Int64("rows", rows))
	}

	return nil
}

// flushAccessMarks is flushUnihashMarks' cache_objects twin and runs ONCE per run,
// immediately before the first cache_objects stage.
//
// Once per run, not once per stage: the flusher's unit is the whole pending set,
// not a namespace, so a second call a stage later would find whatever arrived in
// between -- exactly the residual window this cannot close -- at the cost of a
// round trip per stage per backend. The per-chunk PendingTouch veto (§6.2
// mechanism 2) is what covers that residual on this side, which is why the
// cache_objects stages can afford one flush where stage 1 could not.
func (e *Engine) flushAccessMarks(ctx context.Context, dryRun bool) error {
	if dryRun || e.access == nil {
		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	rows, err := e.access.FlushAccessNow(stmtCtx)
	if err != nil {
		return fmt.Errorf("pre-sweep accessed_at flush: %w", err)
	}

	if rows > 0 {
		e.log.InfoContext(ctx, "flushed pending object reads before the retention stages",
			slog.Int64("rows", rows))
	}

	return nil
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

	// B7 (000013): hashserv owns no cache_objects rows and no blobs, so its
	// objects_deleted is unihashes+outhashes -- the same figure HashservRows
	// above just accumulated -- and bytes_freed is always zero, structurally: a
	// unihash row has no size_bytes column to sum. This is reached only past the
	// !p.hasWindow guard at the top of this function, so a hashserv backend with
	// no configured window (nothing for this run to have swept) gets no row,
	// exactly like a cache_objects backend the plan declined.
	e.publishRunBackend(ctx, runID, p.id, unihashes+outhashes, 0, sum.DryRun)

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

		// A REFUSED BACKEND MUST NOT VANISH FROM THE GAUGES (P: R6#8). The reset above
		// dropped every storage series; republishCached deliberately skipped this
		// backend because a managed backend with stages is one the sweep was ABOUT to
		// measure. Returning here without republishing makes a broken-derivation sstate
		// backend disappear from bakery_storage_objects/_bytes until the next
		// --gc-usage-interval pass -- so the one backend whose retention has visibly
		// stopped working is also the one whose size nobody can see.
		e.republishOne(p)

		return nil
	}

	// B7 (000013): snapshot the run-wide counters BEFORE this backend's own
	// stages and quota eviction run, so the delta below is exactly this
	// backend's share. Sound because the sweep visits backends strictly one at a
	// time -- there is no concurrent sweepBackend call whose writes could land
	// between this read and the one after evictToQuota returns.
	beforeObjects, beforeBytes := sum.ObjectsDeleted, sum.LogicalBytesFreed

	u := newUsage(len(p.stages))
	cov := coverage{scanned: 0, resolved: 0}

	for i, st := range p.stages {
		// STAGE 8 DELETES IN SQL, BEFORE ITS ACCOUNTING SCAN (E: R6#4/R7#9). The
		// tag/manifest anti-join and the write barrier are evaluated together, at delete
		// time, by SweepUnreferencedManifests; the scan that follows then measures what
		// actually survived rather than what a Go-side join predicted.
		// p.hasWindow GUARDS THE RETENTION FLAVOR: with no configured window the
		// stage window is 0, which would reach SQL as `< now()` and delete every
		// untagged manifest -- NULL means retain forever, here as everywhere. The
		// quota flavor (evictToQuota -> evictManifests) deliberately has no such
		// guard: a cap is a cap. This managed()-but-windowless shape is real: an
		// OCI backend with quota_bytes set and retention_window NULL.
		if st.namespace == nsManifests && p.hasWindow {
			if err := e.sweepManifests(ctx, runID, &p, st.window, metrics.GCReasonRetention, sum); err != nil {
				return err
			}
		}

		decide := e.deciderFor(&p, st, now, &cov)

		if err := e.runStage(ctx, runID, &p, st, i, now, decide, u, sum); err != nil {
			return err
		}
	}

	if p.kind == repository.BackendKindSstate {
		e.rec.SstateCoverage(p.org, p.project, cov.fraction())
	}

	e.publishUsage(ctx, p, u, sum.DryRun)

	evictErr := e.evictToQuota(ctx, runID, now, &p, u, sum)

	// Published REGARDLESS of evictErr, mirroring finish()'s own posture on the
	// run as a whole: a partial figure ("how far this backend got before it
	// failed") is more useful to an org viewer than none, and the run itself
	// still ends up FAILED (runFrom propagates evictErr up), which is the loud
	// signal.
	e.publishRunBackend(ctx, runID, p.id, sum.ObjectsDeleted-beforeObjects, sum.LogicalBytesFreed-beforeBytes, sum.DryRun)

	return evictErr
}

// publishRunBackend writes B7's gc_run_backends row (000013) for the ONE backend
// sweepBackend or sweepHashserv just finished sweeping -- best-effort, exactly
// like publishUsage immediately above: logged and swallowed on failure rather
// than failing an otherwise-successful run over a reporting write.
//
// A DRY RUN WRITES NOTHING, for the same reason publishUsage's own dry-run guard
// exists: the figures describe a sweep that is not going to happen. A
// usage-only run (MeasureUsage) never reaches this function at all -- it drives
// its own read-only measureAll loop and calls neither sweepBackend nor
// sweepHashserv -- so "real run only" falls out of WHO CALLS THIS rather than a
// second trigger check here.
func (e *Engine) publishRunBackend(
	ctx context.Context, runID, backendID, objectsDeleted, bytesFreed int64, dryRun bool,
) {
	if dryRun {
		return
	}

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	if err := e.db.RecordGCRunBackend(stmtCtx, repository.RecordGCRunBackendParams{
		RunID: runID, BackendID: backendID,
		ObjectsDeleted: objectsDeleted, BytesFreed: bytesFreed,
	}); err != nil {
		e.log.ErrorContext(ctx, "recording gc run backend activity",
			slog.Int64("run", runID), slog.Int64("backend", backendID), slog.Any("error", err))
	}
}

// decider decides which rows of one scan page are doomed. It is per PAGE rather than
// per row because the sstate rule needs a batch existence probe: one round trip per
// page instead of one per key.
type decider func(ctx context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error)

// deciderFor builds a stage's retention rule. Each is spec §3's third predicate --
// the write barrier's two halves are already applied by ScanObjectsForGC.
func (e *Engine) deciderFor(p *backendPlan, st stage, now time.Time, cov *coverage) decider {
	cutoff := now.Add(-st.window)
	live := st.window > 0 && p.hasWindow

	switch {
	case p.kind == repository.BackendKindSstate:
		// SPEC §5'S CONJUNCTIVE RULE: dead iff the window elapsed AND no surviving
		// unihash names it. The reachability half is a page-level probe, so it is
		// composed rather than inlined -- quota eviction ANDs the SAME half against a
		// different age rule (R6#9), and two copies of "is this object reachable" is
		// exactly how the two would come to disagree.
		return conjunction(e.sstateUnreachable(p, cov), func(row repository.ScanObjectsForGCRow) bool {
			return live && livenessOf(row).Before(cutoff)
		})

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
		return rowRule(func(row repository.ScanObjectsForGCRow) bool {
			return live && row.UpdatedAt.Time.Before(cutoff)
		})

	case st.namespace == nsManifests:
		// ACCOUNTING ONLY (E: R6#4/R7#9). Stage 8's deletions are SweepUnreferencedManifests'
		// alone: the anti-join has to see the tags namespace as it stands at DELETE time,
		// and a Go-side decider can only see it as it stood when the tags stage scanned
		// past. This pass runs AFTER that statement, so every row it sees is a survivor
		// and the mask is uniformly false -- it exists to fill the usage counters and the
		// quota histogram, nothing else.
		return rowRule(func(_ repository.ScanObjectsForGCRow) bool { return false })

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

// conjunction ANDs a page-level mask with a per-row rule, in that order: the mask
// is computed first (it may cost a round trip), the rule is free.
//
// It is what keeps "is this row still reachable" a SINGLE implementation shared by
// the retention rule and the quota rule (R6#9). A cap ignores the retention window
// -- a cap is a cap -- but it must not ignore reachability: evicting an sstate
// object whose unihash is alive, or a manifest a tag names, is not shedding cold
// data, it is breaking a live build to make room.
func conjunction(mask decider, fn rule) decider {
	return func(ctx context.Context, rows []repository.ScanObjectsForGCRow) ([]bool, error) {
		out, err := mask(ctx, rows)
		if err != nil {
			return nil, err
		}

		for i, row := range rows {
			out[i] = out[i] && fn(row)
		}

		return out, nil
	}
}

// sstateUnreachable is spec §5's reachability half, on its own: true for a row that
// NO surviving unihash on the paired hashserv backend names.
//
// An unparseable key resolves to no unihash and is therefore UNREACHABLE -- it dies
// on age alone. That is only safe because every caller ANDs this with an age rule:
// a swspec object (do_populate_lic, empty arch fields) is legal and common, and
// treating it as immediately deletable rather than merely age-eligible would delete
// a live cache.
func (e *Engine) sstateUnreachable(p *backendPlan, cov *coverage) decider {
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

		for i := range rows {
			_, reachable := alive[derived[i]]

			if derived[i] != "" && reachable {
				cov.resolved++
			}

			mask[i] = !reachable
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
	decide := e.sstateUnreachable(p, &probe)
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

			if err := e.processPage(ctx, runID, p, st, index, now, decide, page, u, sum); err != nil {
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
	runID int64,
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

	// batchBytes is the LOGICAL size of the batch this page is about to request
	// deleted -- B7's gc_run_backends.bytes_freed (000013). It is summed from the
	// SAME scan that built batch, so it costs no extra round trip, and it is
	// credited on the REQUESTED set rather than re-derived from what DeleteBatch
	// actually removed: DeleteObjectsByKeys re-checks the write barrier at delete
	// time and can legitimately delete fewer than len(batch) rows (a key a
	// concurrent /ac/ overwrite just resurrected), and that gap is the same one
	// ScanObjectsForGC's own comment already accepts as costing nothing at the
	// call site. The ObjectsDeleted counter below is NOT relaxed the same way --
	// it uses the real DeleteBatch count, n -- so this is the one place the two
	// totals can disagree by the width of that race, which is bounded by a
	// single chunk and never by more.
	var batchBytes int64

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
		batchBytes += row.SizeBytes
	}

	if len(batch) == 0 {
		return nil
	}

	if sum.DryRun {
		sum.ObjectsDeleted += int64(len(batch))
		sum.LogicalBytesFreed += batchBytes
		e.rec.ObjectsDeleted(p.backend, st.namespace, st.reason, int64(len(batch)))

		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	n, err := e.blobs.DeleteBatch(stmtCtx, runID, batch)

	cancel()

	if err != nil {
		return fmt.Errorf("delete %s/%s chunk: %w", p.project, st.namespace, err)
	}

	sum.ObjectsDeleted += n
	sum.LogicalBytesFreed += batchBytes
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

// sweepManifests is stage 8's deletion, and the ONLY thing that deletes a manifest
// (E: R6#4/R7#9, R6#9).
//
// One statement does the write barrier, the age rule and the tag anti-join, all
// evaluated against the corpus AS IT STANDS WHEN THE DELETE RUNS. The Go-side
// tagDigests set it replaces was assembled while the TAGS stage scanned, pages
// earlier: it answered "was this digest tagged a moment ago", and a `docker pull`
// that writes or revalidates a tag in the gap -- the ordinary case, since SWR
// revalidation writes tags continuously -- left a live tag naming a manifest the
// sweep had already condemned.
//
// window is the stage's effective W for the retention pass and the eviction
// histogram's cutoff for the quota pass; a zero window means "every unreferenced
// manifest", which is what a fully-evicted stage asks for. reason is what the
// deletion is attributed to.
//
// THE LRU INVALIDATION IS NOT OPTIONAL. Manifests are served through blob.Get, so
// a surviving positive entry answers "present" for a row that no longer exists,
// and the byte reap then turns that into a permanent dangling-metadata 500. The
// keys come back from the statement precisely so this can happen.
func (e *Engine) sweepManifests(
	ctx context.Context,
	runID int64,
	p *backendPlan,
	window time.Duration,
	reason metrics.GCReason,
	sum *Summary,
) error {
	if window < 0 {
		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	defer cancel()

	if sum.DryRun {
		n, err := e.db.DryRunSweepUnreferencedManifests(stmtCtx,
			repository.DryRunSweepUnreferencedManifestsParams{
				BackendID: p.id, RetentionWindow: interval(window), RunID: runID,
			})
		if err != nil {
			return fmt.Errorf("dry-run sweep manifests for backend %d: %w", p.id, err)
		}

		sum.ObjectsDeleted += n
		e.rec.ObjectsDeleted(p.backend, nsManifests, reason, n)

		return nil
	}

	keys, err := e.db.SweepUnreferencedManifests(stmtCtx, repository.SweepUnreferencedManifestsParams{
		BackendID: p.id, RetentionWindow: interval(window), RunID: runID,
	})
	if err != nil {
		return fmt.Errorf("sweep manifests for backend %d: %w", p.id, err)
	}

	if len(keys) == 0 {
		return nil
	}

	e.blobs.InvalidateKeys(p.id, nsManifests, keys)

	sum.ObjectsDeleted += int64(len(keys))
	e.rec.ObjectsDeleted(p.backend, nsManifests, reason, int64(len(keys)))

	e.log.InfoContext(ctx, "swept unreferenced manifests",
		slog.String("org", p.org), slog.String("project", p.project),
		slog.Int("manifests", len(keys)), slog.String("reason", string(reason)))

	return nil
}

// evictToQuota is quota-pressure eviction, and it runs INSIDE the staged pass: the
// same stage order, exhausting each stage's candidates before touching its successor.
//
// IT IGNORES THE RETENTION WINDOW AND NOTHING ELSE (R6#9). A cap is a cap, so age
// alone decides WHICH rows go -- but the stage's own REACHABILITY rule still
// applies, composed in below: an sstate object whose unihash is alive and a
// manifest a tag names are not cold data to shed, they are the live cache. The
// eviction plan's histogram cutoff simply takes the place of the window.
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
	probe := coverage{scanned: 0, resolved: 0}

	for i, st := range p.stages {
		quotaStage := st
		quotaStage.reason = metrics.GCReasonQuota

		age := func(row repository.ScanObjectsForGCRow) bool { return plan.doomed(i, row, now) }

		var decide decider

		switch {
		case p.kind == repository.BackendKindSstate:
			// The reachability half is the SAME probe the retention pass uses; only the
			// age half is replaced. probe's coverage counters are thrown away here: the
			// gauge an operator reads is the retention pass's, over the whole scan.
			decide = conjunction(e.sstateUnreachable(p, &probe), age)

		case st.namespace == nsManifests:
			// The anti-join cannot be expressed as a Go-side row rule at all, so quota
			// eviction of manifests goes through the SAME statement retention does, with
			// the histogram cutoff standing in for the window (R6#9). A fully-evicted
			// stage passes a zero window, which selects every unreferenced manifest.
			if err := e.evictManifests(ctx, runID, p, plan, i, sum); err != nil {
				return err
			}

			decide = rowRule(func(_ repository.ScanObjectsForGCRow) bool { return false })

		default:
			decide = rowRule(age)
		}

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

// evictManifests translates one eviction plan into stage 8's window.
//
// A stage the plan takes ENTIRELY gets a zero window (every unreferenced manifest,
// regardless of age); the one PARTIAL stage gets the histogram's cutoff, which is
// exactly the age at which the plan stops freeing; a stage the plan does not reach
// is left alone. The tag anti-join is the statement's own, so this cannot evict a
// manifest a live tag names no matter how far the plan reaches.
func (e *Engine) evictManifests(
	ctx context.Context,
	runID int64,
	p *backendPlan,
	plan evictionPlan,
	index int,
	sum *Summary,
) error {
	switch {
	case index < plan.fullStages:
		return e.sweepManifests(ctx, runID, p, 0, metrics.GCReasonQuota, sum)
	case index == plan.partial:
		return e.sweepManifests(ctx, runID, p, plan.minAge, metrics.GCReasonQuota, sum)
	default:
		return nil
	}
}

// republishOne re-publishes one backend's last known measurement after the gauge
// reset, for a backend this pass will not measure.
func (e *Engine) republishOne(p backendPlan) {
	e.measuredMu.Lock()
	defer e.measuredMu.Unlock()

	snap, ok := e.lastUsage[p.id]
	if !ok {
		return
	}

	e.rec.Usage(p.org, p.project, p.backend, snap.objects, snap.bytes, p.quota, snap.at)
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
