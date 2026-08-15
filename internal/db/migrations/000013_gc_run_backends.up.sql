-- SPA->API wiring wave, B7 (docs/design/specs/2026-08-15-spa-api-wiring.md):
-- per-backend GC attribution, so an org viewer can see what a sweep did to
-- THEIR projects without the site-admin-only /gc screen.
--
-- ONE ROW PER (run, backend) THE SWEEP ACTUALLY VISITED. "Visited" means the
-- engine reached that backend's own summary point in internal/gc/sweep.go --
-- sweepHashserv or sweepBackend ran its stages -- not merely that a
-- cache_backends row existed. A backend the plan declined (no stages, neither a
-- window nor a quota configured, or refused by the sstate coverage guard) gets
-- NO row, on purpose: the table's whole value is that "swept, nothing eligible"
-- (a row with objects_deleted = 0 and bytes_freed = 0) is distinguishable from
-- "not swept this run" (no row at all), and writing a zero row for a backend
-- that was never scanned would erase that distinction.
--
-- WRITTEN ONLY FOR A REAL RUN. A dry run deletes nothing and a usage-only run
-- (MeasureUsage) never calls sweepHashserv/sweepBackend at all -- it goes
-- through its own read-only measureAll loop -- so neither path reaches the
-- write. See sweep.go's publishRunBackend for exactly where.
CREATE TABLE gc_run_backends (
    -- gc_runs rows are never deleted (they are the audit trail this table
    -- extends), so there is no ON DELETE clause to choose on this side.
    run_id          bigint NOT NULL REFERENCES gc_runs (id),
    -- ON DELETE CASCADE, the same convention cache_backend_usage (000012) uses:
    -- this row holds no refcounted resource, only a historical attribution, so a
    -- backend teardown (already refused by cache_objects' own RESTRICT while it
    -- holds objects) may take its old activity rows with it for free.
    backend_id      bigint NOT NULL REFERENCES cache_backends (id) ON DELETE CASCADE,

    objects_deleted bigint NOT NULL DEFAULT 0 CHECK (objects_deleted >= 0),

    -- LOGICAL bytes, the same convention cache_backend_usage.logical_bytes
    -- (000012) and the quota histogram (internal/gc/quota.go) use: the sum of
    -- size_bytes the rows THIS backend named carried at delete time, full charge
    -- to every namer under cross-project dedup. It is not a physical-reclaim
    -- figure -- a deduped blob can still be live under another namer, and the
    -- actual unlink happens later, instance-wide, in Layer B -- and it is not
    -- attempted for every stage: internal/gc/sweep.go's stage 8
    -- (SweepUnreferencedManifests) deletes without reading size_bytes back, so an
    -- OCI backend's manifest deletions are undercounted here by design rather
    -- than plumbed through a delete statement whose write-barrier correctness is
    -- deliberately not touched for a reporting figure.
    bytes_freed     bigint NOT NULL DEFAULT 0 CHECK (bytes_freed >= 0),

    PRIMARY KEY (run_id, backend_id)
);
