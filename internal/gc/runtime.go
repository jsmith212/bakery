package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// THE RUNTIME (spec §9). Three background loops, all tied to the server's lifetime
// context and all started beside the existing key toucher and session cleaner.

const (
	// touchStalenessRamped is T while the corpus is still mostly untouched (spec
	// §6.4). 000012's fillfactor change is CATALOG-ONLY and moves no existing tuple,
	// so the FIRST touch of every pre-existing row is a non-HOT update -- a new tuple
	// plus an entry in every index. Opening at 24h spreads that one-time
	// index-bloat-and-WAL spike over days instead of concentrating it in the first
	// hour of the first sweep.
	touchStalenessRamped = 24 * time.Hour

	// rampThresholdPermille is where T tightens from touchStalenessRamped to the
	// configured steady value: when fewer than half the scanned rows still have a NULL
	// accessed_at.
	//
	// THE SPEC SAYS "24h FOR THE FIRST 7 DAYS AFTER MIGRATION". This is that rule,
	// driven by the state it is a proxy for instead of by a wall clock, and the reason
	// is that the wall clock is not available: golang-migrate records a version and a
	// dirty flag, no timestamp, and neither 000012 nor any table carries one either.
	// The alternatives were a new schema marker (the migration has landed) or the
	// process's own uptime (a weekly-restarting deployment would never tighten). The
	// NULL fraction is measured for free by a pass that already reads accessed_at, it
	// falls monotonically as the toucher does its work, and it answers the question
	// the 7 days were standing in for: "are we still paying for the first touch of
	// pre-existing rows?"
	rampThresholdPermille = 500

	// usageStartupDelay is how long the usage pass waits before its first measurement.
	// Boot is the busiest moment in the process's life -- migrations have just run,
	// every route cache is cold -- and a full scan of every backend is not what it
	// needs from its first second.
	usageStartupDelay = 30 * time.Second
)

// Loop runs a sweep every --gc-interval until ctx is cancelled.
//
// IT REFUSES TO START UNDER --allow-multi-instance, loudly (spec §9.4). All three
// reasons are load-bearing: the LRU invalidation DeleteBatch performs is
// process-local, the boot reaper below would fail another instance's live sweep, and
// §6.2's pending-touch veto is only sound because this process's record of unflushed
// reads is complete. None of them degrades gracefully -- each one silently deletes
// data another instance is still serving.
func (e *Engine) Loop(ctx context.Context) {
	switch {
	case !e.cfg.Enabled:
		e.log.WarnContext(ctx, "gc is disabled: nothing is ever swept, and cached objects "+
			"and blobs accumulate without bound")

		return

	case e.cfg.AllowMultiInstance:
		e.log.WarnContext(ctx, "gc will NOT run: --allow-multi-instance is set. The sweep's "+
			"LRU invalidation, its boot reaper and its pending-touch veto are all "+
			"process-local, and each of them silently deletes data another instance is "+
			"still serving. Retention, quota eviction and byte reclamation are all off.")

		return
	}

	e.log.InfoContext(ctx, "gc loop started",
		slog.Duration("interval", e.cfg.Interval),
		slog.Duration("grace_period", e.cfg.GracePeriod),
		slog.Bool("retention", !e.cfg.DisableRetention))

	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A sweep that loses its context finishes the CHUNK it is in, writes its
			// terminal gc_runs row under context.WithoutCancel, and returns; the error
			// here is that shutdown, not a failure to report twice.
			if _, err := e.Run(ctx, TriggerInterval, false); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, ErrAlreadyRunning) {
				e.log.ErrorContext(ctx, "gc run failed", slog.Any("error", err))
			}
		}
	}
}

// UsageLoop measures every backend's usage on --gc-usage-interval.
//
// IT IS DECOUPLED FROM RETENTION (spec §8, findings 7b/13). An operator who has
// turned retention off, or who has left every window NULL, still needs to know how
// much each project is storing -- and a quota badge that goes stale the moment
// somebody sets --gc-disable-retention is a dashboard that lies during exactly the
// incident it exists for. It skips a backend a sweep has already measured within the
// interval, so the two passes do not scan the same rows twice.
func (e *Engine) UsageLoop(ctx context.Context) {
	if !e.cfg.Enabled {
		return
	}

	first := time.NewTimer(usageStartupDelay)
	defer first.Stop()

	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}

	t := time.NewTicker(e.cfg.UsageInterval)
	defer t.Stop()

	for {
		if err := e.MeasureUsage(ctx); err != nil && !errors.Is(err, context.Canceled) {
			e.log.ErrorContext(ctx, "usage measurement failed", slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// MeasureUsage is one lightweight pass: count and size every backend's rows, publish
// the gauges, and record the measurement.
//
// It reads through the SAME keyset cursor the sweep uses but decides nothing and
// deletes nothing. It does not open a gc_runs row: there is no run here, and a
// measurement that occupied the active slot would block the real sweep behind a
// bookkeeping pass.
//
// ScanObjectsForGC needs a run id for the write barrier's frozen snapshot, so this
// pass borrows the most recent one by starting -- and immediately finishing -- a
// dry run. That keeps CLAUDE.md's rule ("never select snapshot back into Go") intact
// and costs one row per pass, which is also the audit trail for when a measurement
// happened.
func (e *Engine) MeasureUsage(ctx context.Context) error {
	rows, err := e.db.ListBackendsForGC(ctx)
	if err != nil {
		return fmt.Errorf("list backends: %w", err)
	}

	plans := buildPlans(rows)
	cutoff := time.Now().Add(-e.cfg.UsageInterval)

	pending := make([]backendPlan, 0, len(plans))

	e.measuredMu.Lock()

	for _, p := range plans {
		if len(p.stages) == 0 {
			continue
		}

		if at, ok := e.measured[p.id]; ok && at.After(cutoff) {
			continue
		}

		pending = append(pending, p)
	}

	e.measuredMu.Unlock()

	if len(pending) == 0 {
		return e.publishPhysical(ctx)
	}

	run, err := e.db.StartGCRun(ctx, repository.StartGCRunParams{
		GracePeriod: interval(e.cfg.GracePeriod), Trigger: string(TriggerInterval), DryRun: true,
	})
	if err != nil {
		return fmt.Errorf("start usage measurement run: %w", err)
	}

	sum := Summary{
		RunID: run.ID, Trigger: TriggerInterval, DryRun: true,
		ObjectsDeleted: 0, HashservRows: 0, BlobsMarked: 0, BlobsDeleted: 0,
		BytesReclaimed: 0, BackendsRefused: 0,
	}

	measureErr := e.measureAll(ctx, run, pending, &sum)

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	status := repository.GcRunStatusSucceeded
	msg := ""

	if measureErr != nil {
		status, msg = repository.GcRunStatusFailed, runError(measureErr)
	}

	e.finish(finishCtx, run.ID, status, msg, sum)

	if measureErr != nil {
		return measureErr
	}

	return e.publishPhysical(ctx)
}

// measureAll runs the read-only accounting pass over the backends that need it.
func (e *Engine) measureAll(
	ctx context.Context, run repository.StartGCRunRow, plans []backendPlan, sum *Summary,
) error {
	now := run.StartedAt.Time

	// No ResetUsage here, deliberately: this pass measures a SUBSET (it skips
	// backends a sweep just measured), and resetting the vecs would drop the skipped
	// backends' series until something re-published them. The full sweep resets,
	// because it covers every backend and is therefore the only pass that can tell a
	// vanished backend from an unvisited one.
	for _, p := range plans {
		u := newUsage(len(p.stages))

		for i, st := range p.stages {
			keep := rowRule(func(_ repository.ScanObjectsForGCRow) bool { return false })

			if err := e.runStage(ctx, run.ID, &p, st, i, now, keep, u, sum); err != nil {
				return err
			}
		}

		e.publishUsage(ctx, p, u, false)
	}

	return nil
}

// noteTouchRamp folds one pass's NULL-accessed_at fraction into the ramp signal.
func (e *Engine) noteTouchRamp(u *usage) {
	if u.objects == 0 {
		return
	}

	e.nullPermille.Store(u.nullAccessed * 1000 / u.objects)
}

// TouchInterval is F for blob.Service.StartAccessToucher, normalized.
func (e *Engine) TouchInterval() time.Duration { return e.cfg.TouchInterval }

// TouchStaleness is T for blob.Service.StartAccessToucher, and it RAMPS (spec §6.4).
//
// Pass it as the staleness function: the toucher asks on every tick, so the ramp
// tightens the moment a measurement says the corpus has been touched, with no
// restart.
func (e *Engine) TouchStaleness() time.Duration {
	if e.nullPermille.Load() > rampThresholdPermille {
		return max(touchStalenessRamped, e.cfg.TouchStaleness)
	}

	return e.cfg.TouchStaleness
}

// ReapOrphanedRuns marks every gc_runs row still in 'running' as failed.
//
// CALL IT ONLY WHEN THIS PROCESS ACTUALLY HOLDS THE BOOT ADVISORY LOCK (spec §9.3,
// finding 4). The gate is the LOCK, not the --allow-multi-instance flag: a boot that
// merely passed the flag may be starting alongside a healthy instance whose sweep is
// live, and failing that run would both lie in the audit trail and free the active
// slot for a second concurrent sweep. A process that holds the lock has already
// proven no other instance is writing, so anything still 'running' is its own
// predecessor's crash.
func (e *Engine) ReapOrphanedRuns(ctx context.Context) error {
	n, err := e.db.MarkOrphanedGCRunsFailed(ctx)
	if err != nil {
		return fmt.Errorf("reap orphaned gc runs: %w", err)
	}

	if n > 0 {
		e.log.WarnContext(ctx, "marked orphaned gc runs failed: a previous process died mid-sweep",
			slog.Int64("runs", n))
	}

	return nil
}

// RedrivePendingDelete drains the pending_delete queue once, in the background, at
// boot (spec §9.3).
//
// It is not the same work as the first scheduled sweep and must not wait for it: a
// process that died between the mark and the unlink left durable tombstones whose
// bytes are still on disk and whose blob rows already read as gone. --gc-interval is
// six hours; that is six hours of unreclaimed disk for work that is already decided.
func (e *Engine) RedrivePendingDelete(ctx context.Context) {
	if !e.cfg.Enabled || e.cfg.AllowMultiInstance {
		return
	}

	sum := Summary{
		RunID: 0, Trigger: TriggerInterval, DryRun: false,
		ObjectsDeleted: 0, HashservRows: 0, BlobsMarked: 0, BlobsDeleted: 0,
		BytesReclaimed: 0, BackendsRefused: 0,
	}

	if err := e.reapPending(ctx, &sum); err != nil {
		if !errors.Is(err, context.Canceled) {
			e.log.ErrorContext(ctx, "boot re-drive of pending deletes failed", slog.Any("error", err))
		}

		return
	}

	if sum.BlobsDeleted > 0 {
		e.log.InfoContext(ctx, "re-drove pending deletes left by a previous process",
			slog.Int64("blobs", sum.BlobsDeleted), slog.Int64("bytes", sum.BytesReclaimed))
	}
}

// text renders an optional error message for gc_runs.error. An empty message is
// NULL, not the empty string: a run that succeeded has no error, and "" would read
// as one in every query that tests the column for presence.
func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{String: "", Valid: false}
	}

	return pgtype.Text{String: s, Valid: true}
}
