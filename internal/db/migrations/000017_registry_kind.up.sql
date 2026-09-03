-- BuildKit cache export (spec 2026-09-03-buildkit-cache-export.md): a new writable
-- OCI namespace, `registry`. It adds ONLY the enum value here -- no new table, no
-- new column. The storage model is the SAME cache_objects trio the `oci` kind
-- already uses (blobs / manifests / tags), just under this kind's own backend_id;
-- (project_id, kind) is what makes it one writable namespace per project.
--
-- ADD VALUE runs outside a transaction block on older Postgres, but this project
-- targets PG18 (CLAUDE.md), where ALTER TYPE ... ADD VALUE is transaction-safe --
-- migrate's own per-file transaction wrapping is fine here.
ALTER TYPE backend_kind ADD VALUE 'registry';
