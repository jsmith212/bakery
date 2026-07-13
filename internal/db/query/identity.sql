-- Users, organizations, projects and memberships: the control plane.
--
-- Everything here is a COLD path. Nothing in this file runs on a cache request:
-- route resolution is behind an in-process LRU, and cache auth is a single probe
-- on api_keys with zero joins.

-- The identity key is (issuer, subject), NEVER email: an OIDC `sub` is unique only
-- within an issuer, and email is mutable and reassignable in the IdP, so keying a
-- user on it is an account-takeover vector.
--
-- Writes site_role_OIDC, never site_role. Since 000008 site_role is GENERATED as
-- coalesce(greatest(site_role_oidc, site_role_local), 'user') -- the database
-- computes the effective role and refuses a direct write, so a login cannot
-- clobber a local site-admin grant even by accident. The reconciler owns exactly
-- one of the two sources and names only that one.
--
-- NULLIF: 'user' is the ORDINARY site role, i.e. the ABSENCE of a grant, not a
-- claim of one. Storing it would assert the IdP affirmatively claimed it, and
-- would make an ordinary user indistinguishable from one the IdP had demoted.
--
-- name: UpsertUser :one
INSERT INTO users (issuer, subject, email, display_name, site_role_oidc, last_login_at)
VALUES ($1, $2, $3, $4, NULLIF(sqlc.arg(site_role)::site_role, 'user'), now())
ON CONFLICT (issuer, subject) DO UPDATE
   SET email          = EXCLUDED.email,
       display_name   = EXCLUDED.display_name,
       site_role_oidc = EXCLUDED.site_role_oidc,
       last_login_at  = now()
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByIssuerSubject :one
SELECT * FROM users WHERE issuer = $1 AND subject = $2;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- ORG MEMBERSHIP RECONCILIATION -- the security-critical write.
--
-- Org membership is HYBRID since 000008: an OIDC half the claims own, and a local
-- half the API owns. The reconciler owns EXACTLY ONE of them. These three queries
-- run in ONE transaction and among them they name oidc_role and oidc_group and
-- nothing else -- never local_role, never granted_by, never granted_at, never
-- project_memberships. A local grant survives a login because the reconciler does
-- not name the column that holds it. That is structural, not conventional.
--
-- The DELETE cascades org_memberships -> project_memberships -> api_keys: a user
-- dropped from an OIDC group loses EVERY key they hold anywhere in that org, in one
-- statement. That cascade is the answer to "how can a self-contained API-key grant
-- ever be revoked without a live group lookup?" -- it is revoked at RECONCILIATION
-- time, not at validation time.
--
-- FAIL CLOSED, IN THE CALLER: if the groups claim is UNREADABLE (absent, or an
-- Azure AD `_claim_names` overage), do not call these AT ALL -- refuse the login.
-- An unreadable claim is not an empty keep-set; it is no answer. A genuinely empty
-- claim (`groups: []`) IS an answer and an empty keep-set is exactly right for it:
-- reconcile the OIDC half away and leave every local grant standing.

-- Removes the memberships NOTHING justifies any more: the claims have dropped them
-- and no local grant stands in their place. This is the revocation half of the
-- design, and the `local_role IS NULL` guard is what keeps it from being the
-- destruction half.
--
-- IT MUST RUN BEFORE the UPDATE below, and that order is a schema fact, not a
-- preference: `role` is GENERATED NOT NULL AS greatest(oidc_role, local_role), so
-- NULLing oidc_role on a row with no local grant makes the generated column NULL
-- and Postgres rejects the statement with 23502. The rows this DELETE removes are
-- precisely the rows that UPDATE could not have touched.
--
-- name: ReconcileOrgMembershipsDelete :execrows
DELETE FROM org_memberships
 WHERE user_id = $1
   AND org_id <> ALL(sqlc.arg(keep)::uuid[])
   AND local_role IS NULL;

-- Clears the OIDC half of the memberships the claims no longer justify but a LOCAL
-- GRANT still does. The row survives, the effective role falls back to the local
-- grant, and the user keeps their project roles and their API keys -- because they
-- are still, deliberately, a member of the org.
--
-- This is the statement a blind `DELETE ... WHERE org_id <> ALL(keep)` replaces,
-- and replacing it is data loss: it cascades away project roles and API keys the
-- user is entitled to, and re-adding the membership does not bring them back.
--
-- name: ReconcileOrgMembershipsClearOIDC :execrows
UPDATE org_memberships
   SET oidc_role = NULL, oidc_group = NULL
 WHERE user_id = $1
   AND org_id <> ALL(sqlc.arg(keep)::uuid[])
   AND local_role IS NOT NULL;

-- Writes oidc_ROLE, never role. Since 000008 `role` is GENERATED as
-- greatest(oidc_role, local_role) and refuses a direct write, so the reconciler
-- cannot clobber a local grant: it does not name the column that holds one.
--
-- oidc_group records WHICH group justified the role. It is audit, not
-- authorization: when a membership outlives an LDAP change an admin needs to know
-- which half is holding it up.
--
-- name: ReconcileOrgMembershipUpsert :exec
INSERT INTO org_memberships (user_id, org_id, oidc_role, oidc_group)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, org_id) DO UPDATE
   SET oidc_role  = EXCLUDED.oidc_role,
       oidc_group = EXCLUDED.oidc_group;

-- name: ListOrgMembershipsForUser :many
SELECT om.org_id, om.role, o.slug, o.name
  FROM org_memberships om
  JOIN organizations o ON o.id = om.org_id
 WHERE om.user_id = $1
 ORDER BY o.slug;

-- Every membership carries its PROVENANCE, both halves of it. The console renders
-- `ldap: acme-leads` beside `local: granted by jsmith`, and an admin looking at a
-- membership that outlived an LDAP revocation has to be able to see which half is
-- holding it up. A list that reported only the effective role would make a local
-- grant invisible, which is the definition of a backdoor.
--
-- granted_by is LEFT JOINed, not inner: ON DELETE SET NULL means the granting user
-- can be gone while the grant they made stands. An inner join would silently drop
-- exactly those rows -- the ones an audit most wants to see.
--
-- name: ListOrgMembers :many
SELECT om.user_id, om.role, om.oidc_role, om.oidc_group, om.local_role,
       om.granted_by, om.granted_at,
       u.email, u.display_name,
       g.email AS granted_by_email
  FROM org_memberships om
  JOIN users u ON u.id = om.user_id
  LEFT JOIN users g ON g.id = om.granted_by
 WHERE om.org_id = $1
 ORDER BY u.email;

-- name: GetOrgMembership :one
SELECT * FROM org_memberships WHERE user_id = $1 AND org_id = $2;

-- LOCAL ORG GRANTS -- the API's half, and ONLY its half.
--
-- Mirror image of the reconciler's queries above: these three name local_role,
-- granted_by and granted_at, and NEVER oidc_role or oidc_group. Between the two
-- sets, neither source can clobber the other -- not by convention, but because
-- neither statement names the other's columns. The effective `role` is GENERATED
-- from both and is not writable by anyone.

-- name: GrantOrgMembershipLocal :one
INSERT INTO org_memberships (user_id, org_id, local_role, granted_by, granted_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, org_id) DO UPDATE
   SET local_role = EXCLUDED.local_role,
       granted_by = EXCLUDED.granted_by,
       granted_at = now()
RETURNING *;

-- Clears the LOCAL half of a membership an OIDC claim ALSO justifies. The row
-- survives, the effective role falls back to the claim, and the user keeps their
-- project roles and API keys -- because they are, still, a member of the org.
--
-- The `oidc_role IS NOT NULL` guard is not belt-and-braces: `role` is GENERATED
-- NOT NULL AS greatest(oidc_role, local_role), so NULLing local_role on a row with
-- no claim would make the generated column NULL and Postgres would reject the
-- UPDATE with 23502. The guard turns that would-be 500 into "no rows", which is
-- how the caller knows the row must be DELETED instead.
--
-- name: RevokeOrgMembershipLocal :one
UPDATE org_memberships
   SET local_role = NULL, granted_by = NULL, granted_at = NULL
 WHERE user_id = $1 AND org_id = $2 AND oidc_role IS NOT NULL
RETURNING *;

-- Removes a membership NOTHING justifies once the local grant is gone. Guarded, so
-- it can never take a claim-derived membership with it: LDAP owns those and the API
-- must not be able to remove one, not even by accident.
--
-- This DELETE is the one that cascades org_memberships -> project_memberships ->
-- api_keys. That is correct and intended here: the user is leaving the org.
--
-- name: DeleteLocalOrgMembership :execrows
DELETE FROM org_memberships
 WHERE user_id = $1 AND org_id = $2 AND oidc_role IS NULL;

-- Resolves the {user} path segment when the caller names an EMAIL. Case-insensitive
-- to match users_email_lower_key, which is the uniqueness the identity model
-- actually has -- a case-sensitive lookup would miss `Marko@acme.dev` and report
-- "no such user" for a user who plainly exists.
--
-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1);

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

-- Gives a project's CREATOR the admin role on it, in the same transaction that
-- creates the project. Without this the creator can administer the project (an org
-- admin CanAdminProject) but cannot mint a key for it: api_keys carries
-- `FOREIGN KEY (user_id, project_id) REFERENCES project_memberships`, so a key for
-- a non-member CANNOT EXIST, and the scope cap refuses them with
-- `scope_exceeds_role`. That is the second half of the dead-end M1.5 abolishes.
--
-- The EXISTS guard is load-bearing. project_memberships carries
-- `FOREIGN KEY (user_id, org_id) REFERENCES org_memberships`, so this INSERT would
-- be a 23503 -- a 500 on an otherwise valid request -- for a creator who is not an
-- org member. A SITE ADMIN is exactly that: they may create a project in any org
-- without belonging to it. So the guard makes the grant a no-op for them rather
-- than an error, and they take the ordinary route (grant yourself a role) if they
-- want a key. It cannot silently skip anyone else: creating an org now MAKES you a
-- member of it.
--
-- name: GrantProjectMembershipToCreator :execrows
INSERT INTO project_memberships (user_id, project_id, org_id, role)
SELECT sqlc.arg(user_id), p.id, p.org_id, 'admin'
  FROM projects p
 WHERE p.id = sqlc.arg(project_id)
   AND EXISTS (
       SELECT 1 FROM org_memberships om
        WHERE om.user_id = sqlc.arg(user_id) AND om.org_id = p.org_id
   )
ON CONFLICT (user_id, project_id) DO NOTHING;

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
