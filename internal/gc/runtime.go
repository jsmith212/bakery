package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

	// usageStartupDelay is how long the usage pass waits before its first measurement.
	// Boot is the busiest moment in the process's life -- migrations have just run,
	// every route cache is cold -- and a full scan of every backend is not what it
	// needs from its first second.
	usageStartupDelay = 30 * time.Second

	// sweepStartupDelay is the CEILING on how long the sweep loop waits before its
	// FIRST run (R7#7), mirroring usageStartupDelay. Without it the first sweep of a
	// process's life happens one whole --gc-interval (six hours, shipped) after boot,
	// so a deployment that restarts more often than that never sweeps at all -- and
	// nothing in any metric says so, because a loop that has not run yet emits
	// exactly what a healthy idle one does.
	//
	// It is a ceiling rather than the delay itself: the real delay is
	// min(this, --gc-interval), jittered, so a test (or an operator) that configures
	// a one-minute interval does not wait half of it on a constant, and so a fleet
	// restarted together does not start every instance's first sweep in the same
	// second.
	sweepStartupDelay = 30 * time.Second
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

	first := e.firstSweepDelay()

	e.log.InfoContext(ctx, "gc loop started",
		slog.Duration("interval", e.cfg.Interval),
		slog.Duration("first_run_in", first),
		slog.Duration("grace_period", e.cfg.GracePeriod),
		slog.Bool("retention", !e.cfg.DisableRetention))

	// THE FIRST SWEEP DOES NOT WAIT A WHOLE INTERVAL (R7#7). A ticker alone means a
	// process that restarts more often than --gc-interval (six hours) never sweeps,
	// silently; the startup timer is what makes a boot a sweep opportunity rather
	// than a reset of the clock.
	startup := time.NewTimer(first)
	defer startup.Stop()

	select {
	case <-ctx.Done():
		return
	case <-startup.C:
		e.runOnce(ctx)
	}

	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.runOnce(ctx)
		}
	}
}

// runOnce is one scheduled sweep plus its error reporting.
//
// A sweep that loses its context finishes the CHUNK it is in, writes its terminal
// gc_runs row under context.WithoutCancel, and returns; the error here is that
// shutdown, not a failure to report twice. ErrAlreadyRunning is likewise ordinary
// -- an operator's API trigger can legitimately still be running when the tick
// lands.
func (e *Engine) runOnce(ctx context.Context) {
	if _, err := e.Run(ctx, TriggerInterval, false); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, ErrAlreadyRunning) {
		e.log.ErrorContext(ctx, "gc run failed", slog.Any("error", err))
	}
}

// firstSweepDelay is min(sweepStartupDelay, --gc-interval), jittered into
// [d/10, d/2].
//
// The jitter is not decoration: a fleet restarted together (a rolling deploy, a
// node pool replacement) would otherwise start every instance's first sweep in the
// same second, against the same database. The floor at d/10 keeps boot's busiest
// moment -- migrations just finished, every route cache is cold -- clear of a full
// scan of every backend, which is the same reason usageStartupDelay exists.
func (e *Engine) firstSweepDelay() time.Duration {
	d := min(sweepStartupDelay, e.cfg.Interval)
	if d <= 0 {
		return 0
	}

	span := d/2 - d/10
	if span <= 0 {
		return d / 10
	}

	//nolint:gosec // jitter, not a secret
	return d/10 + time.Duration(rand.Int64N(int64(span)))
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
//
// THAT ROW'S TRIGGER IS 'usage', NOT 'interval' (R7#12). It is minted every six
// hours forever, whether or not retention is enabled, and mislabelling it as an
// interval sweep would put a permanent stream of zero-delete rows in the list an
// operator reads to see what the sweep did. GET /api/v1/gc/runs filters them out
// unless asked.
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
		GracePeriod: interval(e.cfg.GracePeriod), Trigger: string(TriggerUsage), DryRun: true,
	})
	if err != nil {
		return fmt.Errorf("start usage measurement run: %w", err)
	}

	sum := Summary{
		RunID: run.ID, Trigger: TriggerUsage, DryRun: true,
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

// TouchInterval is F for blob.Service.StartAccessToucher, normalized.
func (e *Engine) TouchInterval() time.Duration { return e.cfg.TouchInterval }

// LoadTouchRamp reads gc_state.touch_ramp_until ONCE, at boot, and is what makes
// TouchStaleness a real clock (J: R6#7/R7#6/R8#5).
//
// It REPLACES the pre-review-fix proxy: a per-mille fraction of scanned rows whose
// accessed_at was still NULL, recomputed on every backend's usage publish and
// therefore last-backend-wins -- one small, cold backend measured last could hold
// the whole instance at the ramped T forever, and on a mostly-cold corpus (an
// archive, a downloads mirror) the fraction never converges at all. 000012 stamps
// a real timestamp from its own now(), which needs no coordination between
// backends and answers the actual question: how long since the fillfactor
// transition began.
//
// ONCE, not per tick: the value cannot change (nothing writes gc_state after the
// migration), so re-reading it on every flush would be a query per minute forever
// for an answer that is fixed at boot. A read failure is not fatal -- the caller
// logs it and the ramp stays at its conservative end (see TouchStaleness), which
// costs write amplification, never correctness.
func (e *Engine) LoadTouchRamp(ctx context.Context) error {
	until, err := e.db.GetGCState(ctx)
	if err != nil {
		return fmt.Errorf("read gc state: %w", err)
	}

	if !until.Valid {
		return nil
	}

	e.rampUntil.Store(until.Time.UnixNano())

	return nil
}

// TouchStaleness is T for blob.Service.StartAccessToucher and for hashserv's
// unihash toucher, and it RAMPS (spec §6.4).
//
// Pass it as the staleness FUNCTION, not a duration: the touchers ask on every
// tick, so the ramp tightens the moment gc_state.touch_ramp_until passes, with no
// restart and no second source of truth.
//
// A zero rampUntil -- LoadTouchRamp never ran, or its read failed -- resolves to
// the RAMPED end on purpose. Being wrong that way costs a longer staleness guard
// (fewer accessed_at writes than necessary, bounded by T against windows of days);
// being wrong the other way concentrates the one-time non-HOT first-touch of every
// pre-existing row into the first hour of the first sweep, which is the spike the
// ramp exists to spread.
func (e *Engine) TouchStaleness() time.Duration {
	until := e.rampUntil.Load()
	if until == 0 || time.Now().UnixNano() < until {
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
