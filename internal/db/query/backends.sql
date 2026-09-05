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
-- `deleting_at IS NULL` IS THE ROUTE RESOLVER'S WHOLE VIEW OF TEARDOWN (000018). A
-- marked backend must read as UNCONFIGURED here, not as a disabled or an empty one:
-- CLAUDE.md's rule is that a project/kind with no cache_backends row 404s -- it never
-- mounts a mount point it cannot serve -- and a backend the GC is actively emptying
-- is exactly that. Filtering in the QUERY rather than in the resolver is what makes
-- it structural: CachedResolver.load takes pgx.ErrNoRows as its 404, so there is no
-- flag for a caller to forget to check and no second code path where a torn-down
-- backend could still resolve.
--
-- name: GetBackend :one
SELECT id, enabled, read_auth_required, config
  FROM cache_backends
 WHERE project_id = $1 AND kind = $2 AND deleting_at IS NULL;

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
--     kind = 'registry' ALSO KEEPS the org default (buildkit-cache-export spec
--     §5): unlike oci this kind has no upstream to fall back to on eviction, so
--     `mode=max` exports run multi-GB by design and a byte quota is the
--     operator's only ceiling -- the opposite of oci's product decision, not an
--     oversight.
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
                WHEN 'registry' THEN interval '30 days'
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
-- The API calls this ONLY on a backend it has just proved empty (BackendHasObjects
-- below, in the same request): on anything else it marks deleting_at instead and
-- lets the GC's teardown stage do the emptying. A 23503 from here is therefore a
-- LOST RACE -- a build wrote an object between the probe and the delete -- and the
-- handler falls back to the mark, which is the same answer it would have given had
-- the probe seen that row.
--
-- name: DeleteBackend :execrows
DELETE FROM cache_backends WHERE id = $1;

-- ===========================================================================
-- Teardown (000018). The API marks; the GC empties and then deletes.
-- ===========================================================================

-- Does this backend still hold anything a RESTRICT foreign key would refuse to let
-- go? EXISTS, never count(*): the question is "any", cache_objects is the table sized
-- in the tens of millions, and an exact count would scan the whole backend's slice to
-- answer a boolean. hashserv is the one kind whose RESTRICT comes from somewhere else
-- entirely (hashserv_unihashes / hashserv_outhashes, 000010) and it owns no
-- cache_objects rows at all, so all three tables are asked here -- a hashserv backend
-- that answered "empty" on cache_objects alone would be deleted straight into a
-- foreign-key violation.
--
-- name: BackendHasObjects :one
SELECT (EXISTS (SELECT 1 FROM cache_objects      o WHERE o.backend_id = sqlc.arg(backend_id))
     OR EXISTS (SELECT 1 FROM hashserv_unihashes u WHERE u.backend_id = sqlc.arg(backend_id))
     OR EXISTS (SELECT 1 FROM hashserv_outhashes h WHERE h.backend_id = sqlc.arg(backend_id))
       )::boolean AS has_objects;

-- The MARK. Idempotent by construction: coalesce keeps the ORIGINAL deleting_at, so a
-- second DELETE on an already-deleting backend reports the same instant the first one
-- did rather than restarting the clock -- and the handler can answer 202 again
-- without a special case.
--
-- `enabled = false` rides along because the two facts are the same fact from a
-- different angle, and because the GC's disabled clamp (least(configured, 30d)) is
-- the right posture for a backend nobody can reach: if the teardown stage is somehow
-- not running, the retention stages still shrink it.
--
-- name: MarkBackendDeleting :one
UPDATE cache_backends
   SET deleting_at = coalesce(deleting_at, now()),
       enabled     = false
 WHERE id = $1
RETURNING *;

-- Every backend of one project, marked in one statement -- DELETE /projects/{project}
-- marks the whole set inside its own transaction, so a crash cannot leave half a
-- project torn down and half of it live.
--
-- name: MarkProjectBackendsDeleting :execrows
UPDATE cache_backends
   SET deleting_at = coalesce(deleting_at, now()),
       enabled     = false
 WHERE project_id = $1;

-- The empty ones go immediately, in the same transaction, so tearing down a project
-- whose backends were only ever configured (the common case: someone made a typo)
-- completes synchronously and answers 204 rather than parking a project in `deleting`
-- for up to one GC interval with nothing to sweep.
--
-- The anti-joins mirror BackendHasObjects exactly, evaluated at DELETE time: a
-- backend a build wrote to between the two statements simply is not deleted here and
-- stays marked, which is the outcome the mark already provides for.
--
-- name: DeleteEmptyBackendsForProject :execrows
DELETE FROM cache_backends cb
 WHERE cb.project_id = $1
   AND NOT EXISTS (SELECT 1 FROM cache_objects      o WHERE o.backend_id = cb.id)
   AND NOT EXISTS (SELECT 1 FROM hashserv_unihashes u WHERE u.backend_id = cb.id)
   AND NOT EXISTS (SELECT 1 FROM hashserv_outhashes h WHERE h.backend_id = cb.id);

-- The teardown stage's worklist: every marked backend, with the slugs its metrics
-- label on (CLAUDE.md: Prometheus labels are slugs, never ids) and the paired project
-- so a fully-emptied project can be finished in the same pass.
--
-- Riding cache_backends_deleting_idx, the partial index 000018 creates: on a healthy
-- installation this returns nothing, forever, and must cost nothing to ask.
--
-- name: ListBackendsForTeardown :many
SELECT cb.id, cb.kind, cb.project_id, p.slug AS project_slug, o.slug AS org_slug
  FROM cache_backends cb
  JOIN projects p      ON p.id = cb.project_id
  JOIN organizations o ON o.id = p.org_id
 WHERE cb.deleting_at IS NOT NULL
 ORDER BY cb.id;

-- The last step of one backend's teardown. `deleting_at IS NOT NULL` is not
-- decoration: it is what stops this statement from ever deleting a LIVE backend if a
-- caller passes the wrong id, and what makes it a no-op (0 rows, not an error) if the
-- mark was rolled back while the sweep was running.
--
-- A 23503 here means a build wrote an object after this run's snapshot was frozen --
-- the write barrier spares such a row by design -- so the caller leaves the backend
-- marked and finishes it on the next tick.
--
-- name: DeleteTornDownBackend :execrows
DELETE FROM cache_backends WHERE id = $1 AND deleting_at IS NOT NULL;

-- hashserv's teardown, and the reason it is not DeleteBatch's job: hashserv owns no
-- cache_objects rows and no blobs, so there is no refcount to decrement, no byte to
-- reclaim and no LRU entry to invalidate -- exactly the reasoning sweepHashserv
-- already applies to stages 1 and 2. Chunked (ctid keyset via a LIMITed subquery)
-- because a backend can hold millions of unihashes and one unbounded DELETE is one
-- unbounded transaction.
--
-- name: PurgeHashservUnihashesChunk :execrows
DELETE FROM hashserv_unihashes u
 WHERE u.ctid IN (
     SELECT c.ctid FROM hashserv_unihashes c
      WHERE c.backend_id = sqlc.arg(backend_id) LIMIT sqlc.arg(chunk_limit)
 );

-- name: PurgeHashservOuthashesChunk :execrows
DELETE FROM hashserv_outhashes h
 WHERE h.ctid IN (
     SELECT c.ctid FROM hashserv_outhashes c
      WHERE c.backend_id = sqlc.arg(backend_id) LIMIT sqlc.arg(chunk_limit)
 );
