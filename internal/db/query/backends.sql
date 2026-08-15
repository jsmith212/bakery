-- Cache backends: the routing/metadata anchor. M1 ships NO backend implementation;
-- this is what blob.Service and every M2..M5 backend hang off.
--
-- UNIQUE (project_id, kind) is the routing grammar itself:
-- /cache/{org}/{project}/sstate/... names exactly ONE mount. It is also what makes
-- the sstate <-> hashserv coupling 1:1 by construction -- without it, "which
-- hashserv roots which sstate?" has no answer and the M3 GC is structurally
-- impossible to write correctly.

-- COLD: route-cache fill only. One probe on cache_backends_project_id_kind_key.
--
-- name: GetBackend :one
SELECT id, enabled, read_auth_required, config
  FROM cache_backends
 WHERE project_id = $1 AND kind = $2;

-- name: GetBackendByID :one
SELECT * FROM cache_backends WHERE id = $1;

-- name: ListBackendsForProject :many
SELECT * FROM cache_backends WHERE project_id = $1 ORDER BY kind;

-- read_auth_required, never write_auth_required: reads may be opened up per
-- backend, but WRITES ALWAYS REQUIRE A KEY. "Unauthenticated writes" -- a
-- cache-poisoning vector -- is not a state this database can represent.
--
-- SEEDS retention_window / quota_bytes FROM THE ORG DEFAULTS (R6#2/R7#4). Before
-- this, a freshly-created backend's retention_window and quota_bytes came back
-- NULL always -- 000012's opinionated seeding UPDATE only ever ran ONCE, against
-- rows that existed at migration time, so any backend created afterwards silently
-- fell outside "retention ships ON, opinionated" (spec §1.1) until a human
-- hand-ran the same UPDATE the migration already knows how to do. This computes
-- the SAME opinionated defaults at INSERT time, via scalar subqueries against the
-- org this backend's project belongs to (NOT a JOIN in the INSERT's own FROM/
-- SELECT list, so an invalid project_id still fails the cache_backends_project_id
-- FK the way it always has, rather than silently inserting zero rows):
--
--   retention_window = COALESCE(org.default_retention_window, <per-kind default>)
--     EXCEPT kind = 'downloads', which is hard-NULLed regardless of an org
--     default (product decision 2, spec §1.2): downloads is an ARCHIVE, not a
--     cache, and an org-wide default must never silently start expiring
--     premirror tarballs an operator never asked to be evictable.
--   quota_bytes = org.default_quota_bytes EXCEPT:
--     kind = 'hashserv' -- cache_backends_hashserv_no_quota CHECK forbids a
--                          non-NULL value outright (hashserv has no
--                          cache_objects rows to charge a quota against, so even
--                          inheriting a non-NULL org default here would abort
--                          the INSERT with a constraint violation instead of the
--                          backend simply not getting a quota).
--     kind = 'oci'      -- product decision 3 (spec §1.3): a pull-through proxy
--                          is bounded by its retention window, not a byte quota,
--                          by design -- inheriting the org default would
--                          silently turn on enforcement the product decision
--                          explicitly declined.
--     downloads KEEPS the org default (spec §1.2: only its retention_window is
--     archived; its quota stays advisory-only, which the console renders, not
--     forbidden the way hashserv's and oci's are).
--
-- name: CreateBackend :one
WITH org AS (
    SELECT o.default_retention_window, o.default_quota_bytes
      FROM projects p
      JOIN organizations o ON o.id = p.org_id
     WHERE p.id = sqlc.arg(project_id)
)
INSERT INTO cache_backends
    (project_id, kind, enabled, read_auth_required, config, retention_window, quota_bytes)
VALUES (
    sqlc.arg(project_id), sqlc.arg(kind), sqlc.arg(enabled), sqlc.arg(read_auth_required), sqlc.arg(config),
    CASE
        WHEN sqlc.arg(kind)::backend_kind = 'downloads' THEN NULL
        ELSE coalesce(
            (SELECT default_retention_window FROM org),
            CASE sqlc.arg(kind)::backend_kind
                WHEN 'sstate'   THEN interval '90 days'
                WHEN 'hashserv' THEN interval '90 days'
                WHEN 'bazel'    THEN interval '30 days'
                WHEN 'oci'      THEN interval '30 days'
                ELSE NULL
            END)
    END,
    CASE
        WHEN sqlc.arg(kind)::backend_kind IN ('hashserv', 'oci') THEN NULL
        ELSE (SELECT default_quota_bytes FROM org)
    END
)
RETURNING *;

-- Extended with retention_window / quota_bytes, both sqlc.narg (spec §7 delta):
-- a PLAIN nullable UPDATE, not a COALESCE-guarded one -- passing a value SETS the
-- column, passing NULL CLEARS it back to "retain forever" / "no cap", and there
-- is deliberately no third "leave this column alone" wire state at THIS layer.
-- The read-modify-write that already gives enabled/read_auth_required/config
-- their PATCH semantics (the API handler reads the CURRENT row and passes back
-- either the request's value or the backend's own current one, boolOr-style) is
-- exactly what a future caller wanting "leave alone" for these two columns must
-- do too -- adding that tri-state INSIDE the query would need a sentinel, and a
-- sentinel is exactly the kind of magic value a nullable interval/bigint column
-- (where NULL is already a legitimate, meaningful "no window"/"no cap") does not
-- have room for.
--
-- name: UpdateBackend :one
UPDATE cache_backends
   SET enabled            = $2,
       read_auth_required = $3,
       config              = $4,
       retention_window    = sqlc.narg(retention_window),
       quota_bytes         = sqlc.narg(quota_bytes)
 WHERE id = $1
RETURNING *;

-- ON DELETE RESTRICT from cache_objects means this is refused while the backend
-- still holds objects. Teardown goes through blob.Service's chunked purge, which
-- the refcount trigger then makes arithmetically correct for free.
--
-- name: DeleteBackend :execrows
DELETE FROM cache_backends WHERE id = $1;
