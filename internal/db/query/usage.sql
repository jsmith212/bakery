-- SPA->API wiring wave, B2 + B3 (docs/design/specs/2026-08-15-spa-api-wiring.md):
-- reading cache_backend_usage (the M6 quota/storage gauge) and browsing
-- cache_objects for the console. cache_backend_usage has had exactly one writer
-- since 000012 (UpsertBackendUsage, query/gc.sql) and no reader anywhere in the
-- codebase until these two queries -- both LEFT JOIN it, deliberately, because a
-- backend with no row yet (never measured) and a backend genuinely idle at zero
-- bytes are different facts and the API must be able to tell them apart on the
-- wire (measured_at NULL vs measured_at present).

-- GetOrgUsageByProject is B2a: one row PER PROJECT, summed across every backend
-- the project has configured.
--
-- FROM projects, not FROM cache_backends: a project with no backends configured
-- yet must still appear (at 0 objects / 0 bytes / unmeasured), because the org
-- projects screen lists every project in the org, not only the ones with
-- something to report. Both joins are therefore LEFT.
--
-- objects_count/logical_bytes: SUM() over an all-NULL group is NULL, coalesced to
-- 0 -- an honest zero for "no backend has ever reported usage here", same as "no
-- backend configured at all". measured_at: MIN() ignores NULLs and returns NULL
-- only when EVERY backend's usage row is missing, which is the caller's signal
-- that this project's total is not merely stale but has literally never been
-- measured. MIN rather than MAX is the conservative choice: the total is only as
-- fresh as its stalest contributing measurement, and reporting the newest one
-- would overstate how current the SUM is.
--
-- name: GetOrgUsageByProject :many
SELECT p.slug                                AS project_slug,
       coalesce(sum(u.objects_count), 0)::bigint AS objects_count,
       coalesce(sum(u.logical_bytes), 0)::bigint AS logical_bytes,
       min(u.measured_at)::timestamptz       AS measured_at
  FROM projects p
  LEFT JOIN cache_backends cb ON cb.project_id = p.id
  LEFT JOIN cache_backend_usage u ON u.backend_id = cb.id
 WHERE p.org_id = sqlc.arg(org_id)
 GROUP BY p.id, p.slug
 ORDER BY p.slug;

-- GetProjectBackendUsage is B2b: one row PER BACKEND (kind), unaggregated -- the
-- project detail / overview screen wants the per-kind breakdown, not a sum, and
-- quota_bytes/retention_window travel alongside so the console can render "212
-- GB / 500 GB cap" without a second round trip.
--
-- Plain LEFT JOIN, no GROUP BY: cache_backend_usage.backend_id is that table's
-- PRIMARY KEY, so at most one usage row exists per backend and this is a
-- one-to-zero-or-one join, not a fan-out.
--
-- name: GetProjectBackendUsage :many
SELECT cb.kind                AS kind,
       u.objects_count        AS objects_count,
       u.logical_bytes        AS logical_bytes,
       u.measured_at          AS measured_at,
       cb.quota_bytes         AS quota_bytes,
       cb.retention_window    AS retention_window
  FROM cache_backends cb
  LEFT JOIN cache_backend_usage u ON u.backend_id = cb.id
 WHERE cb.project_id = sqlc.arg(project_id)
 ORDER BY cb.kind;

-- ListCacheObjectsForBrowse is B3: a keyset page over ONE backend's cache_objects,
-- optionally scoped to a namespace and a key prefix.
--
-- KEYSET ON (namespace, key), never OFFSET -- cache_objects is the one table in
-- this schema sized in the tens of millions of rows (spec references, and
-- internal/gc's own ScanObjectsForGC carries the identical warning). after_key
-- empty ('', the sentinel ScanObjectsForGC also uses -- key's own CHECK forbids a
-- real empty key) means "from the beginning of the namespace".
--
-- namespace is matched exactly and is never optional in this query -- the API layer
-- defaults an absent ?namespace= to "" (nsDefault, sstate/downloads' own
-- namespace) before calling this, so every call names one. That keeps the
-- (backend_id, namespace, key) comparison a genuine cache_objects_pkey range
-- scan -- widening it to "any namespace" here would turn the leading-column
-- equality into a range condition and cost the index prefix.
--
-- prefix_upper is NULLABLE and computed IN GO (api.prefixUpperBound): a proper
-- "starts with" range scan needs an exclusive UPPER bound one unit past the
-- prefix's last byte, which a LIKE pattern cannot express as an index condition
-- under the key column's default collation (see query/objects.sql's
-- ListObjectKeysByPrefix, which accepts the LIKE seq-scan for its own,
-- much-smaller namespace on exactly this reasoning). NULL means "no prefix
-- filter" -- every key from after_key onward.
--
-- name: ListCacheObjectsForBrowse :many
SELECT namespace, key, digest, size_bytes, created_at, accessed_at
  FROM cache_objects
 WHERE backend_id = sqlc.arg(backend_id)
   AND namespace  = sqlc.arg(namespace)
   AND key        > sqlc.arg(after_key)::text
   AND key        >= sqlc.arg(prefix)::text
   AND (sqlc.narg(prefix_upper)::text IS NULL OR key < sqlc.narg(prefix_upper)::text)
 ORDER BY key
 LIMIT sqlc.arg(page_limit);
