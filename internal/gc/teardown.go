package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// THE TEARDOWN STAGE (migration 000018). It runs FIRST in every tick, before
// every retention and quota stage, and it is the only stage that has no age
// predicate at all.
//
// # What it is for
//
// cache_objects -> cache_backends is ON DELETE RESTRICT, and so are
// hashserv_unihashes and hashserv_outhashes. That is deliberate -- a cascade would
// drop object metadata without decrementing a single blob refcount, pinning the
// bytes forever -- and its consequence was that a backend that had ever served a
// build could not be deleted, ever. DELETE /backends/{kind} answered 409 forever;
// DELETE /projects/{project} inherited the same wall one level up and did not even
// map the error.
//
// The API's answer is a MARK (cache_backends.deleting_at) and nothing else. This is
// the machine that makes the mark come true: empty the backend through
// blob.Service.DeleteBatch, then drop the row RESTRICT can no longer refuse, then
// -- when a torn-down project has no backends left -- drop the project.
//
// # Why the emptying is not done in the HTTP request
//
// A five-thousand-object backend times out the request. A ten-million-object one
// holds one transaction across the whole delete, pinning a snapshot on the hottest
// table in the schema. And either would have to bypass DeleteBatch -- the only
// sanctioned deletion path (digest-ordered blob locks, both write-barrier halves
// re-derived at delete time, shard-grouped LRU invalidation) -- or reimplement it.
// Here, all of that is already true and already paced by --gc-batch-size /
// --gc-batch-pause.
//
// # NO AGE RULE, AND THEREFORE NO hasWindow GUARD
//
// Every other Layer-A stage carries `coalesce(accessed_at, created_at) < now() - W`
// and the p.hasWindow guard that stops a NULL window reaching SQL as `< now()`.
// This stage has neither, and that is not an oversight: there is no window to be
// NULL. "Delete everything this backend holds" is the operator's own instruction,
// not a retention policy, so `retention_window IS NULL` (retain forever) has nothing
// to say about it -- a downloads ARCHIVE an operator has explicitly asked to tear
// down is torn down.
//
// # No PendingTouch veto either
//
// The veto (§6.2 mechanism 2) exists so a key this process answered "present" for
// seconds ago is not swept as ninety days cold. A doomed backend has no such
// reservation to honour: its route already 404s, so any read that produced a pending
// mark is older than the mark on the backend itself, and vetoing on it would stall
// the teardown of a hot backend indefinitely. DeleteBatch still invalidates the LRU
// for every key it removes, which is the part that matters.
//
// # It runs under --gc-disable-retention
//
// The brake exists for "the retention policy deleted things we wanted". A teardown
// is not the retention policy: it is one explicit, named, per-backend instruction a
// human issued through the API, and leaving it stuck would mean a backend that
// cannot be deleted for as long as the brake is on -- the exact state 000018 exists
// to end. Layer B's mark stays halted, so the bytes still sit in the grace window
// and the brake still does what it is for.

// teardownChunk is the row count for the hashserv purge's LIMITed DELETE. It reuses
// --gc-batch-size rather than a knob of its own, for the same reason every other
// stage does: the pacing an operator tuned is the pacing they want.
func (e *Engine) teardownChunk() int32 {
	//nolint:gosec // BatchSize is a bounded config knob, normalized in New
	return int32(e.cfg.BatchSize)
}

// sweepTeardown empties and then removes every marked backend, and finishes every
// marked project whose last backend has gone.
//
// A DRY RUN DOES NOTHING HERE. Its numbers describe a sweep that will not happen,
// and a teardown is not a policy decision to report on -- it is work in progress
// whose only interesting state is "still going" or "done".
func (e *Engine) sweepTeardown(
	ctx context.Context, run repository.StartGCRunRow, sum *Summary,
) error {
	if sum.DryRun {
		return nil
	}

	stmtCtx, cancel := chunkCtx(ctx)
	rows, err := e.db.ListBackendsForTeardown(stmtCtx)

	cancel()

	if err != nil {
		return fmt.Errorf("list backends for teardown: %w", err)
	}

	for _, b := range rows {
		if err := e.tearDownBackend(ctx, run.ID, b, sum); err != nil {
			return err
		}
	}

	return e.finishTornDownProjects(ctx)
}

// tearDownBackend empties ONE marked backend and, if it succeeded, removes its row.
func (e *Engine) tearDownBackend(
	ctx context.Context, runID int64, b repository.ListBackendsForTeardownRow, sum *Summary,
) error {
	before := sum.ObjectsDeleted
	beforeBytes := sum.LogicalBytesFreed

	if err := e.purgeObjects(ctx, runID, b, sum); err != nil {
		return err
	}

	rows, err := e.purgeHashserv(ctx, b, sum)
	if err != nil {
		return err
	}

	// The same B7 activity row every other stage writes (000013): a teardown is a
	// deletion an org's own GC activity screen should account for, and attributing it
	// to the backend it emptied is the only place it can go -- the row is about to
	// stop existing, and gc_run_backends.backend_id is ON DELETE CASCADE, so the
	// history goes with it. That is correct rather than lossy: an activity row naming
	// a backend nobody can look up is noise.
	e.publishRunBackend(ctx, runID, b.ID,
		sum.ObjectsDeleted-before+rows, sum.LogicalBytesFreed-beforeBytes, false)

	return e.dropBackendRow(ctx, b, sum.ObjectsDeleted-before+rows)
}

// purgeObjects deletes every cache_objects row of one doomed backend, across EVERY
// namespace, in --gc-batch-size chunks through blob.Service.DeleteBatch.
//
// ONE CURSOR FOR ALL NAMESPACES. ScanObjectsForGC's keyset is over
// cache_objects_pkey's (backend_id, namespace, key), so starting from the ("", "")
// sentinel and following the last row of every page walks the backend's whole slice
// of that btree once -- there is no per-namespace stage here because there is no
// per-namespace RULE here. DeleteBatch requires one (backend, namespace) per call
// (ErrMixedDeleteBatch), so a page that spans a namespace boundary is split into
// runs and issued as several batches, which costs nothing: the boundary can be
// crossed at most once per namespace for the whole backend.
func (e *Engine) purgeObjects(
	ctx context.Context, runID int64, b repository.ListBackendsForTeardownRow, sum *Summary,
) error {
	backend := backendOf(b.Kind)
	afterNamespace, afterKey := nsDefault, ""

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		stmtCtx, cancel := chunkCtx(ctx)

		rows, err := e.db.ScanObjectsForGC(stmtCtx, repository.ScanObjectsForGCParams{
			BackendID:      b.ID,
			AfterNamespace: afterNamespace,
			AfterKey:       afterKey,
			ScanLimit:      e.teardownChunk(),
			RunID:          runID,
		})

		cancel()

		if err != nil {
			return fmt.Errorf("scan backend %d for teardown: %w", b.ID, err)
		}

		if len(rows) == 0 {
			return nil
		}

		last := rows[len(rows)-1]
		afterNamespace, afterKey = last.Namespace, last.Key

		if err := e.deletePage(ctx, runID, b, backend, rows, sum); err != nil {
			return err
		}

		if len(rows) < e.cfg.BatchSize {
			return nil
		}

		if err := e.pause(ctx); err != nil {
			return err
		}
	}
}

// deletePage issues one DeleteBatch per namespace run within a scanned page.
func (e *Engine) deletePage(
	ctx context.Context,
	runID int64,
	b repository.ListBackendsForTeardownRow,
	backend metrics.Backend,
	rows []repository.ScanObjectsForGCRow,
	sum *Summary,
) error {
	for start := 0; start < len(rows); {
		namespace := rows[start].Namespace

		end := start
		for end < len(rows) && rows[end].Namespace == namespace {
			end++
		}

		batch := make([]blob.DeleteRef, 0, end-start)

		var bytes int64

		for _, row := range rows[start:end] {
			var d blob.Digest

			copy(d[:], row.Digest)

			batch = append(batch, blob.DeleteRef{
				Ref: blob.Ref{
					BackendID: b.ID,
					Org:       b.OrgSlug,
					Project:   b.ProjectSlug,
					Backend:   backend,
					Kind:      teardownKindLabel(namespace),
					Namespace: namespace,
					Key:       row.Key,
				},
				Digest: d,
			})
			bytes += row.SizeBytes
		}

		stmtCtx, cancel := chunkCtx(ctx)
		n, err := e.blobs.DeleteBatch(stmtCtx, runID, batch)

		cancel()

		if err != nil {
			return fmt.Errorf("tear down %s/%s namespace %q: %w",
				b.OrgSlug, b.ProjectSlug, namespace, err)
		}

		sum.ObjectsDeleted += n
		sum.LogicalBytesFreed += bytes
		e.rec.ObjectsDeleted(backend, namespace, metrics.GCReasonTeardown, n)

		start = end
	}

	return nil
}

// teardownKindLabel is the blob.Ref metrics sub-kind for a namespace being torn
// down. It mirrors plan.go's per-stage kindLabel rather than inventing a "teardown"
// kind, because Ref.Kind names WHAT the object is, not why it is going; the reason
// is already a closed label of its own on bakery_gc_objects_deleted_total.
func teardownKindLabel(namespace string) string {
	switch namespace {
	case nsAC, nsACGRPC, nsSccache:
		return "ac"
	case nsCAS:
		return "cas"
	case nsTags:
		return "tag"
	case nsManifests:
		return "manifest"
	case nsBlobs:
		return "blob"
	default:
		return "object"
	}
}

// purgeHashserv empties a doomed hashserv backend's two tables and reports how many
// rows it deleted.
//
// NOT DeleteBatch, and for the same reason sweepHashserv (stages 1 and 2) is not:
// hashserv owns no cache_objects rows and no blobs, so there is no refcount to
// decrement, no byte to reclaim, and no LRU entry to invalidate. What it does own is
// a RESTRICT foreign key each, which is what makes this step necessary rather than
// merely tidy -- a hashserv backend whose unihashes are still there cannot be
// dropped, and nothing else in the sweep would ever delete them all.
//
// The order is the GC's own root ordering, one level down: unihashes are the root
// and outhashes hang off them, so the root goes first here exactly as it does in
// stage 1 before stage 2.
func (e *Engine) purgeHashserv(
	ctx context.Context, b repository.ListBackendsForTeardownRow, sum *Summary,
) (int64, error) {
	if b.Kind != repository.BackendKindHashserv {
		return 0, nil
	}

	var total int64

	for _, purge := range []struct {
		what string
		fn   func(context.Context, int64, int32) (int64, error)
	}{
		{"unihashes", func(c context.Context, id int64, n int32) (int64, error) {
			return e.db.PurgeHashservUnihashesChunk(c,
				repository.PurgeHashservUnihashesChunkParams{BackendID: id, ChunkLimit: n})
		}},
		{"outhashes", func(c context.Context, id int64, n int32) (int64, error) {
			return e.db.PurgeHashservOuthashesChunk(c,
				repository.PurgeHashservOuthashesChunkParams{BackendID: id, ChunkLimit: n})
		}},
	} {
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}

			stmtCtx, cancel := chunkCtx(ctx)
			n, err := purge.fn(stmtCtx, b.ID, e.teardownChunk())

			cancel()

			if err != nil {
				return total, fmt.Errorf("purge hashserv %s for backend %d: %w", purge.what, b.ID, err)
			}

			total += n
			sum.HashservRows += n

			if n < int64(e.cfg.BatchSize) {
				break
			}

			if err := e.pause(ctx); err != nil {
				return total, err
			}
		}
	}

	return total, nil
}

// dropBackendRow removes the emptied backend, tolerating the one failure that is not
// a failure.
//
// A 23503 here means a row was written after this run's snapshot was frozen -- which
// the write barrier spares BY DESIGN, and which ScanObjectsForGC therefore never
// returned. That is not an error condition, it is a backend that finishes on the next
// tick, so it is logged at INFO and the run continues. Failing the run instead would
// turn "a build wrote to a backend someone is deleting" into a red sweep.
func (e *Engine) dropBackendRow(
	ctx context.Context, b repository.ListBackendsForTeardownRow, deleted int64,
) error {
	stmtCtx, cancel := chunkCtx(ctx)
	n, err := e.db.DeleteTornDownBackend(stmtCtx, b.ID)

	cancel()

	if err != nil {
		if isForeignKeyViolation(err) {
			e.log.InfoContext(ctx, "backend teardown is not finished: rows arrived after this run's "+
				"snapshot was frozen, and the write barrier spared them. It will finish on a later run",
				slog.String("org", b.OrgSlug), slog.String("project", b.ProjectSlug),
				slog.String("kind", string(b.Kind)), slog.Int64("backend", b.ID))

			return nil
		}

		return fmt.Errorf("delete torn-down backend %d: %w", b.ID, err)
	}

	if n > 0 {
		e.log.InfoContext(ctx, "tore down a cache backend",
			slog.String("org", b.OrgSlug), slog.String("project", b.ProjectSlug),
			slog.String("kind", string(b.Kind)), slog.Int64("rows_deleted", deleted))
	}

	return nil
}

// finishTornDownProjects deletes every marked project that has no backends left.
//
// THE ANTI-JOIN IS THE ORDERING RULE, one level up the ownership tree from
// "tags before manifests before blobs": projects -> cache_backends is ON DELETE
// RESTRICT, so a project row can only go after its last backend row has. The query
// evaluates it at SELECT time and the foreign key evaluates it again at DELETE time,
// so a backend re-created in the gap blocks the delete rather than orphaning
// anything.
//
// The member lock the delete's cascade needs rides inside DeleteTornDownProject's own
// statement (see its comment): this package has no transaction to put it in, and
// every write here is deliberately one self-contained statement.
func (e *Engine) finishTornDownProjects(ctx context.Context) error {
	stmtCtx, cancel := chunkCtx(ctx)
	rows, err := e.db.ListProjectsForTeardown(stmtCtx)

	cancel()

	if err != nil {
		return fmt.Errorf("list projects for teardown: %w", err)
	}

	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}

		delCtx, cancelDel := chunkCtx(ctx)
		n, err := e.db.DeleteTornDownProject(delCtx, p.ID)

		cancelDel()

		if err != nil {
			if isForeignKeyViolation(err) {
				continue
			}

			return fmt.Errorf("delete torn-down project %s/%s: %w", p.OrgSlug, p.Slug, err)
		}

		if n > 0 {
			e.log.InfoContext(ctx, "tore down a project",
				slog.String("org", p.OrgSlug), slog.String("project", p.Slug))
		}
	}

	return nil
}

// isForeignKeyViolation reports a 23503. It is spelled out here rather than imported
// because internal/api's own isPGCode is unexported and this package deliberately
// depends on nothing in the control plane.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
