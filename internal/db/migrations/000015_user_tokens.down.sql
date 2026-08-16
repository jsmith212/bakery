DROP TRIGGER IF EXISTS users_site_role_bumps_authz_epoch ON users;
DROP TRIGGER IF EXISTS project_memberships_update_bumps_authz_epoch ON project_memberships;
DROP TRIGGER IF EXISTS project_memberships_bump_authz_epoch ON project_memberships;
DROP TRIGGER IF EXISTS org_memberships_update_bumps_authz_epoch ON org_memberships;
DROP TRIGGER IF EXISTS org_memberships_bump_authz_epoch ON org_memberships;
DROP FUNCTION IF EXISTS bakery_bump_own_authz_epoch();
DROP FUNCTION IF EXISTS bakery_bump_authz_epoch();
ALTER TABLE users DROP COLUMN IF EXISTS authz_epoch;
DROP TABLE IF EXISTS user_tokens;
