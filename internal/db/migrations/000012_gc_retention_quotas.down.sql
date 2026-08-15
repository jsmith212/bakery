-- Reverse of 000012. Order is the mirror image of the up file.

-- LOCK_TIMEOUT AT THE TOP (R6#13), not tucked in front of the last two ALTERs.
-- `SET LOCAL` scopes to the CURRENT TRANSACTION, and migrate runs this whole file
-- as one transaction (per-file, per up.sql's own comment) -- so a lock_timeout set
-- partway down the file leaves every statement ABOVE it (the gc_runs ALTER, both
-- cache_backends ALTERs, the DROP INDEX/CREATE INDEX pair) taking their ACCESS
-- EXCLUSIVE locks with NO timeout at all. Those earlier statements hit the exact
-- same tables (cache_backends, gc_runs) the up.sql's own lock_timeout comment
-- worries about -- the sstate HEAD storm and the REAPI FindMissingBlobs storm
-- never touch gc_runs or cache_backends directly, but a booting instance under
-- --allow-multi-instance racing an already-serving one could still hold a lock
-- this file's early statements wait on, exactly as up.sql's comment describes.
-- Setting it once, first, covers every statement below for the rest of the
-- transaction.
SET LOCAL lock_timeout = '5s';

DROP INDEX gc_runs_single_active_idx;
CREATE UNIQUE INDEX gc_runs_single_active_idx ON gc_runs (status) WHERE status = 'running';

ALTER TABLE gc_runs
    DROP COLUMN trigger,
    DROP COLUMN dry_run,
    DROP COLUMN hashserv_rows_deleted;

-- Mirrors gc_state's creation position in the up file: created AFTER
-- cache_backend_usage there, so dropped BEFORE it here.
DROP TABLE IF EXISTS gc_state;

DROP TABLE IF EXISTS cache_backend_usage;

ALTER TABLE organizations
    DROP COLUMN default_retention_window,
    DROP COLUMN default_quota_bytes;

ALTER TABLE cache_backends
    DROP CONSTRAINT cache_backends_retention_window_positive,
    DROP CONSTRAINT cache_backends_quota_bytes_positive,
    DROP CONSTRAINT cache_backends_hashserv_no_quota;

ALTER TABLE cache_backends
    DROP COLUMN retention_window,
    DROP COLUMN quota_bytes;

-- Restore the pre-M6 storage parameters exactly: 000006 set cache_objects'
-- fillfactor to 95 (insert-mostly; only /ac/ overwrites in place) and set no
-- autovacuum override; 000010 set no storage parameters at all on
-- hashserv_unihashes, so RESET (not a specific value) is the correct inverse.
ALTER TABLE cache_objects RESET (autovacuum_vacuum_scale_factor);
ALTER TABLE cache_objects SET (fillfactor = 95);
ALTER TABLE cache_objects DROP COLUMN accessed_at;

ALTER TABLE hashserv_unihashes RESET (fillfactor, autovacuum_vacuum_scale_factor);
ALTER TABLE hashserv_unihashes DROP COLUMN accessed_at;
