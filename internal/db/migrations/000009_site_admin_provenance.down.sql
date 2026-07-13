-- Dropping this loses only the AUDIT half of a claim-derived site role. The role
-- itself lives in site_role_oidc and is untouched, so nobody's authorization
-- changes -- the listing simply stops being able to name the group.
ALTER TABLE users DROP COLUMN site_oidc_group;
