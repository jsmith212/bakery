-- Bakery M1: identity. HARD DELETE everywhere (see 000006 for why soft delete on
-- orgs/projects is actively dangerous: it would leave blob refcounts pinned).

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The identity key is (issuer, subject), NEVER email. An OIDC `sub` is unique
    -- only WITHIN an issuer, and email is mutable and reassignable in the IdP --
    -- keying a user on it is an account-takeover vector. Storing the issuer also
    -- lets DEV_LOGIN's synthetic users (issuer 'dev') coexist with the real IdP's
    -- with no special-case column.
    issuer        text NOT NULL CHECK (length(btrim(issuer)) > 0),
    subject       text NOT NULL CHECK (length(btrim(subject)) > 0),
    email         text NOT NULL CHECK (length(btrim(email)) > 0),
    display_name  text NOT NULL DEFAULT '',
    -- Reconciled from OIDC group claims on EVERY login. Never edited in-app.
    -- NOT NULL + default: "no site role" and "the ordinary site role" are the same
    -- thing, so there is nothing for a NULL to mean.
    site_role     site_role NOT NULL DEFAULT 'user',
    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_issuer_subject_key UNIQUE (issuer, subject)
);
-- One human, one row. Case-insensitive: IdPs are inconsistent about email case.
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));
CREATE TRIGGER users_touch BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text NOT NULL CONSTRAINT organizations_slug_ok CHECK (bakery_slug_ok(slug)),
    name       text NOT NULL CHECK (length(btrim(name)) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organizations_slug_key UNIQUE (slug)
);
CREATE TRIGGER organizations_touch BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- RESTRICT, not CASCADE. A project owns cache_backends, which own
    -- cache_objects, which hold blob refcounts, which pin bytes on disk. A cascade
    -- would drop the metadata WITHOUT decrementing a single refcount, pinning the
    -- bytes forever. The database refuses the destructive shortcut and forces
    -- deletion through blob.Service's chunked purge.
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE RESTRICT,
    slug       text NOT NULL CONSTRAINT projects_slug_ok CHECK (bakery_slug_ok(slug)),
    name       text NOT NULL CHECK (length(btrim(name)) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT projects_org_id_slug_key UNIQUE (org_id, slug),
    -- NOT redundant: this is the FK target that lets project_memberships carry a
    -- provably-correct org_id. Without it the composite FK in project_memberships
    -- cannot exist.
    CONSTRAINT projects_id_org_id_key UNIQUE (id, org_id)
);
CREATE TRIGGER projects_touch BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

-- Org roles are 100% derived from OIDC group claims and reconciled on EVERY login.
-- There is deliberately NO `source` column: a manually granted org role would be
-- silently deleted by the user's next login, so it must be unrepresentable.
CREATE TABLE org_memberships (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    role       org_role NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, org_id),
    -- FK target for project_memberships' composite FK.
    CONSTRAINT org_memberships_user_id_org_id_key UNIQUE (user_id, org_id)
);
-- PostgreSQL does NOT index the referencing side of an FK. Without this, every
-- organizations DELETE seq-scans this table.
CREATE INDEX org_memberships_org_id_idx ON org_memberships (org_id);
CREATE TRIGGER org_memberships_touch BEFORE UPDATE ON org_memberships
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

-- Project roles are managed IN-APP. The OIDC reconciler must never touch this table.
--
-- THE LOAD-BEARING STRUCTURE. org_id is carried here even though it is derivable
-- from project_id, and that is not a normalization violation because it is not free
-- to vary:
--   FK (project_id, org_id) -> projects (id, org_id)
--       pins org_id to EXACTLY the project's org. It cannot disagree.
--   FK (user_id, org_id) -> org_memberships (user_id, org_id)
--       makes "a project member is an org member" a fact the DATABASE enforces.
-- Together: when login reconciliation deletes an org membership (the user left the
-- OIDC group), that user's project memberships in that org go with it -- and 000004's
-- API keys go with those. ONE DELETE revokes every key the user holds in that org.
-- That is what makes a self-contained (join-free) API key grant safe to trust.
CREATE TABLE project_memberships (
    user_id    uuid NOT NULL,
    project_id uuid NOT NULL,
    org_id     uuid NOT NULL,
    role       project_role NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, project_id),
    CONSTRAINT project_memberships_project_fk
        FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id)
        ON DELETE CASCADE,
    CONSTRAINT project_memberships_org_membership_fk
        FOREIGN KEY (user_id, org_id) REFERENCES org_memberships (user_id, org_id)
        ON DELETE CASCADE
);
-- "List the members of project X", and the referencing side of the projects FK.
CREATE INDEX project_memberships_project_id_org_id_idx
    ON project_memberships (project_id, org_id);
-- The referencing side of the org_memberships FK. The PK leads with user_id but not
-- org_id, so this is genuinely needed and covered by nothing above.
CREATE INDEX project_memberships_user_id_org_id_idx
    ON project_memberships (user_id, org_id);
CREATE TRIGGER project_memberships_touch BEFORE UPDATE ON project_memberships
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();
