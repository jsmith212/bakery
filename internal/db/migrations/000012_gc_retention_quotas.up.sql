-- Bakery M6: GC retention + quotas. spec/2026-08-14-m6-gc-retention-quotas.md is
-- the contract; section numbers below refer to it.
--
-- TWO LAYERS (spec §2). Layer A -- this migration -- is metadata: namespace-scoped
-- sweeps of cache_objects / hashserv_unihashes / hashserv_outhashes, driven by a
-- retention_window per cache_backends row. Layer B is the ALREADY-BUILT
-- MarkBlobsPendingDelete -> ReapDigest machinery (000007, query/gc.sql) and is
-- untouched here; a Layer-A delete simply fires the same refcount trigger that
-- feeds it.
--
-- RETENTION SHIPS ON, OPINIONATED (spec §1.1). This migration seeds real
-- retention_window values onto every EXISTING backend below. The owner accepted
-- that the first sweep after upgrade deletes objects older than the window by
-- created_at (every accessed_at is NULL on day one -- spec §4's "upgrade shock").
-- The rails are runtime, not schema: the 24h grace period already on gc_runs
-- (000007), `bakery gc run --dry-run`, and `--gc-disable-retention`.

-- ---------------------------------------------------------------------------
-- accessed_at, fillfactor, autovacuum -- the two tables the GC touch-toucher hits
-- ---------------------------------------------------------------------------
--
-- accessed_at is nullable, has NO DEFAULT (NULL is exactly "never touched since
-- upgrade", which is the true state of every pre-existing row), and IS IN NO
-- INDEX -- not as a key, not in a predicate (spec §6). Any indexed column the
-- toucher's UPDATE touches forfeits HOT, and the toucher runs on the same hot
-- tables the sstate HEAD storm and the REAPI FindMissingBlobs storm hit.
--
-- fillfactor 85 is catalog-only and moves NO existing tuple (spec §6.4): the
-- corpus sits at ~95-100% fill today, so the FIRST touch of every pre-existing row
-- is non-HOT regardless. Lowering fillfactor now is what makes the SECOND and
-- later touches of that same row HOT once the page has the slack a non-HOT update
-- needs. autovacuum_vacuum_scale_factor = 0.02 (default 0.2, a 10x tightening) is
-- the matching autovacuum aggressiveness for the one-time bloat spike that first
-- touch produces.
--
-- LOCK_TIMEOUT, not indefinite blocking. Every ALTER below takes ACCESS
-- EXCLUSIVE, however briefly, on cache_objects -- the table the sstate HEAD storm
-- and the REAPI FindMissingBlobs storm hit -- and on hashserv_unihashes. In
-- production this is safe BY CONSTRUCTION: migrations run before any listener
-- opens (server.Boot: connect+ping -> advisory boot lock -> migrate -> ... -> bind
-- listeners), and the boot advisory lock has already proven this is the ONLY
-- bakery instance touching the database, so there is no concurrent HEAD storm to
-- queue behind. The one configuration that invalidates that reasoning is
-- --allow-multi-instance, where a second, already-serving instance's live traffic
-- could hold locks this ALTER waits on -- which is one more reason (alongside the
-- LRU, boot-reaper and pending-set-completeness reasons the GC runtime records)
-- that GC refuses to run under --allow-multi-instance (spec §9.4). lock_timeout
-- turns that one unsupported configuration's failure mode from "migration hangs
-- forever" into "the migration fails loudly inside migrate's own per-file
-- transaction, changes nothing, and the operator reruns it" rather than a
-- half-applied schema.
SET LOCAL lock_timeout = '5s';

ALTER TABLE cache_objects ADD COLUMN accessed_at timestamptz;
ALTER TABLE cache_objects SET (fillfactor = 85, autovacuum_vacuum_scale_factor = 0.02);

ALTER TABLE hashserv_unihashes ADD COLUMN accessed_at timestamptz;
ALTER TABLE hashserv_unihashes SET (fillfactor = 85, autovacuum_vacuum_scale_factor = 0.02);

-- ---------------------------------------------------------------------------
-- cache_backends: retention_window + quota_bytes
-- ---------------------------------------------------------------------------
--
-- NULL always means "retain forever" / "no cap", and stays settable per backend
-- (spec §4). Both are real columns, not config jsonb: they are read on every GC
-- pass and compared arithmetically, which the schemaless config column (000003)
-- was never meant for.
ALTER TABLE cache_backends
    ADD COLUMN retention_window interval,
    ADD COLUMN quota_bytes      bigint;

ALTER TABLE cache_backends
    ADD CONSTRAINT cache_backends_retention_window_positive
        CHECK (retention_window IS NULL OR retention_window > interval '0'),
    ADD CONSTRAINT cache_backends_quota_bytes_positive
        CHECK (quota_bytes IS NULL OR quota_bytes > 0),
    -- Quota is REFUSED on hashserv backends, structurally (spec §7.4, finding 15).
    -- hashserv has no cache_objects rows -- the quota histogram in spec §8 runs
    -- over cache_objects -- so a hashserv quota would be unenforceable by
    -- construction and would read 0 forever, which is a silent lie rather than an
    -- honest "no cap". Product decision 3 (spec §1.3) also gives OCI no quota, but
    -- that is a SEEDING choice (CreateBackend never sets one), not a schema
    -- refusal: unlike hashserv, an OCI quota is at least representable and
    -- enforceable if an operator opts in later.
    ADD CONSTRAINT cache_backends_hashserv_no_quota
        CHECK (kind <> 'hashserv' OR quota_bytes IS NULL);

-- OPINIONATED DEFAULTS (spec §1.1, §4), seeded onto every EXISTING row.
-- CreateBackend seeds the same table onto every NEW row (a later stage; this
-- migration only backfills history). `downloads` is excluded on purpose and
-- stays NULL: it is an ARCHIVE, not a cache (spec §1.2) -- a premirror tarball
-- whose upstream died is unrecoverable, unlike every other namespace's eviction.
UPDATE cache_backends
   SET retention_window = CASE kind
           WHEN 'sstate'   THEN interval '90 days'
           WHEN 'hashserv' THEN interval '90 days'
           WHEN 'bazel'    THEN interval '30 days'
           WHEN 'oci'      THEN interval '30 days'
       END
 WHERE retention_window IS NULL AND kind <> 'downloads';

-- ---------------------------------------------------------------------------
-- organizations: default_retention_window / default_quota_bytes
-- ---------------------------------------------------------------------------
--
-- SEED DEFAULTS for new backends in this org, NEVER enforced ceilings (spec
-- §1.3): there is no cross-project aggregate cap in M6, and nothing here reads
-- these two columns except CreateBackend's seeding logic. NULL means "fall back
-- to the per-kind default from the table above" -- an org opts IN to an override,
-- it never opts OUT of the opinionated defaults by leaving this NULL.
ALTER TABLE organizations
    ADD COLUMN default_retention_window interval
        CHECK (default_retention_window IS NULL OR default_retention_window > interval '0'),
    ADD COLUMN default_quota_bytes bigint
        CHECK (default_quota_bytes IS NULL OR default_quota_bytes > 0);

-- ---------------------------------------------------------------------------
-- cache_backend_usage: the quota / storage-gauge measurement cache
-- ---------------------------------------------------------------------------
--
-- ONE ROW PER BACKEND, upserted by UpsertBackendUsage. Usage measurement is
-- decoupled from GC being enabled (spec §8, findings 7b/13): a lightweight usage
-- pass runs on its own interval even with retention disabled, so this table's
-- freshness is a property of THAT pass, not of the retention sweep.
--
-- LOGICAL bytes, not physical (spec §8): "logical" means the sum of
-- cache_objects.size_bytes this backend NAMES, full charge to every namer under
-- cross-project dedup -- the same order-independent, locally-computable property
-- the refcount trigger has. It is a cap on what the backend is CHARGED for, not on
-- what the instance stores on disk; InstancePhysicalBytes (query/gc.sql) is the
-- separate, backend-blind number an operator alerts on for disk.
--
-- ON DELETE CASCADE, unlike cache_objects' RESTRICT from cache_backends: this row
-- holds no refcounted resource, only a stale measurement, so a backend teardown
-- (which DeleteBackend already refuses while cache_objects rows exist) may take
-- its usage row with it for free.
CREATE TABLE cache_backend_usage (
    backend_id    bigint PRIMARY KEY REFERENCES cache_backends (id) ON DELETE CASCADE,
    objects_count bigint NOT NULL CHECK (objects_count >= 0),
    logical_bytes bigint NOT NULL CHECK (logical_bytes >= 0),
    measured_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- gc_runs: trigger, dry_run, hashserv_rows_deleted
-- ---------------------------------------------------------------------------

ALTER TABLE gc_runs
    -- Who started this run. Only two sources exist today (spec §9.1/§9.10): the
    -- in-process interval loop, and POST /api/v1/gc/run. Neither the boot reaper
    -- nor the shutdown finisher START a run, so neither needs a trigger value of
    -- its own.
    ADD COLUMN trigger text NOT NULL DEFAULT 'interval'
        CONSTRAINT gc_runs_trigger_known CHECK (trigger IN ('interval', 'api')),
    -- A dry run writes this row (auditable, list-able) but performs no delete --
    -- every sweep query below has a read-only mirror. dry_run is frozen at start,
    -- same as grace_period, so it cannot flip mid-run.
    ADD COLUMN dry_run boolean NOT NULL DEFAULT false,
    -- hashserv is the GC root (stage 1 of the sweep, spec §3): its own count is
    -- broken out from objects_deleted rather than folded into it, so a run's
    -- summary can answer "did the root move" separately from "how much sstate
    -- came with it".
    ADD COLUMN hashserv_rows_deleted bigint NOT NULL DEFAULT 0
        CHECK (hashserv_rows_deleted >= 0);

-- DRY RUNS DON'T HOLD THE ACTIVE SLOT (spec §7.2, finding 14). A dry run performs
-- no write and shares no snapshot-visibility hazard with a real run or with
-- another dry run, so serialising them behind ONE 'running' slot only blocks an
-- operator's `--dry-run` invocation behind an unrelated one with no safety
-- benefit. Real runs still get exactly one at a time: the predicate is
-- `status = 'running' AND NOT dry_run`, so a real run and any number of concurrent
-- dry runs coexist, but two real runs still collide on this unique index exactly
-- as before.
DROP INDEX gc_runs_single_active_idx;
CREATE UNIQUE INDEX gc_runs_single_active_idx
    ON gc_runs (status) WHERE status = 'running' AND NOT dry_run;
