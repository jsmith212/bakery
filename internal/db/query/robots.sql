-- Robots and their org tokens (`bkro_`). See 000016_robots.up.sql for why these
-- are separate tables and not `users` rows.
--
-- NOTHING IN THIS FILE IS REACHABLE FROM THE LOGIN PATH, and that is the design.
-- The reconciler's statements live in identity.sql and name neither `robots` nor
-- `org_tokens`; TestReconcileNeverNamesRobotTables asserts it by reading the query
-- files rather than by trusting the call graph.
--
-- As with user_tokens.sql, the HOT probe is not here: it is raw SQL in
-- internal/auth (orgtoken.go, validateOrgTokenSQL). Everything below is cold.

-- name: CreateRobot :one
INSERT INTO robots (org_id, name, description, created_by, created_by_email)
VALUES ($1, $2, $3, sqlc.narg(created_by), sqlc.arg(created_by_email))
RETURNING *;

-- name: GetRobotForOrg :one
SELECT * FROM robots WHERE id = $1 AND org_id = $2;

-- Scoped by org, always. The org came from the guard's resolved scope, so a robot
-- id from another tenant simply is not in this list and 404s.
--
-- name: ListRobotsForOrg :many
SELECT * FROM robots WHERE org_id = $1 ORDER BY name;

-- Cascades org_tokens: deleting a robot revokes every token it ever held, in one
-- statement, with no application code in the loop.
--
-- name: DeleteRobotForOrg :execrows
DELETE FROM robots WHERE id = $1 AND org_id = $2;

-- org_id is written from the ROBOT's row, not from a parameter, so the
-- denormalized copy in org_tokens can never disagree with robots.org_id.
--
-- name: CreateOrgToken :one
INSERT INTO org_tokens (robot_id, org_id, name, token_sha256, token_prefix,
                        scope, expires_at, created_by, created_by_email)
SELECT r.id, r.org_id, sqlc.arg(name), sqlc.arg(token_sha256), sqlc.arg(token_prefix),
       sqlc.arg(scope), sqlc.arg(expires_at), sqlc.narg(created_by), sqlc.arg(created_by_email)
  FROM robots r
 WHERE r.id = sqlc.arg(robot_id) AND r.org_id = sqlc.arg(org_id)
RETURNING id, robot_id, org_id, name, token_prefix, scope, expires_at,
          created_by, created_by_email, created_at;

-- name: ListOrgTokensForOrg :many
SELECT id, robot_id, org_id, name, token_prefix, scope,
       expires_at, revoked_at, last_used_at,
       created_by, created_by_email, created_at
  FROM org_tokens
 WHERE org_id = $1
 ORDER BY created_at DESC;

-- Scoped by robot AND org, for the same IDOR reason as RevokeUserToken.
--
-- name: RevokeOrgToken :execrows
UPDATE org_tokens SET revoked_at = now()
 WHERE id = $1 AND robot_id = $2 AND org_id = $3 AND revoked_at IS NULL;

-- COALESCED, NEVER per-request. See api_keys.sql's TouchAPIKey.
--
-- name: TouchOrgTokens :exec
UPDATE org_tokens SET last_used_at = now()
 WHERE id = ANY($1::uuid[])
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes');
