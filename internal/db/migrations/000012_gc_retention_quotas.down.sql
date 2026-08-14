-- Reverse of 000012. Order is the mirror image of the up file.

DROP INDEX gc_runs_single_active_idx;
CREATE UNIQUE INDEX gc_runs_single_active_idx ON gc_runs (status) WHERE status = 'running';

ALTER TABLE gc_runs
    DROP COLUMN trigger,
    DROP COLUMN dry_run,
    DROP COLUMN hashserv_rows_deleted;

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

SET LOCAL lock_timeout = '5s';

-- Restore the pre-M6 storage parameters exactly: 000006 set cache_objects'
-- fillfactor to 95 (insert-mostly; only /ac/ overwrites in place) and set no
-- autovacuum override; 000010 set no storage parameters at all on
-- hashserv_unihashes, so RESET (not a specific value) is the correct inverse.
ALTER TABLE cache_objects RESET (autovacuum_vacuum_scale_factor);
ALTER TABLE cache_objects SET (fillfactor = 95);
ALTER TABLE cache_objects DROP COLUMN accessed_at;

ALTER TABLE hashserv_unihashes RESET (fillfactor, autovacuum_vacuum_scale_factor);
ALTER TABLE hashserv_unihashes DROP COLUMN accessed_at;
