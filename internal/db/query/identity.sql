-- Users, organizations, projects and memberships: the control plane.
--
-- Everything here is a COLD path. Nothing in this file runs on a cache request:
-- route resolution is behind an in-process LRU, and cache auth is a single probe
-- on api_keys with zero joins.

-- The identity key is (issuer, subject), NEVER email: an OIDC `sub` is unique only
-- within an issuer, and email is mutable and reassignable in the IdP, so keying a
-- user on it is an account-takeover vector.
--
-- name: UpsertUser :one
INSERT INTO users (issuer, subject, email, display_name, site_role, last_login_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (issuer, subject) DO UPDATE
   SET email         = EXCLUDED.email,
       display_name  = EXCLUDED.display_name,
       site_role     = EXCLUDED.site_role,
       last_login_at = now()
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByIssuerSubject :one
SELECT * FROM users WHERE issuer = $1 AND subject = $2;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- ORG MEMBERSHIP RECONCILIATION -- the security-critical write. Org roles are
-- 100% claim-derived, so the reconciler owns the whole set for that user. Run both
-- statements in ONE transaction.
--
-- The DELETE cascades org_memberships -> project_memberships -> api_keys: a user
-- dropped from an OIDC group loses EVERY key they hold anywhere in that org, in one
-- statement. That cascade is the answer to "how can a self-contained API-key grant
-- ever be revoked without a live group lookup?" -- it is revoked at RECONCILIATION
-- time, not at validation time.
--
-- FAIL CLOSED: if the group claim is ABSENT or maps to ZERO orgs, do NOT run this
-- with an empty array -- REJECT THE LOGIN. Azure AD truncates >200 groups into a
-- _claim_names overage, and an empty keep-set here would silently and irreversibly
-- wipe every project role and API key the user has. Re-adding the org membership
-- does NOT restore them. This guard is mandatory.
--
-- name: ReconcileOrgMembershipsRemove :execrows
DELETE FROM org_memberships
 WHERE user_id = $1 AND org_id <> ALL(sqlc.arg(keep)::uuid[]);

-- name: ReconcileOrgMembershipUpsert :exec
INSERT INTO org_memberships (user_id, org_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, org_id) DO UPDATE SET role = EXCLUDED.role;

-- name: ListOrgMembershipsForUser :many
SELECT om.org_id, om.role, o.slug, o.name
  FROM org_memberships om
  JOIN organizations o ON o.id = om.org_id
 WHERE om.user_id = $1
 ORDER BY o.slug;

-- name: ListOrgMembers :many
SELECT om.user_id, om.role, u.email, u.display_name
  FROM org_memberships om
  JOIN users u ON u.id = om.user_id
 WHERE om.org_id = $1
 ORDER BY u.email;

-- name: GetOrgMembership :one
SELECT * FROM org_memberships WHERE user_id = $1 AND org_id = $2;

-- The slug CHECK calls bakery_slug_ok, so a reserved or malformed slug is refused
-- here no matter which writer proposes it. internal/slug mirrors the rule in Go
-- only so the API can render a friendly 400 instead of surfacing a 23514.
--
-- name: CreateOrganization :one
INSERT INTO organizations (slug, name) VALUES ($1, $2) RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = $1;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: ListOrganizations :many
SELECT * FROM organizations ORDER BY slug;

-- name: UpdateOrganization :one
UPDATE organizations SET name = $2 WHERE id = $1 RETURNING *;

-- ON DELETE RESTRICT all the way down means this is refused while the org still
-- has projects. That is deliberate: a CASCADE would drop metadata without
-- decrementing a single blob refcount, pinning the bytes forever with a count
-- nobody will ever decrement.
--
-- name: DeleteOrganization :execrows
DELETE FROM organizations WHERE id = $1;

-- name: CreateProject :one
INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $3) RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1;

-- name: ListProjectsForOrg :many
SELECT * FROM projects WHERE org_id = $1 ORDER BY slug;

-- name: UpdateProject :one
UPDATE projects SET name = $2 WHERE id = $1 RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE id = $1;

-- COLD: route-cache fill only, then ~never again. Two index probes
-- (organizations_slug_key, then projects_org_id_slug_key). Deliberately NOT
-- denormalised into one covering index: that would need a slug-immutability
-- trigger to stay safe, which forecloses renames -- a product decision the schema
-- must not make.
--
-- name: ResolveRoute :one
SELECT p.id AS project_id, p.org_id
  FROM projects p
  JOIN organizations o ON o.id = p.org_id
 WHERE o.slug = $1 AND p.slug = $2;

-- Project roles are managed IN-APP. The OIDC reconciler must NEVER touch this
-- table.
--
-- The composite FKs make this insert refuse a user who is not an org member, and
-- pin org_id to exactly the project's org. Both facts are enforced by the database,
-- not remembered by the API.
--
-- name: UpsertProjectMembership :one
INSERT INTO project_memberships (user_id, project_id, org_id, role)
SELECT $1, p.id, p.org_id, $3
  FROM projects p
 WHERE p.id = $2
ON CONFLICT (user_id, project_id) DO UPDATE SET role = EXCLUDED.role
RETURNING *;

-- name: GetProjectMembership :one
SELECT * FROM project_memberships WHERE user_id = $1 AND project_id = $2;

-- name: ListProjectMembers :many
SELECT pm.user_id, pm.role, u.email, u.display_name
  FROM project_memberships pm
  JOIN users u ON u.id = pm.user_id
 WHERE pm.project_id = $1
 ORDER BY u.email;

-- name: ListProjectMembershipsForUser :many
SELECT pm.project_id, pm.role, p.slug AS project_slug, o.slug AS org_slug
  FROM project_memberships pm
  JOIN projects p ON p.id = pm.project_id
  JOIN organizations o ON o.id = p.org_id
 WHERE pm.user_id = $1
 ORDER BY o.slug, p.slug;

-- Cascades into api_keys: removing someone from a project deletes their keys for
-- it. The API never has to remember to.
--
-- name: DeleteProjectMembership :execrows
DELETE FROM project_memberships WHERE user_id = $1 AND project_id = $2;
