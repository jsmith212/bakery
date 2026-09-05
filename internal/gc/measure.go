package gc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// PER-PROJECT USAGE MEASUREMENT, ON READ.
//
// # The problem
//
// Every objects/size figure in the console comes from cache_backend_usage, and until
// now its only writer was the periodic pass on --gc-usage-interval, whose default was
// SIX HOURS. To anyone using the console the numbers simply never moved: create a
// backend, push a gigabyte, and the dashboard says "not yet measured" for the rest of
// the working day. The figure was correct and useless.
//
// # The design that was rejected
//
// A live counter -- a trigger on cache_objects maintaining a per-backend total -- is
// exact, instant, and would put a row-lock convoy on the single hottest write path in
// the product. Every sstate PUT, every CAS upload and every /ac overwrite would
// serialise behind one row per backend, under a BB_NUMBER_THREADS-parallel storm. It
// is the same pattern the accessed_at toucher exists to avoid (marks coalesced in
// memory, one batched UPDATE per tick, precisely so a hot key is not written on every
// read), and adopting it for a DASHBOARD FIGURE would be trading the product's
// throughput for a number nobody reads at write time.
//
// # What this does instead
//
// Measure on READ, when the answer on file is stale, and never more often than the
// caller's freshness threshold. MeasureProjectUsage is one grouped aggregate per
// backend -- no keyset pages, no inter-chunk pause, and no gc_runs row, because a
// measurement decides nothing and so needs no write barrier. A per-project
// singleflight collapses the fan-out of several people opening the same dashboard at
// once into a single query, which is exactly the shape of the load this path sees.
//
// The periodic pass stays as the BACKSTOP, and its default tightens from six hours to
// one: it is what keeps the Prometheus gauges and a project nobody is looking at from
// drifting, and read-triggered measurement cannot cover either.

// measureTimeout bounds ONE read-triggered measurement, and it is the reason this is
// safe to put on a GET at all.
//
// A count over a ten-million-row backend is an index scan, and on a cold instance it
// can take seconds. Past this, the caller serves the STALE row unchanged -- which is
// not a degradation, because measured_at travels with every figure and says exactly
// how old it is. Blocking a dashboard indefinitely on a measurement would be the
// worse answer to the same question.
const measureTimeout = 10 * time.Second

// MeasureProject measures ONE project's backends and writes their usage rows.
//
// SINGLEFLIGHT, KEYED ON THE PROJECT. Several people opening the same project's
// overview in the same second is the ordinary load here -- and so is one person's
// browser firing the overview, the backends index and the backend detail page's three
// parallel loads. Without collapsing them, the freshness gate's "the row is stale"
// answer is true for all of them simultaneously and every one issues the same
// aggregate. With it, one runs and the rest wait for its result.
//
// A caller whose OWN context dies while sharing another's flight gets its context
// error back; the flight itself keeps running under the leader's context, which is
// correct -- the measurement is worth finishing for whoever is still waiting, and the
// row it writes is worth having regardless.
func (e *Engine) MeasureProject(ctx context.Context, projectID pgtype.UUID) error {
	key := projectID.String()

	ch := e.measureFlight.DoChan(key, func() (any, error) {
		// The flight's own bounded context, derived from the ENGINE LIFETIME rather than
		// from the request that happened to start it: a shared flight must not be
		// cancelled because the first browser tab that asked navigated away, and it must
		// not outlive the process either.
		flightCtx, cancel := context.WithTimeout(e.lifetime, measureTimeout)
		defer cancel()

		return nil, e.measureProjectNow(flightCtx, projectID)
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-ch:
		return res.Err
	}
}

// measureProjectNow is the measurement itself: one aggregate, then one upsert and one
// gauge publish per backend.
func (e *Engine) measureProjectNow(ctx context.Context, projectID pgtype.UUID) error {
	rows, err := e.db.MeasureProjectUsage(ctx, projectID)
	if err != nil {
		return fmt.Errorf("measure project usage: %w", err)
	}

	at := time.Now()

	for _, r := range rows {
		if err := e.db.UpsertBackendUsage(ctx, repository.UpsertBackendUsageParams{
			BackendID: r.BackendID, ObjectsCount: r.ObjectsCount, LogicalBytes: r.LogicalBytes,
		}); err != nil {
			return fmt.Errorf("record usage for backend %d: %w", r.BackendID, err)
		}

		var quota int64
		if r.QuotaBytes.Valid {
			quota = r.QuotaBytes.Int64
		}

		// The SAME gauges the sweep and the periodic pass publish, so a read-triggered
		// measurement keeps Prometheus in step with the console instead of leaving the
		// two to disagree about the same backend.
		e.rec.Usage(r.OrgSlug, r.ProjectSlug, backendOf(r.Kind), r.ObjectsCount, r.LogicalBytes, quota, at)

		// And the SAME bookkeeping: `measured` is what lets the periodic pass skip a
		// backend somebody's dashboard just measured, and `lastUsage` is what
		// republishes it after the sweep's gauge reset. Skipping either would make a
		// read-triggered measurement invisible to both.
		e.measuredMu.Lock()
		e.measured[r.BackendID] = at
		e.lastUsage[r.BackendID] = snapshot{objects: r.ObjectsCount, bytes: r.LogicalBytes, at: at}
		e.measuredMu.Unlock()
	}

	e.log.DebugContext(ctx, "measured a project's usage on read",
		slog.Int("backends", len(rows)))

	return nil
}
