-- Bakery M1.5: the hybrid role model. Org membership and site role each get TWO
-- independent sources -- OIDC claims and in-app local grants -- and an EFFECTIVE
-- role the DATABASE computes as greatest(oidc, local).
--
-- This supersedes 000002's comment that a `source` column is deliberately absent.
-- That was right for a claim-only model: a manually granted role WOULD have been
-- silently deleted by the next login, so the schema made it unrepresentable. What
-- changed is not the danger, it is the shape of the fix -- the reconciler now
-- names only the oidc_* columns, so it CANNOT clobber a local grant, and the
-- state is safe to represent.
--
-- THE SOURCES ARE COLUMNS, NEVER ROWS. project_memberships carries
--     FOREIGN KEY (user_id, org_id) REFERENCES org_memberships (user_id, org_id) CASCADE
-- and that cascade -- org_memberships -> project_memberships -> api_keys -- is what
-- revokes every API key a user holds in an org when they leave it. It requires
-- (user_id, org_id) to stay UNIQUE. A PRIMARY KEY (user_id, org_id, source) would
-- destroy that uniqueness, destroy the FK, and silently reopen the revocation
-- hole. The primary key does not change.
--
-- greatest() over an enum IS immutable and IS legal in a STORED generated column
-- (verified against postgres:18-alpine, and on the PG14 floor). greatest() ignores
-- NULLs and returns NULL only when every argument is NULL, which is exactly the
-- max-wins rule. The enums are declared in privilege order in 000001
-- ('member' < 'admin' < 'owner', 'user' < 'admin'), so enum comparison IS the
-- privilege comparison and no application code can get it wrong.

-- ---------------------------------------------------------------------------
-- org_memberships
-- ---------------------------------------------------------------------------

ALTER TABLE org_memberships
    ADD COLUMN oidc_role  org_role,   -- NULL: no claim justifies this membership
    ADD COLUMN oidc_group text,       -- which group justified it (audit)
    ADD COLUMN local_role org_role,   -- NULL: no local grant
    ADD COLUMN granted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN granted_at timestamptz;

-- Every membership that exists today is claim-derived -- 000002 made a local grant
-- unrepresentable on purpose -- so this backfill is exactly true, not a guess.
UPDATE org_memberships SET oidc_role = role;

-- Postgres cannot convert an existing column to GENERATED in place, so `role` is
-- dropped and re-added. Dropping it does NOT disturb the composite FK from
-- project_memberships: that FK is on (user_id, org_id), and `role` is not in it.
-- Do not "re-add the FK to be safe" -- re-adding it is the one operation here that
-- could actually lose the CASCADE, if it were retyped wrong.
--
-- NOT NULL is preserved deliberately: it is what keeps sqlc generating a plain
-- OrgRole instead of a nullable one, and therefore what keeps every read site in
-- the API and auth layers from having to handle a nullable effective role.
ALTER TABLE org_memberships DROP COLUMN role;
ALTER TABLE org_memberships
    ADD COLUMN role org_role NOT NULL
        GENERATED ALWAYS AS (greatest(oidc_role, local_role)) STORED;

-- A row justified by NEITHER source is garbage and must not exist: while it exists
-- it suppresses the project-membership cascade, leaving alive exactly the API keys
-- that leaving the org is supposed to revoke.
--
-- NOTE: this CHECK is logically equivalent to the NOT NULL above (role IS NULL iff
-- both sources are NULL), and Postgres reports the NOT NULL first, so this
-- constraint's name never actually appears in an error. It is kept as the
-- declarative statement of the invariant and as the backstop if NOT NULL is ever
-- relaxed. Do not write a test that asserts on its name.
ALTER TABLE org_memberships
    ADD CONSTRAINT org_memberships_has_a_source
        CHECK (oidc_role IS NOT NULL OR local_role IS NOT NULL);

-- Provenance is meaningful only for a local grant. granted_by is deliberately NOT
-- in this constraint: it is ON DELETE SET NULL, so deleting the granting user must
-- not retroactively invalidate every grant they ever made.
ALTER TABLE org_memberships
    ADD CONSTRAINT org_memberships_local_provenance
        CHECK ((local_role IS NULL) = (granted_at IS NULL));

-- ---------------------------------------------------------------------------
-- users.site_role
-- ---------------------------------------------------------------------------

ALTER TABLE users
    ADD COLUMN site_role_oidc   site_role,
    ADD COLUMN site_role_local  site_role,
    ADD COLUMN site_granted_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN site_granted_at  timestamptz;

-- NULLIF, not a straight copy: 'user' is the ORDINARY site role, i.e. the ABSENCE
-- of a site grant (000002: "no site role and the ordinary site role are the same
-- thing, so there is nothing for a NULL to mean"). Copying it into site_role_oidc
-- would assert that the IdP affirmatively claimed it, which is not what the old
-- column meant. Only 'admin' is a real claim.
UPDATE users SET site_role_oidc = NULLIF(site_role, 'user');

-- ASYMMETRIC with org_memberships ON PURPOSE, and this is the subtle one: there is
-- no users_has_a_source CHECK, because a user with NEITHER source is not garbage --
-- they are an ordinary user. So the generated column COALESCES to 'user' and stays
-- NOT NULL, preserving 000002's contract that site_role is never NULL. A plain
-- greatest() would compute NULL for every ordinary user and this migration would
-- fail on its own NOT NULL.
--
-- An org membership with no source must not exist; a user with no site grant must.
ALTER TABLE users DROP COLUMN site_role;
ALTER TABLE users
    ADD COLUMN site_role site_role NOT NULL
        GENERATED ALWAYS AS (coalesce(greatest(site_role_oidc, site_role_local), 'user')) STORED;

ALTER TABLE users
    ADD CONSTRAINT users_site_local_provenance
        CHECK ((site_role_local IS NULL) = (site_granted_at IS NULL));
