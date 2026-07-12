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
-- name: CreateBackend :one
INSERT INTO cache_backends (project_id, kind, enabled, read_auth_required, config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateBackend :one
UPDATE cache_backends
   SET enabled = $2, read_auth_required = $3, config = $4
 WHERE id = $1
RETURNING *;

-- ON DELETE RESTRICT from cache_objects means this is refused while the backend
-- still holds objects. Teardown goes through blob.Service's chunked purge, which
-- the refcount trigger then makes arithmetically correct for free.
--
-- name: DeleteBackend :execrows
DELETE FROM cache_backends WHERE id = $1;
