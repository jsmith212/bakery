-- Personal access tokens (`bkru_`). See 000015_user_tokens.up.sql for the schema
-- and for why the authorization epoch exists.
--
-- The HOT probe is NOT here. It lives as raw SQL beside the api_keys probe in
-- internal/auth (usertoken.go, validateUserTokenSQL), for the same reason
-- ValidateAPIKey's Go caller does: the validator sits behind a small consumer-side
-- interface so the hot path can be driven against a fake as well as a real
-- Postgres, and it re-affirms the hash equality in constant time in our own
-- address space. Everything below is the COLD control plane.

-- name: CreateUserToken :one
INSERT INTO user_tokens (user_id, name, token_sha256, token_prefix, max_scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, name, token_prefix, max_scope, expires_at, created_at;

-- Never returns token_sha256. Nothing outside the validator should ever hold one,
-- and there is no plaintext column for anything to return.
--
-- name: ListUserTokensForUser :many
SELECT id, user_id, name, token_prefix, max_scope,
       expires_at, revoked_at, last_used_at, created_at
  FROM user_tokens
 WHERE user_id = $1
 ORDER BY created_at DESC;

-- Scoped to the OWNER, not just to the id. The {id} in the path is caller-supplied
-- and `RevokeUserToken` would otherwise revoke any token in the installation given
-- only its uuid -- the textbook IDOR, in three lines of reasonable-looking code.
-- The user_id predicate makes the ownership check part of the statement rather
-- than something a handler has to remember.
--
-- name: RevokeUserToken :execrows
UPDATE user_tokens SET revoked_at = now()
 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: GetUserTokenForUser :one
SELECT id, user_id, name, token_prefix, max_scope,
       expires_at, revoked_at, last_used_at, created_at
  FROM user_tokens
 WHERE id = $1 AND user_id = $2;

-- COALESCED, NEVER per-request -- see api_keys.sql's TouchAPIKey for the row-lock
-- convoy this avoids. Batched: one CI machine drives a whole build with ONE token.
--
-- name: TouchUserTokens :exec
UPDATE user_tokens SET last_used_at = now()
 WHERE id = ANY($1::uuid[])
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes');
