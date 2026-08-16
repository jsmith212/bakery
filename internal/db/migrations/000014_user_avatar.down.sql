-- Dropping this loses only a rendering nicety. Authorization is untouched --
-- avatar_url is never read by anything on an authz path.
ALTER TABLE users DROP COLUMN avatar_url;
