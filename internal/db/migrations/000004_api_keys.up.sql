-- Bakery M1: API keys.
--
-- THE CENTRAL DECISION IS THE FOREIGN KEY. Validation must be SELF-CONTAINED (no
-- live group lookup) and it must be FAST -- it runs on every request of a
-- BB_NUMBER_THREADS-parallel sstate HEAD storm. So the hot query is ONE covering
-- index probe with ZERO joins, and the grant (project_id, scope) lives on the row.
--
-- The obvious schema (independent FKs to users and projects) then permits a key
-- belonging to a user who is not a member of that project -- and it keeps working
-- forever after the user is removed, because a self-contained grant can never
-- notice. Instead the key references the MEMBERSHIP as a unit:
--     (user_id, project_id) -> project_memberships (user_id, project_id) CASCADE
-- and project_memberships in turn references org_memberships. Consequences, all
-- enforced by the database and none by application memory:
--   * A key for a non-member cannot be created. Not "is rejected" -- cannot exist.
--   * Removing someone from a project deletes their keys for it.
--   * Login reconciliation dropping an org membership cascades
--     org_memberships -> project_memberships -> api_keys: ONE DELETE revokes every
--     key that user holds anywhere in that org.
-- Revocation therefore happens at RECONCILIATION time, by a cascade -- not at
-- validation time, by a join. Validation stays a single probe and stays correct.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL,
    project_id   uuid NOT NULL,
    name         text NOT NULL CHECK (length(btrim(name)) > 0),
    -- SHA-256 of the FULL presented `bkry_<random>` token. bytea(32), not 64 hex
    -- chars: half the index entry, memcmp comparison, no case/encoding bug that
    -- surfaces only as "my key stopped working". There is no plaintext column --
    -- "shown exactly once" is enforced by the schema's inability to represent the
    -- secret, not by application discipline.
    --
    -- sha256, not bcrypt/argon2, on purpose: the token is 256 bits of CSPRNG
    -- output, so there is no low-entropy secret to stretch, and a KDF on the HEAD
    -- hot path would be a self-inflicted denial of service.
    --
    -- The CHECK is a real defence: a plaintext bkry_ token is ~48 bytes, so a code
    -- path that ever writes the TOKEN instead of its HASH fails LOUDLY at the INSERT
    -- instead of silently storing a live credential.
    token_sha256 bytea NOT NULL CHECK (octet_length(token_sha256) = 32),
    -- Display only, so the console can tell keys apart after the one-time reveal.
    token_prefix text NOT NULL CHECK (token_prefix ~ '^bkry_[A-Za-z0-9_-]{6,12}$'),
    scope        api_key_scope NOT NULL,
    -- NULL = never expires: an absent bound. One meaning, so the nullable is honest.
    expires_at   timestamptz,
    -- NULL = not revoked. The ONLY soft-state column in the schema (see the partial
    -- unique index below for the price it pays).
    revoked_at   timestamptz,
    -- NEVER written on the request path. One CI machine drives a whole build with
    -- ONE key; an inline `UPDATE api_keys SET last_used_at = now()` funnels
    -- thousands of parallel HEADs into a row-lock convoy on the single hottest row
    -- in the database. A coalescing background flusher writes it, at most once per
    -- key per interval, off the request's critical path.
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_membership_fk
        FOREIGN KEY (user_id, project_id)
        REFERENCES project_memberships (user_id, project_id) ON DELETE CASCADE,
    CONSTRAINT api_keys_expires_after_created
        CHECK (expires_at IS NULL OR expires_at > created_at),
    CONSTRAINT api_keys_revoked_after_created
        CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- THE hot index. UNIQUE on the probe column, INCLUDE-ing every column the
-- validation query needs => Index Only Scan, Heap Fetches: 0, no join, on both hit
-- and miss. revoked_at is in the INCLUDE list precisely so the `revoked_at IS NULL`
-- filter does not force a heap fetch.
CREATE UNIQUE INDEX api_keys_token_sha256_key
    ON api_keys (token_sha256)
    INCLUDE (id, user_id, project_id, scope, expires_at, revoked_at);

-- Supports the CASCADE from project_memberships. The PK is `id`, so without this
-- every membership removal seq-scans api_keys.
CREATE INDEX api_keys_user_id_project_id_idx ON api_keys (user_id, project_id);

-- Names unique per (project, user) among LIVE keys only. A plain
-- UNIQUE (project_id, user_id, name) is the reflex, and it would mean revoking the
-- key named "ci" permanently BURNS the name "ci". That is the soft-delete trap in
-- miniature -- contained here, and paid for with exactly this one partial index.
CREATE UNIQUE INDEX api_keys_active_name_key
    ON api_keys (project_id, user_id, name) WHERE revoked_at IS NULL;

CREATE TRIGGER api_keys_touch BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();
