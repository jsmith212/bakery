-- Garbage collection. THE LOOP IS M6; the WRITE BARRIER is a property of the
-- schema and it lands now, with its tests, because retrofitting a barrier onto
-- tables that have been accumulating rows without one is not a migration, it is an
-- outage.

-- INSERT and COMMIT this BEFORE any scanning begins: started_at and snapshot are
-- frozen as of this transaction, and every sweep statement afterwards filters
-- against them. gc_runs_single_active_idx makes a second concurrent REAL
-- (dry_run = false) run a unique violation rather than a race; dry runs share no
-- such slot (000012: `WHERE status = 'running' AND NOT dry_run`) and may overlap
-- each other and a real run freely, because a dry run never writes.
--
-- Never select `snapshot` back into Go. It is referenced only inside SQL
-- predicates, where it is cast back to pg_snapshot.
--
-- name: StartGCRun :one
INSERT INTO gc_runs (grace_period, trigger, dry_run)
VALUES ($1, $2, $3)
RETURNING id, started_at;

-- Gains `AND status = 'running'` (finding 4): a stale finisher -- the boot reaper
-- racing the process's own shutdown finisher, or a shutdown finisher racing a run
-- that already finished on its own -- can now never move a run OUT of a terminal
-- state. Without the predicate, a delayed FinishGCRun(failed, "shutdown") landing
-- after the run had already legitimately succeeded would silently overwrite
-- objects_deleted/blobs_marked/etc back toward zero on a row every metric and every
-- audit view treats as done. :execrows is the caller's signal: 0 means this call
-- lost the race and touched nothing, not that the update was a no-op success.
--
-- name: FinishGCRun :execrows
UPDATE gc_runs
   SET status                = $2,
       error                 = $3,
       finished_at           = now(),
       objects_deleted       = $4,
       blobs_marked          = $5,
       blobs_deleted         = $6,
       bytes_reclaimed       = $7,
       hashserv_rows_deleted = $8
 WHERE id = $1 AND status = 'running';

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

-- Boot-time reaper (spec §9.3), and it runs IFF this process actually holds the
-- boot advisory lock -- gated by the CALLER on the lock, not on a flag, because
-- under --allow-multi-instance a booting instance must never mark ANOTHER
-- instance's live sweep failed. gc_runs_single_active_idx caps the real-run slot
-- at one row, but a crashed DRY run can also be left 'running' (dry runs hold no
-- slot), so the predicate is deliberately just `status = 'running'` and catches
-- both.
--
-- name: MarkOrphanedGCRunsFailed :execrows
UPDATE gc_runs
   SET status      = 'failed',
       error       = 'orphaned: process restarted mid-run',
       finished_at = now()
 WHERE status = 'running';

-- ===========================================================================
-- Layer A, per-backend enumeration (spec §3, §9.1: the in-process GC loop reads
-- this list once per pass and iterates every kind's stage over it).
-- ===========================================================================

-- Every cache_backends row, joined out to the slugs GC's metrics label on
-- (CLAUDE.md: Prometheus labels are slugs, never keys or digests -- and never a
-- bigint id). `enabled` is returned, NEVER filtered on: disabled backends are
-- swept too, and HARDER (spec §3, finding 10) -- disabled is a stronger retention
-- signal, not an exemption, so the caller computes an effective window of
-- least(configured, 30d) rather than skipping the row. retention_window and
-- quota_bytes ride along NULL exactly when a backend is unmanaged by GC (spec §8,
-- finding 7a: NULL window + NULL quota means "skip this backend's retention scan
-- entirely" -- inert config costs nothing).
--
-- name: ListBackendsForGC :many
SELECT cb.id, cb.kind, cb.enabled, cb.retention_window, cb.quota_bytes,
       cb.project_id, p.slug AS project_slug, p.org_id, o.slug AS org_slug
  FROM cache_backends cb
  JOIN projects p ON p.id = cb.project_id
  JOIN organizations o ON o.id = p.org_id
 ORDER BY cb.id;

-- The touch-staleness ramp clock (spec §6.4, 000012's gc_state table). Read ONCE
-- per toucher flush tick, the same cadence ListBackendsForGC is read once per GC
-- pass: `now() < touch_ramp_until` selects the widened T = 24h staleness guard
-- for the first 7 days after the 000012 migration ran (the one-time index-bloat
-- window fillfactor 85 manages but cannot eliminate, since it is catalog-only and
-- moves no existing tuple); past that timestamp the toucher tightens back down to
-- the configured --gc-touch-staleness (default 1h).
--
-- This REPLACES the pre-review-fix toucher's last-backend-wins permille proxy
-- (J: R6#7/R7#6/R8#5) -- an ad-hoc fraction recomputed per backend flush, with no
-- real notion of "time since upgrade" and a race between whichever backend's
-- flush happened to run last. gc_state's single row, stamped from THIS
-- migration's own now() at apply time, is unambiguous and needs no
-- cross-backend coordination. Stage F4 wires the toucher to this query and
-- removes the old proxy.
--
-- No WHERE clause: the table's own CHECK (id) plus its boolean primary key make
-- more than one row a schema-level impossibility, so there is nothing to filter.
--
-- name: GetGCState :one
SELECT touch_ramp_until FROM gc_state;

-- ===========================================================================
-- Layer A, cache_objects: the write-barrier-filtered scan + the keyed batch
-- delete (spec §3, §9.2).
-- ===========================================================================
--
-- ScanObjectsForGC applies ONLY the universal write barrier (spec §3's first two
-- of three ANDed predicates) -- NOT a stage's own retention/reachability rule.
-- That third predicate needs cross-table unihash lookups (sstate, §5),
-- tag-vs-manifest anti-joins (OCI, §3 stage 8) or a quota histogram (§8): none of
-- those belong in a generic per-backend cursor, so the caller decides eligibility
-- in Go over the rows this returns and deletes the losers via DeleteObjectsByKeys.
--
-- KEYSET, not OFFSET, over cache_objects_pkey's own column order
-- (backend_id, namespace, key): a single backend's slice is a contiguous range of
-- that btree, and the tuple comparison `(namespace, key) > (after_namespace,
-- after_key)` resumes exactly where the previous batch's LAST row left off. Pass
-- ('', '') to start from the beginning -- key has a CHECK (octet_length BETWEEN 1
-- AND 1024), so no real row's key is ever '', and namespace legitimately IS '' for
-- sstate/downloads, which is why the comparison is on the PAIR and not on key
-- alone.
--
-- run_id, not started_at/snapshot passed from Go: CLAUDE.md's rule for gc_runs
-- ("never select snapshot back into Go") holds here exactly as it holds in
-- MarkBlobsPendingDelete -- the frozen values are looked up FROM the row, in SQL,
-- by id.
--
-- run AS MATERIALIZED, snapshot cast to pg_snapshot ONCE (R8#6). Every query in
-- this file that joins a `run` CTE against a PER-ROW corpus (this one,
-- SweepUnihashes, SweepOrphanedOuthashes, NullOrphanSiginfo,
-- SweepUnreferencedManifests, DeleteObjectsByKeys, and each one's dry-run
-- mirror) shares this exact shape, for the same reason:
--
--   1. AS MATERIALIZED forces the single-row `run` lookup to execute ONCE and
--      be reused as a literal one-row table for every row of the outer scan --
--      without it, Postgres 12+'s CTE inlining is free (and, for a CTE this
--      cheap-looking, likely) to fold `run`'s SELECT into the outer plan and
--      re-evaluate `SELECT started_at, snapshot FROM gc_runs WHERE id = ...`
--      once per candidate row instead of once per statement. Harmless on a
--      handful of rows; on a 10^7-row keyset scan (spec §8's own sizing) it is
--      the same index probe run millions of times for an answer that cannot
--      change within the statement.
--   2. `snapshot::pg_snapshot AS snap`, cast ONCE inside the CTE's SELECT list,
--      not left as `g.snapshot::pg_snapshot` in the outer predicate. Even with
--      the CTE materialized, a cast written INSIDE the per-row predicate
--      expression -- `pg_visible_in_snapshot(o.live_xid::text::xid8,
--      g.snapshot::pg_snapshot)` -- is itself an expression the executor
--      evaluates once per row it is tested against: it would re-parse the SAME
--      (potentially long, one entry per concurrently-open transaction) textual
--      snapshot representation into a pg_snapshot value on every row instead of
--      once. Casting inside the materialized CTE means the ROW `run` produces
--      already carries a pg_snapshot-typed column, and the per-row predicate
--      only ever does the part that must genuinely run per row: testing THAT
--      row's live_xid against the (now pre-parsed) snapshot.
--
-- name: ScanObjectsForGC :many
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
SELECT o.namespace, o.key, o.digest, o.size_bytes,
       o.created_at, o.accessed_at, o.updated_at, o.content_type
  FROM cache_objects o, run g
 WHERE o.backend_id = sqlc.arg(backend_id)
   AND (o.namespace, o.key) > (sqlc.arg(after_namespace)::text, sqlc.arg(after_key)::text)
   AND o.created_at < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap)
 ORDER BY o.namespace, o.key
 LIMIT sqlc.arg(scan_limit);

-- THE GC's ONLY batch delete of cache_objects (spec §9.2; blob.Service.DeleteBatch
-- is the only caller). Scoped to ONE (backend_id, namespace) -- every GC stage
-- already operates one namespace at a time -- with the doomed set's keys and
-- digests passed as parallel arrays.
--
-- CARRIES THE WRITE BARRIER, BOTH HALVES (R7#5 CRITICAL). Every other Layer-A
-- sweep (SweepUnihashes, SweepOrphanedOuthashes, SweepUnreferencedManifests,
-- NullOrphanSiginfo) re-derives `created_at < run.started_at AND
-- pg_visible_in_snapshot(...)` INSIDE its own DELETE/UPDATE, against the row as
-- it stands AT DELETE TIME -- this query used to be the one exception, trusting
-- the caller's doomed set (built from an EARLIER ScanObjectsForGC pass) as if it
-- were still true. It is not, in general: the doomed set is computed from a scan
-- taken before this statement runs, and in the gap a concurrent /ac/ overwrite
-- (PutObjectOverwritable) can repoint the SAME (backend_id, namespace, key) at a
-- new digest -- which refreshes created_at and live_xid via the ordinary UPDATE
-- path (there is no separate "this row was overwritten" column; the row's own
-- creation columns move). `o.key = doomed.key` alone does not care when the row
-- was last (re)written, so without re-checking the barrier HERE, a doomed set
-- built from stale evidence deletes a key a build just resurrected -- the exact
-- failure TestGCWriteBarrierSparesAConcurrentBuild (db_test.go) and its per-stage
-- replicas (gc_retention_test.go) exist to catch, reproduced one layer up in the
-- one stage that skipped it. A key that raced past the barrier is simply left
-- alone; the caller's :execrows already treats "deleted fewer rows than
-- requested" as ordinary (ScanObjectsForGC's own snapshot can be stale by the
-- time the delete lands), so this costs nothing at the call site.
--
-- run AS MATERIALIZED, snapshot cast to pg_snapshot ONCE (R8#6): see
-- ScanObjectsForGC above for the full rationale -- identical here, this DELETE
-- evaluates pg_visible_in_snapshot() once per row of the doomed set, and without
-- a materialized, pre-cast CTE the planner is free to inline the
-- text->pg_snapshot cast into that per-row expression and re-parse the (long)
-- textual snapshot on every row instead of once for the whole statement.
--
-- DIGEST-ORDERED, not caller-ordered (finding 3): the refcount trigger's /ac/
-- overwrite branch takes its paired blob locks in ascending digest order
-- SPECIFICALLY to avoid an ABBA deadlock against a concurrent overwrite touching
-- the same two digests in the opposite order (see cache_objects_refcount() in
-- 000006). A batch delete that processed rows in an arbitrary order would
-- reintroduce that same ABBA a thousand digests wide: this transaction locking
-- blobs.digest = A then B while a concurrent /ac/ PUT locks B then A. Sorting the
-- driving set by digest BEFORE the DELETE touches a single row -- mirroring
-- DeleteObjectsChunk's existing subquery-then-DELETE shape -- makes every writer
-- that deletes in digest order (this query) or locks in digest order (the
-- trigger) acquire blobs row locks in the SAME global order, so the cycle cannot
-- form. An ORDER BY inside this USING subquery is a row-order hint, not a lock-
-- order guarantee (R7#11) -- LockBlobDigests (query/objects.sql) is the actual
-- guarantee, and blob.Service.DeleteBatch calls it, in the same transaction,
-- BEFORE this DELETE. The caller retries bounded on 40P01 regardless, for the
-- residual case of two batches racing each other.
--
-- name: DeleteObjectsByKeys :execrows
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
DELETE FROM cache_objects o
 USING (
     SELECT k.key, d.digest
       FROM unnest(sqlc.arg(keys)::text[])    WITH ORDINALITY AS k(key, ord)
       JOIN unnest(sqlc.arg(digests)::bytea[]) WITH ORDINALITY AS d(digest, ord)
         ON k.ord = d.ord
      ORDER BY d.digest
 ) doomed, run g
 WHERE o.backend_id  = sqlc.arg(backend_id)
   AND o.namespace    = sqlc.arg(namespace)
   AND o.key          = doomed.key
   AND o.created_at   < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap);

-- Batched, staleness-guarded write of accessed_at = now() (spec §6.1): the
-- toucher flusher's ONLY write. `touchKeysSQL` shape -- ONE UPDATE per flush tick
-- per backend, not one per key, is what makes "N reads in T => one UPDATE"
-- (TestToucherFlushIsCoalesced) hold.
--
-- THE STALENESS GUARD (accessed_at IS NULL OR accessed_at < now() - staleness) is
-- not an optimisation, it is what keeps this a HOT update under a hot key: without
-- it, a key read every few seconds writes accessed_at every few seconds forever,
-- against a column that is (correctly) in no index but still costs a tuple copy
-- per write. Guarding on T (--gc-touch-staleness) bounds the write rate to at most
-- one per key per T regardless of read rate.
--
-- DOES NOT TOUCH created_at OR live_xid, on purpose (mirrors TouchObject's
-- comment in objects.sql): those two ARE the write barrier, which marks ROW
-- CREATION, and a touch that confirmed a row is still read created nothing.
--
-- name: TouchObjectsAccessed :execrows
UPDATE cache_objects
   SET accessed_at = now()
 WHERE backend_id = sqlc.arg(backend_id)
   AND namespace   = sqlc.arg(namespace)
   AND key = ANY(sqlc.arg(keys)::text[])
   AND (accessed_at IS NULL OR accessed_at < now() - sqlc.arg(staleness)::interval);

-- The hashserv_unihashes twin of TouchObjectsAccessed, keyed on the table's real
-- primary key (backend_id, method, taskhash) rather than a single key column, so
-- method and taskhash travel as parallel arrays through the same unnest-then-join
-- shape DeleteObjectsByKeys uses. Same staleness guard, same "does not touch
-- created_at / live_xid" rule, for the same reason.
--
-- name: TouchUnihashesAccessed :execrows
UPDATE hashserv_unihashes u
   SET accessed_at = now()
  FROM (
      SELECT m.method, t.taskhash
        FROM unnest(sqlc.arg(methods)::text[])    WITH ORDINALITY AS m(method, ord)
        JOIN unnest(sqlc.arg(taskhashes)::text[]) WITH ORDINALITY AS t(taskhash, ord)
          ON m.ord = t.ord
  ) pair
 WHERE u.backend_id = sqlc.arg(backend_id)
   AND u.method     = pair.method
   AND u.taskhash   = pair.taskhash
   AND (u.accessed_at IS NULL OR u.accessed_at < now() - sqlc.arg(staleness)::interval);

-- ===========================================================================
-- Layer A, hashserv (spec §3 stages 1-2). hashserv_unihashes is the GC ROOT: an
-- sstate object is reachable only FROM a unihash row, so this sweep runs BEFORE
-- sstate's (CLAUDE.md, query/hashserv.sql's file header).
-- ===========================================================================
--
-- THE LIVENESS RULE, VERBATIM (spec §3): coalesce(accessed_at, created_at) <
-- now() - window. NOT a bare `accessed_at < ...` -- that makes every pre-upgrade
-- row (every accessed_at is NULL on day one) immortal, and makes quota eviction
-- non-terminating (finding 1, TestNullAccessedAtRowsAreSweepable). created_at is
-- used ONLY when there is no read record at all, which is genuinely cold; this is
-- not the same trap as trusting a CAS blob's created_at alone (CLAUDE.md's GC/M4
-- note), because coalesce here still prefers the read timestamp when one exists.
--
-- Both write-barrier halves, exactly as MarkBlobsPendingDelete carries them, via
-- the same run_id-scoped CTE.
--
-- run AS MATERIALIZED, snapshot cast ONCE (R8#6): see ScanObjectsForGC
-- (above) for the full rationale.
--
-- name: SweepUnihashes :execrows
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
DELETE FROM hashserv_unihashes u
 USING run g
 WHERE u.backend_id = sqlc.arg(backend_id)
   AND u.created_at < g.started_at
   AND pg_visible_in_snapshot(u.live_xid::text::xid8, g.snap)
   AND coalesce(u.accessed_at, u.created_at) < now() - sqlc.arg(retention_window)::interval;

-- Dry-run mirror of SweepUnihashes: identical predicate, COUNT instead of DELETE.
-- Every destructive sweep query in this file has one of these; a dry run's gc_runs
-- row (dry_run = true) is genuinely read-only and performs no delete anywhere.
--
-- name: DryRunSweepUnihashes :one
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
SELECT count(*)::bigint AS would_delete
  FROM hashserv_unihashes u, run g
 WHERE u.backend_id = sqlc.arg(backend_id)
   AND u.created_at < g.started_at
   AND pg_visible_in_snapshot(u.live_xid::text::xid8, g.snap)
   AND coalesce(u.accessed_at, u.created_at) < now() - sqlc.arg(retention_window)::interval;

-- Batch existence probe for the sstate unihash-root derivation (spec §5 step 4):
-- given every unihash a scan batch of sstate keys derived, which ones still have a
-- surviving row on the PAIRED hashserv backend. Rides hashserv_unihashes_unihash
-- (backend_id, unihash) exactly as UnihashExists (query/hashserv.sql) does for the
-- single-value form; this is that query's StatObjectsBatch-shaped sibling so a
-- scan batch costs one round trip instead of one per key.
--
-- Returns ONLY the unihashes that exist -- same "absence is the answer" contract
-- as StatObjectsBatch (query/objects.sql). An unparseable or absent unihash is
-- legal (spec §5 step 5: do_populate_lic swspecs, or a deployment with zero
-- hashserv coverage) and is NOT an error; it is what makes the caller treat the
-- sstate key as unreachable.
--
-- name: UnihashesExistBatch :many
SELECT unihash
  FROM hashserv_unihashes
 WHERE backend_id = sqlc.arg(backend_id)
   AND unihash = ANY(sqlc.arg(unihashes)::text[]);

-- THE OTHER HALF OF THE sstate COVERAGE GUARD (spec §5, finding 12). Coverage on
-- its own cannot tell two situations apart, and they demand opposite behaviour:
--
--   no hashserv data at all (an rsync'd mirror, BB_HASHSERVE=auto, no hashserv
--   backend) -- collapsing sstate retention to age-only is CORRECT there;
--   the derivation broke -- age-only retention would delete a live cache.
--
-- Both read as "zero unihashes resolved". This query is what separates them: the
-- engine refuses to sweep an sstate backend that resolved ZERO unihashes while its
-- PAIRED hashserv backend holds rows.
--
-- EXISTS, not count(*): the answer is a boolean, so the plan stops at the first
-- matching row of the (backend_id, ...) index prefix. The guard costs one index
-- probe per sstate backend per run rather than a full count of a table that holds
-- one row per task per build.
--
-- name: HashservBackendHasUnihashes :one
SELECT EXISTS (
    SELECT 1 FROM hashserv_unihashes WHERE backend_id = sqlc.arg(backend_id)
)::boolean AS has_rows;

-- Orphaned outhash rows (spec §3 stage 2): space reclamation, not correctness
-- (finding 9) -- the equivalence queries in query/hashserv.sql (GetEquivalentForOuthash,
-- GetOuthashWithUnihash) already JOIN hashserv_unihashes, so an orphan is filtered
-- out of every answer, never served as one. Deleting it just stops it from taking
-- up space.
--
-- NO accessed_at ON THIS TABLE (spec §7: only cache_objects and
-- hashserv_unihashes get the column -- hashserv_outhashes is read exclusively by
-- get-outhash/get(all=true), never on a build's hot path, so there is nothing
-- worth tracking reads for). The liveness rule therefore degenerates to plain
-- created_at < now() - window rather than a coalesce: there is no second timestamp
-- to prefer.
--
-- run AS MATERIALIZED, snapshot cast ONCE (R8#6): see ScanObjectsForGC
-- (above) for the full rationale.
--
-- name: SweepOrphanedOuthashes :execrows
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
DELETE FROM hashserv_outhashes o
 USING run g
 WHERE o.backend_id = sqlc.arg(backend_id)
   AND o.created_at < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap)
   AND o.created_at < now() - sqlc.arg(retention_window)::interval
   AND NOT EXISTS (
       SELECT 1 FROM hashserv_unihashes u
        WHERE u.backend_id = o.backend_id
          AND u.method     = o.method
          AND u.taskhash   = o.taskhash
   );

-- name: DryRunSweepOrphanedOuthashes :one
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
SELECT count(*)::bigint AS would_delete
  FROM hashserv_outhashes o, run g
 WHERE o.backend_id = sqlc.arg(backend_id)
   AND o.created_at < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap)
   AND o.created_at < now() - sqlc.arg(retention_window)::interval
   AND NOT EXISTS (
       SELECT 1 FROM hashserv_unihashes u
        WHERE u.backend_id = o.backend_id
          AND u.method     = o.method
          AND u.taskhash   = o.taskhash
   );

-- outhash_siginfo is set NULL on ANY outhash row PAST W_siginfo = 4 x W -- orphaned
-- or not (spec §3 stage 2, finding 19): siginfo is ~128 KiB of TOASTed text
-- (000010's comment on hashserv_outhashes.outhash_siginfo), read only by
-- get-outhash/get(all=true) RPCs that are not on any build's path, so it is purely
-- a space-reclamation UPDATE. The write barrier is still carried for the same
-- reason every Layer-A stage carries it: a still-in-flight report's row must not
-- be mutated out from under it, even though the 4x window makes the practical risk
-- small. A row whose siginfo is already NULL is excluded so a re-run never issues
-- a write for a row it already cleared (cheap, and keeps this idempotent at the
-- statement level as well as the data level).
--
-- run AS MATERIALIZED, snapshot cast ONCE (R8#6): see ScanObjectsForGC
-- (above) for the full rationale.
--
-- name: NullOrphanSiginfo :execrows
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
UPDATE hashserv_outhashes o
   SET outhash_siginfo = NULL
  FROM run g
 WHERE o.backend_id = sqlc.arg(backend_id)
   AND o.outhash_siginfo IS NOT NULL
   AND o.created_at < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap)
   AND o.created_at < now() - sqlc.arg(retention_window)::interval;

-- name: DryRunNullOrphanSiginfo :one
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
SELECT count(*)::bigint AS would_null
  FROM hashserv_outhashes o, run g
 WHERE o.backend_id = sqlc.arg(backend_id)
   AND o.outhash_siginfo IS NOT NULL
   AND o.created_at < g.started_at
   AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snap)
   AND o.created_at < now() - sqlc.arg(retention_window)::interval;

-- ===========================================================================
-- Layer A, OCI manifests (spec §3 stage 8). CLAUDE.md's "sweep tags before
-- manifests before the blobs table" resolves here to: this query runs after the
-- tags-namespace sweep (stage 7, an ordinary window-only DeleteObjectsByKeys pass
-- Go drives over ScanObjectsForGC -- no dedicated query, a tag has no fan-in to
-- anti-join against) and before the OCI blobs-namespace sweep (stage 9, same
-- ordinary shape).
-- ===========================================================================
--
-- TAG -> MANIFEST ANTI-JOIN, backend-scoped. cache_objects_digest_idx is on
-- digest ALONE (000006), so the backend_id match on the inner NOT EXISTS is a
-- POST-filter over that index's candidate rows, not an indexed predicate -- fine
-- at OCI scale (spec §3 stage 8 says so explicitly).
--
-- coalesce(accessed_at, created_at), NOT created_at alone (finding 18): a deduped,
-- hot base-image manifest can be old by created_at while still being pulled
-- constantly through a DIFFERENT tag in the SAME backend that this query's
-- anti-join does not see (a different repo, same digest) -- that is precisely the
-- CAS trap CLAUDE.md's GC/M4 note describes, reproduced one layer up in OCI. The
-- coalesce rule is what a fresh StatUncached-triggered read (which touches
-- accessed_at through the LRU, spec §6.1) keeps alive even with zero tags pointing
-- at it directly.
--
-- No DeleteObjectsByKeys round trip: this is a single self-contained DELETE
-- because, unlike the sstate root derivation, the anti-join needs no Go-side
-- lookup to decide eligibility. No explicit digest ordering either (contrast
-- DeleteObjectsByKeys): manifests are an IMMUTABLE namespace written only by
-- PutObjectImmutable, so the refcount trigger's INSERT/DELETE branches each take
-- exactly one blobs row lock -- the paired-lock UPDATE branch that motivates
-- digest ordering never fires here.
--
-- THIS IS THE ONLY DELETION PATH FOR THE manifests NAMESPACE (E: R6#4/R7#9). The
-- pre-review-fix engine decided manifest deletions in Go, from a `tagDigests` set
-- that stage 7 accumulated as it scanned the tags namespace, and then deleted
-- through DeleteObjectsByKeys. That set is a snapshot of the tags namespace taken
-- BEFORE the manifests scan begins and page by page, so it answers "was this
-- digest tagged when stage 7 read that page" -- not "is it tagged now, at the
-- instant the DELETE runs". A `docker pull` that writes a tag between the two
-- (the ordinary case: SWR revalidation writes tags constantly) leaves a live tag
-- pointing at a manifest this sweep has already decided is unreferenced. Doing
-- the anti-join HERE, in the same statement as the DELETE and under the same
-- write barrier, closes that gap by construction: there is no window between the
-- decision and the delete because they are one statement.
--
-- RETURNS THE DELETED KEYS, and the caller MUST invalidate them from the LRU
-- (blob.Service.InvalidateKeys). Manifests are served through blob.Get, so a
-- positive LRU entry outlives a raw SQL delete and answers "present" for an
-- object whose row is gone -- which the byte reap then turns into a dangling
-- metadata 500. This is the same obligation DeleteObjectsByKeys discharges via
-- blob.Service.DeleteBatch's own delBatch call; the difference is only that the
-- keys come back from the statement instead of going into it.
--
-- run AS MATERIALIZED, snapshot cast ONCE (R8#6): see ScanObjectsForGC
-- (above) for the full rationale.
--
-- name: SweepUnreferencedManifests :many
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
DELETE FROM cache_objects m
 USING run g
 WHERE m.backend_id = sqlc.arg(backend_id)
   AND m.namespace   = 'manifests'
   AND m.created_at  < g.started_at
   AND pg_visible_in_snapshot(m.live_xid::text::xid8, g.snap)
   AND coalesce(m.accessed_at, m.created_at) < now() - sqlc.arg(retention_window)::interval
   AND NOT EXISTS (
       SELECT 1 FROM cache_objects t
        WHERE t.backend_id = m.backend_id
          AND t.namespace  = 'tags'
          AND t.digest     = m.digest
   )
RETURNING m.key;

-- name: DryRunSweepUnreferencedManifests :one
WITH run AS MATERIALIZED (
    SELECT started_at, snapshot::pg_snapshot AS snap FROM gc_runs WHERE id = sqlc.arg(run_id)
)
SELECT count(*)::bigint AS would_delete
  FROM cache_objects m, run g
 WHERE m.backend_id = sqlc.arg(backend_id)
   AND m.namespace   = 'manifests'
   AND m.created_at  < g.started_at
   AND pg_visible_in_snapshot(m.live_xid::text::xid8, g.snap)
   AND coalesce(m.accessed_at, m.created_at) < now() - sqlc.arg(retention_window)::interval
   AND NOT EXISTS (
       SELECT 1 FROM cache_objects t
        WHERE t.backend_id = m.backend_id
          AND t.namespace  = 'tags'
          AND t.digest     = m.digest
   );

-- ===========================================================================
-- Quota / storage gauges (spec §8).
-- ===========================================================================

-- One row per backend, upserted by the sweep's own scan pass or by the
-- lightweight --gc-usage-interval pass that runs even with retention disabled
-- (finding 7b/13). objects_count/logical_bytes are the LOGICAL accounting the
-- 000012 table comment explains; measured_at is what makes staleness detectable
-- (bakery_gc_usage_measured_timestamp_seconds, spec §9.9).
--
-- name: UpsertBackendUsage :exec
INSERT INTO cache_backend_usage (backend_id, objects_count, logical_bytes, measured_at)
VALUES (sqlc.arg(backend_id), sqlc.arg(objects_count), sqlc.arg(logical_bytes), now())
ON CONFLICT (backend_id) DO UPDATE
   SET objects_count = EXCLUDED.objects_count,
       logical_bytes = EXCLUDED.logical_bytes,
       measured_at   = EXCLUDED.measured_at;

-- The INSTANCE-WIDE, backend-blind physical gauge (finding 15): quota accounting
-- is logical and per-backend (cache_backend_usage above), so it is the wrong
-- number for "how much disk is this process actually using" under cross-project
-- dedup -- a blob named by three backends counts three times in their logical
-- totals and once here. bakery_storage_physical_bytes is the number an operator
-- alerts on for disk pressure; 'live' excludes bytes already marked
-- pending_delete (not yet reaped, but no longer counted as space in productive
-- use).
--
-- name: InstancePhysicalBytes :one
SELECT coalesce(sum(size_bytes), 0)::bigint AS physical_bytes
  FROM blobs
 WHERE state = 'live';

-- ===========================================================================
-- Operator surface (spec §9.10): GET /api/v1/gc/runs[, /{id}], read-only. Nothing
-- below writes; both ride gc_runs_started_at_idx / the primary key.
-- ===========================================================================

-- KEYSET pagination on id DESC, not OFFSET: gc_runs.id is a bigint IDENTITY that
-- only ever grows, and it agrees with gc_runs_started_at_idx's own DESC order
-- (rows are inserted in id order), so "id < the last row on the previous page"
-- resumes exactly where that page left off. An OFFSET page would silently skip or
-- repeat a row the instant a new run starts between two requests -- which, for an
-- endpoint whose whole purpose is watching runs start, is not a rare case.
--
-- sqlc.narg(status) / sqlc.narg(before_id): NULL means "no filter" and "from the
-- newest row", which is how the API's optional ?status= and a first page both
-- read -- no separate "list everything" query to keep in sync with this one.
--
-- include_usage EXCLUDES trigger = 'usage' rows BY DEFAULT (R7#12). MeasureUsage's
-- --gc-usage-interval pass (spec §8, findings 7b/13) mints its own gc_runs row on
-- every tick -- every 6h, forever, independent of whether retention is even
-- enabled -- purely so the measurement itself is auditable. A console operator
-- watching /api/v1/gc/runs for SWEEP activity does not want that list drowned in
-- one lightweight measurement row every 6h forever; sqlc.arg(include_usage),
-- rather than a NULL-means-no-filter narg like the two above, is a plain boolean
-- because there is no "unset" state for it that means anything different from
-- false -- the API defaults it to false and a caller must opt IN with
-- ?include_usage=true to see them.
--
-- name: ListGCRuns :many
SELECT id, started_at, finished_at, status, error, trigger, dry_run,
       objects_deleted, blobs_marked, blobs_deleted, bytes_reclaimed, hashserv_rows_deleted
  FROM gc_runs
 WHERE (sqlc.narg(status)::gc_run_status IS NULL OR status = sqlc.narg(status)::gc_run_status)
   AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
   AND (sqlc.arg(include_usage)::boolean OR trigger <> 'usage')
 ORDER BY id DESC
 LIMIT sqlc.arg(page_limit);

-- name: GetGCRun :one
SELECT id, started_at, finished_at, status, error, trigger, dry_run,
       objects_deleted, blobs_marked, blobs_deleted, bytes_reclaimed, hashserv_rows_deleted
  FROM gc_runs
 WHERE id = sqlc.arg(id);

-- ===========================================================================
-- gc_run_backends (000013, SPA->API wiring wave B7): per-backend attribution, so
-- an org viewer can see what a sweep did to THEIR projects.
-- ===========================================================================

-- RecordGCRunBackend is the engine's own write, called once per swept backend per
-- real run from internal/gc/sweep.go (sweepHashserv's and sweepBackend's own
-- summary points) -- never for a dry run, never for MeasureUsage's usage-only
-- pass, and never for a backend the plan declined to sweep at all. ON CONFLICT is
-- a defensive upsert, not the expected path: (run_id, backend_id) is written by
-- exactly one call per run under this engine's single-writer discipline (spec
-- CLAUDE.md, "boot takes pg_try_advisory_lock and refuses a second instance"), so
-- the conflict arm exists only to make a retry of this one statement idempotent
-- rather than a second, disagreeing row.
--
-- name: RecordGCRunBackend :exec
INSERT INTO gc_run_backends (run_id, backend_id, objects_deleted, bytes_freed)
VALUES (sqlc.arg(run_id), sqlc.arg(backend_id), sqlc.arg(objects_deleted), sqlc.arg(bytes_freed))
ON CONFLICT (run_id, backend_id) DO UPDATE
   SET objects_deleted = EXCLUDED.objects_deleted,
       bytes_freed     = EXCLUDED.bytes_freed;

-- ListOrgGCActivity is GET /orgs/{org}/gc/activity: the most recent RUNS that
-- touched this org's projects, each with every one of ITS backend rows that
-- belongs to this org (a run can span many orgs; only this org's slice is
-- returned).
--
-- Paginated on RUNS, not on the flat (run, backend) rows the query returns --
-- the API groups this result by run_id in Go and the ?limit= the caller gave
-- bounds how many runs come back, not how many backend rows. The `runs` CTE picks
-- the page of run ids FIRST, scoped to the org and the keyset cursor, and the
-- outer SELECT re-joins to fetch every row of exactly those runs; a plain
-- `LIMIT` on the flat join would cut a run's backend list off mid-way and the
-- caller would never know it was truncated rather than short.
--
-- sqlc.narg(before_id): NULL means "from the newest run", matching ListGCRuns'
-- own convention above.
--
-- name: ListOrgGCActivity :many
WITH runs AS (
    SELECT DISTINCT gr.id
      FROM gc_runs gr
      JOIN gc_run_backends grb ON grb.run_id = gr.id
      JOIN cache_backends cb ON cb.id = grb.backend_id
      JOIN projects p ON p.id = cb.project_id
     WHERE p.org_id = sqlc.arg(org_id)
       AND (sqlc.narg(before_id)::bigint IS NULL OR gr.id < sqlc.narg(before_id)::bigint)
     ORDER BY gr.id DESC
     LIMIT sqlc.arg(run_limit)
)
SELECT gr.id AS run_id, gr.started_at, gr.finished_at, gr.status,
       p.slug AS project_slug, cb.kind, grb.objects_deleted, grb.bytes_freed
  FROM runs
  JOIN gc_runs gr ON gr.id = runs.id
  JOIN gc_run_backends grb ON grb.run_id = gr.id
  JOIN cache_backends cb ON cb.id = grb.backend_id
  JOIN projects p ON p.id = cb.project_id
 WHERE p.org_id = sqlc.arg(org_id)
 ORDER BY gr.id DESC, p.slug, cb.kind;
