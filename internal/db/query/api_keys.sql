-- API keys. Project-scoped AND per-user, read/write-scoped, stored as the SHA-256
-- of the full `bkry_<random>` token. There is no plaintext column: "shown exactly
-- once" is enforced by the schema's inability to represent the secret, not by
-- application discipline.

-- HOT 2: runs on EVERY cache request, including every HEAD of a
-- BB_NUMBER_THREADS-parallel sstate storm. ZERO JOINS, one Index Only Scan on
-- api_keys_token_sha256_key with Heap Fetches: 0 -- revoked_at and expires_at are
-- in the index's INCLUDE list precisely so the filters do not force a heap fetch.
--
-- SELF-CONTAINED, and correct anyway: it does not join project_memberships, but
-- the composite FK guarantees the membership row still exists. If the user lost the
-- org or project grant, this key row was CASCADE-deleted at reconciliation time and
-- this returns zero rows.
--
-- The caller then asserts project_id == the routed project (a uuid compare in Go)
-- and scope = 'write' for writes. It must NEVER join api_keys -> projects ->
-- organizations.
--
-- name: ValidateAPIKey :one
SELECT id, user_id, project_id, scope, expires_at
  FROM api_keys
 WHERE token_sha256 = $1
   AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at > now());

-- COALESCED, NEVER per-request. One CI machine drives a whole build with ONE key;
-- an inline `UPDATE api_keys SET last_used_at = now()` funnels thousands of
-- parallel HEADs into a row-lock convoy on the single hottest row in the database,
-- one WAL record each, to maintain a value nobody reads in real time. Run this
-- fire-and-forget from a background flusher, off the request's critical path.
--
-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now()
 WHERE id = $1
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes');

-- The membership FK means a key for a NON-MEMBER cannot be created. Not "is
-- rejected by a check the API remembers to run" -- cannot exist.
--
-- APP INVARIANT, not expressible in DDL and therefore a test's job: a key is a
-- DELEGATION of the user's authority and must never exceed it. Validation does not
-- join project_memberships (that would put a second probe on the HEAD storm), so
-- the API must cap scope <= the role's maximum AT CREATION, and must downgrade or
-- revoke a user's keys for a project IN THE SAME TRANSACTION as any role
-- downgrade. Paid once, at human speed, instead of on every cache request.
--
-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, project_id, name, token_sha256, token_prefix, scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, project_id, name, token_prefix, scope, expires_at, created_at;

-- Never returns token_sha256. The console has no use for it and nothing outside
-- ValidateAPIKey should ever hold one.
--
-- name: ListAPIKeysForProject :many
SELECT id, user_id, project_id, name, token_prefix, scope,
       expires_at, revoked_at, last_used_at, created_at
  FROM api_keys
 WHERE project_id = $1
 ORDER BY created_at DESC;

-- name: ListAPIKeysForUser :many
SELECT id, user_id, project_id, name, token_prefix, scope,
       expires_at, revoked_at, last_used_at, created_at
  FROM api_keys
 WHERE user_id = $1
 ORDER BY created_at DESC;

-- Soft, and it is the one soft-state column in the schema. The console must show
-- "revoked 3 days ago", and a revoked key's hash must stay reserved. The price is
-- one partial unique index (api_keys_active_name_key) so that revoking the key named
-- "ci" does not permanently burn the name "ci".
--
-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now()
 WHERE id = $1 AND revoked_at IS NULL;

-- Used when a project role is downgraded: revoke, in the same transaction as the
-- downgrade, every key whose scope now exceeds the role.
--
-- name: RevokeAPIKeysForMembership :execrows
UPDATE api_keys SET revoked_at = now()
 WHERE user_id = $1 AND project_id = $2 AND revoked_at IS NULL
   AND scope = ANY(sqlc.arg(scopes)::api_key_scope[]);
