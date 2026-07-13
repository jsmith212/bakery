-- Reverse of 000008. LOSSY BY NATURE, and it must be: the down migration collapses
-- two sources back into one column, so a local grant that outlived its OIDC group
-- comes back indistinguishable from a claim-derived role. There is nowhere for it
-- to go -- 000002's schema cannot represent it. That is the price of reversing.
--
-- It is therefore written to preserve the EFFECTIVE role -- what the user can
-- actually do -- rather than the provenance. Nobody loses access on a rollback.

-- ---------------------------------------------------------------------------
-- users.site_role
-- ---------------------------------------------------------------------------

ALTER TABLE users DROP CONSTRAINT users_site_local_provenance;

-- Drop the generated column, then rebuild the plain one FROM the sources -- which
-- must therefore still exist at this point. The order is load-bearing.
ALTER TABLE users DROP COLUMN site_role;
ALTER TABLE users ADD COLUMN site_role site_role NOT NULL DEFAULT 'user';
UPDATE users SET site_role = coalesce(greatest(site_role_oidc, site_role_local), 'user');

ALTER TABLE users
    DROP COLUMN site_role_oidc,
    DROP COLUMN site_role_local,
    DROP COLUMN site_granted_by,
    DROP COLUMN site_granted_at;

-- ---------------------------------------------------------------------------
-- org_memberships
-- ---------------------------------------------------------------------------

ALTER TABLE org_memberships DROP CONSTRAINT org_memberships_local_provenance;
ALTER TABLE org_memberships DROP CONSTRAINT org_memberships_has_a_source;

ALTER TABLE org_memberships DROP COLUMN role;
ALTER TABLE org_memberships ADD COLUMN role org_role;
-- has_a_source guaranteed at least one source was non-NULL on every row, so
-- greatest() is non-NULL for every surviving row and SET NOT NULL cannot fail.
-- 000002's `role` carries NO default, so none is restored.
UPDATE org_memberships SET role = greatest(oidc_role, local_role);
ALTER TABLE org_memberships ALTER COLUMN role SET NOT NULL;

ALTER TABLE org_memberships
    DROP COLUMN oidc_role,
    DROP COLUMN oidc_group,
    DROP COLUMN local_role,
    DROP COLUMN granted_by,
    DROP COLUMN granted_at;
