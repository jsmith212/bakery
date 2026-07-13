-- Browser sessions. alexedwards/scs's canonical three-column schema, and the
-- queries a hand-written scs.Store over pgxpool needs.
--
-- scs's stock postgresstore is database/sql and is therefore FORBIDDEN. The adapter
-- that consumes these lives in the auth layer and must implement scs.CtxStore, not
-- just scs.Store: scs type-asserts for the Ctx variants and silently drops context
-- if they are absent.
--
-- Sessions are NEVER on the cache hot path. Cache traffic authenticates with
-- api_keys; only the human console touches this table. The two must not be
-- conflated.
--
-- The scs contract: a missing or expired token is a clean MISS, not an error. A
-- pgx.ErrNoRows from SessionFind must become (nil, false, nil), or every request
-- from a logged-out user becomes a 500.

-- name: SessionFind :one
SELECT data FROM sessions WHERE token = $1 AND expiry > now();

-- name: SessionCommit :exec
INSERT INTO sessions (token, data, expiry)
VALUES ($1, $2, $3)
ON CONFLICT (token) DO UPDATE
   SET data = EXCLUDED.data, expiry = EXCLUDED.expiry;

-- name: SessionDelete :exec
DELETE FROM sessions WHERE token = $1;

-- name: SessionAll :many
SELECT token, data FROM sessions WHERE expiry > now();

-- Rides sessions_expiry_idx. Run from a background sweeper.
--
-- name: SessionDeleteExpired :execrows
DELETE FROM sessions WHERE expiry < now();
