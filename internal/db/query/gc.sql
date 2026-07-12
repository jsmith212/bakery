-- Garbage collection. THE LOOP IS M6; the WRITE BARRIER is a property of the
-- schema and it lands now, with its tests, because retrofitting a barrier onto
-- tables that have been accumulating rows without one is not a migration, it is an
-- outage.

-- INSERT and COMMIT this BEFORE any scanning begins: started_at and snapshot are
-- frozen as of this transaction, and every sweep statement afterwards filters
-- against them. gc_runs_single_active_idx makes a second concurrent run a unique
-- violation rather than a race.
--
-- Never select `snapshot` back into Go. It is referenced only inside SQL
-- predicates, where it is cast back to pg_snapshot.
--
-- name: StartGCRun :one
INSERT INTO gc_runs (grace_period)
VALUES ($1)
RETURNING id, started_at;

-- name: FinishGCRun :exec
UPDATE gc_runs
   SET status          = $2,
       error           = $3,
       finished_at     = now(),
       objects_deleted = $4,
       blobs_marked    = $5,
       blobs_deleted   = $6,
       bytes_reclaimed = $7
 WHERE id = $1;

-- THE MARK. The write barrier lives here, and BOTH forms of it are ANDed.
--
--   created_at < started_at
--     the documented form (CLAUDE.md). Indexable, cheap, the prefilter.
--   pg_visible_in_snapshot(live_xid, snapshot)
--     the form that is actually CORRECT. now() is TRANSACTION-START time, so a
--     build that BEGINs before a GC run starts and COMMITs after it produces a row
--     whose created_at predates gc_runs.started_at while being invisible to the
--     GC's snapshot. The timestamp barrier alone says "sweep it" and eats a
--     freshly-minted row of a running build. This was reproduced on a live server.
--     clock_timestamp() narrows the window; it does not close it.
--
-- Rides blobs_gc_candidates_idx, a PARTIAL index whose predicate is
-- `unreferenced_since IS NOT NULL` and emphatically NOT `refcount = 0` -- the two
-- select identical rows, but a column in an index PREDICATE is HOT-blocking, and
-- refcount there makes every dedup increment rewrite an index entry (measured:
-- 0.0% HOT vs 97.6%).
--
-- SKIP LOCKED so one stuck candidate cannot stall the sweep. Under READ COMMITTED
-- FOR UPDATE re-evaluates the qual after the lock is granted (EvalPlanQual), so a
-- blob resurrected while we queued is filtered out automatically -- the recheck is
-- not a second round trip.
--
-- name: MarkBlobsPendingDelete :many
WITH run AS (
    SELECT started_at, snapshot, grace_period FROM gc_runs WHERE id = $1
),
candidate AS (
    SELECT b.digest
      FROM blobs b, run g
     WHERE b.state = 'live'
       AND b.refcount = 0
       AND b.unreferenced_since < now() - g.grace_period
       AND b.created_at < g.started_at
       AND pg_visible_in_snapshot(b.live_xid::text::xid8, g.snapshot::pg_snapshot)
     ORDER BY b.unreferenced_since
     LIMIT $2
       FOR UPDATE OF b SKIP LOCKED
)
UPDATE blobs b
   SET state = 'pending_delete', delete_started_at = now(), updated_at = now()
  FROM candidate c
 WHERE b.digest = c.digest
RETURNING b.digest, b.size_bytes;

-- THE PHYSICAL DELETE. Three statements, ONE transaction, and storage.Delete()
-- runs BETWEEN them while the digest advisory lock is held. Committing first and
-- unlinking after satisfies the crash invariant but REOPENS the resurrection race;
-- do not "optimise" the unlink out of the transaction without re-deriving it.
--
--   1. LockBlobDigest (objects.sql)
--   2. GetBlobForPhysicalDelete -- zero rows means the blob was revived by a
--      concurrent PUT while we queued for the lock. ROLLBACK. Do NOT unlink.
--   3. storage.Delete(digest), in Go, inside this transaction
--   4. ReapBlob
--
-- name: GetBlobForPhysicalDelete :one
SELECT digest, size_bytes
  FROM blobs
 WHERE digest = $1 AND state = 'pending_delete' AND refcount = 0
   FOR UPDATE;

-- cache_objects_blob_fk is ON DELETE RESTRICT, so if the refcount ever LIED and an
-- object still names this blob, this is a foreign-key violation and the
-- transaction aborts -- instead of silently unlinking bytes that are still named.
-- Refcount drift becomes a crash, not a corruption.
--
-- name: ReapBlob :execrows
DELETE FROM blobs
 WHERE digest = $1 AND state = 'pending_delete' AND refcount = 0;

-- Crash recovery. A process that died between the mark and the unlink left a
-- durable 'pending_delete' row. Boot and every GC run re-drive them; this is why
-- 'pending_delete' is a persisted state and not an in-memory work queue.
--
-- name: ListPendingDeleteBlobs :many
SELECT digest, size_bytes
  FROM blobs
 WHERE state = 'pending_delete'
 ORDER BY delete_started_at
 LIMIT $1;
