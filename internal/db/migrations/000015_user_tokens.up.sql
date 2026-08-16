-- Feedback wave 1, item 3: personal access tokens (`bkru_`).
--
-- A user token is the credential a human hands a headless machine when they want
-- it to act AS THEM: `bakery sstate push` from a laptop with no browser, a
-- one-off build box, a script. It is NOT an api_keys row and must never become
-- one, because the two answer different questions:
--
--   api_keys   "this ONE project, at this ONE scope" -- a SNAPSHOT grant, capped
--              at mint time, revoked by an FK cascade.
--   user_tokens "whatever this human can reach, right now, capped at read|write"
--              -- a LIVE grant, resolved against the live role tables on every
--              request.
--
-- Live is the whole point and it is also the whole risk, so read the epoch half
-- of this migration as part of the same design: without it, "live" would mean
-- "live-ish, within a cache TTL, if somebody remembered to call Evict".

CREATE TABLE user_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The ONLY foreign key, and note what it is NOT: there is no project_id and no
    -- membership FK. api_keys hangs off project_memberships so that losing a
    -- membership CASCADE-deletes the key; a user token deliberately cannot do that,
    -- because it is not scoped to a membership -- it is scoped to the human. The
    -- equivalent guarantee is supplied by resolving roles LIVE on every request
    -- (see users.authz_epoch below), not by a cascade.
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (length(btrim(name)) > 0),
    -- SHA-256 of the FULL presented `bkru_<random>` token. Same shape, same
    -- reasoning and same CHECK as api_keys.token_sha256: there is no plaintext
    -- column, so "shown exactly once" is a property of the schema rather than a
    -- discipline of the application, and a code path that ever wrote the TOKEN
    -- instead of its HASH fails loudly at the INSERT.
    token_sha256 bytea NOT NULL CHECK (octet_length(token_sha256) = 32),
    token_prefix text NOT NULL CHECK (token_prefix ~ '^bkru_[A-Za-z0-9_-]{6,12}$'),
    -- A COARSE CEILING on what the live role set may be used for, not a grant.
    --
    -- There is nothing to cap this token TO except its owner's own authority, so
    -- unlike api_keys.scope it names no project. `read` means the token can never
    -- write, however senior its holder becomes; `write` means writes are limited
    -- by the holder's live project roles and by nothing else.
    --
    -- ADMIN CAPABILITY IS NOT REPRESENTABLE HERE, on purpose. api_key_scope has two
    -- values and neither of them is `admin`, so there is no value a future caller
    -- could set that would make this credential able to administer anything.
    max_scope    api_key_scope NOT NULL DEFAULT 'read',
    -- NULL = never expires. The console defaults to 90 days; the schema permits
    -- never, because a build host that must survive an unattended year is a real
    -- deployment and forcing a fake expiry just moves the problem into a cron job.
    expires_at   timestamptz,
    -- NULL = not revoked. Soft, like api_keys: the console must be able to say
    -- "revoked 3 days ago", and a revoked token's hash must stay reserved.
    revoked_at   timestamptz,
    -- NEVER written on the request path -- a coalescing background flusher owns it
    -- (internal/auth's toucher). One CI machine drives a whole build with ONE
    -- credential, so an inline UPDATE funnels a BB_NUMBER_THREADS-parallel HEAD
    -- storm into a row-lock convoy on the single hottest row in the database.
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_tokens_expires_after_created
        CHECK (expires_at IS NULL OR expires_at > created_at),
    CONSTRAINT user_tokens_revoked_after_created
        CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- THE hot index, a deliberate clone of api_keys_token_sha256_key: UNIQUE on the
-- probe column, INCLUDE-ing every column the validation query reads, so the probe
-- is an Index Only Scan with Heap Fetches: 0 on a hit AND on a miss.
--
-- user_id is in the INCLUDE list because the validator JOINs users on the primary
-- key in the SAME statement to read authz_epoch -- one round trip, index scan plus
-- PK lookup. That join is what makes epoch-keyed caching possible without a second
-- probe on the sstate HEAD storm.
CREATE UNIQUE INDEX user_tokens_token_sha256_key
    ON user_tokens (token_sha256)
    INCLUDE (id, user_id, max_scope, expires_at, revoked_at);

-- Names unique per user among LIVE tokens only -- the same partial index api_keys
-- pays for, and for the same reason: a plain UNIQUE would mean revoking the token
-- named "laptop" permanently burns the name "laptop".
CREATE UNIQUE INDEX user_tokens_active_name_key
    ON user_tokens (user_id, name) WHERE revoked_at IS NULL;

-- Supports the CASCADE from users and the console's own listing.
CREATE INDEX user_tokens_user_id_idx ON user_tokens (user_id);

CREATE TRIGGER user_tokens_touch BEFORE UPDATE ON user_tokens
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

-- ---------------------------------------------------------------------------
-- THE AUTHORIZATION EPOCH
-- ---------------------------------------------------------------------------
--
-- A user token resolves the holder's LIVE roles. Reading three tables on every
-- sstate HEAD is not affordable, so the role set is cached in process -- and a
-- cache of AUTHORIZATION is a very different object from a cache of route
-- metadata. The usual answer, "call Evict() from every mutation site", is a
-- whole-program claim maintained by hand: one missed call and a demoted user
-- keeps writing for the length of a TTL, silently, with no failing test.
--
-- So the cache is keyed on (user_id, authz_epoch) and this column is bumped BY
-- THE DATABASE whenever anything that feeds a principal changes. A stale entry is
-- not evicted; it becomes UNREACHABLE, because the next probe reads a different
-- epoch and therefore a different key. There is no call site to forget, and it is
-- sound even under --allow-multi-instance, where an in-process Evict is not.
--
-- The probe reads it in the same statement as the token row (a PK join), so the
-- guarantee costs zero extra round trips.
ALTER TABLE users ADD COLUMN authz_epoch bigint NOT NULL DEFAULT 0;

-- Bumps the epoch of the user a MEMBERSHIP row belongs to.
--
-- AFTER, and FOR EACH ROW: the reconciler's DELETE can touch many memberships in
-- one statement and each one is a different user. RETURN NULL because an AFTER
-- trigger's return value is ignored.
CREATE FUNCTION bakery_bump_authz_epoch() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE users
       SET authz_epoch = authz_epoch + 1
     WHERE id = CASE WHEN TG_OP = 'DELETE' THEN OLD.user_id ELSE NEW.user_id END;

    -- A membership that MOVES between users is not reachable through any query we
    -- ship (the primary key makes it a delete-plus-insert), but if one ever is, the
    -- user who LOST the row must be invalidated too. Cheap, and the failure it
    -- prevents is "the old holder keeps the role in cache".
    IF TG_OP = 'UPDATE' AND OLD.user_id IS DISTINCT FROM NEW.user_id THEN
        UPDATE users SET authz_epoch = authz_epoch + 1 WHERE id = OLD.user_id;
    END IF;

    RETURN NULL;
END;
$$;

-- Org membership: INSERT (granted), UPDATE (oidc_role/local_role changed, which is
-- how EVERY role change on this plane happens -- `role` is generated), DELETE
-- (revoked). All three change what a principal may do.
--
-- IT IS TWO TRIGGERS BECAUSE A `WHEN` CLAUSE CANNOT BE SHARED WITH INSERT/DELETE
-- (there is no OLD on an insert, no NEW on a delete), and the UPDATE half needs
-- one for the same reason users_site_role_bumps_authz_epoch below does: Postgres
-- fires row triggers on an UPDATE regardless of value equality, so an ordinary
-- login -- which re-upserts the OIDC half of every mapped org on EVERY request
-- that reconciles -- would evict that user's whole principal-cache generation
-- once per membership, for no change at all. The upsert itself carries the same
-- guard (ReconcileOrgMembershipUpsert's `DO UPDATE ... WHERE`); this is the half
-- that holds for writers that do not.
CREATE TRIGGER org_memberships_bump_authz_epoch
    AFTER INSERT OR DELETE ON org_memberships
    FOR EACH ROW EXECUTE FUNCTION bakery_bump_authz_epoch();

CREATE TRIGGER org_memberships_update_bumps_authz_epoch
    AFTER UPDATE ON org_memberships
    FOR EACH ROW
    WHEN (OLD.oidc_role IS DISTINCT FROM NEW.oidc_role
       OR OLD.local_role IS DISTINCT FROM NEW.local_role
       OR OLD.user_id IS DISTINCT FROM NEW.user_id)
    EXECUTE FUNCTION bakery_bump_authz_epoch();

-- Project membership: same three, same reason, same split. This one also covers
-- the cascade from org_memberships, because the cascade issues real DELETEs on
-- this table.
CREATE TRIGGER project_memberships_bump_authz_epoch
    AFTER INSERT OR DELETE ON project_memberships
    FOR EACH ROW EXECUTE FUNCTION bakery_bump_authz_epoch();

CREATE TRIGGER project_memberships_update_bumps_authz_epoch
    AFTER UPDATE ON project_memberships
    FOR EACH ROW
    WHEN (OLD.role IS DISTINCT FROM NEW.role
       OR OLD.user_id IS DISTINCT FROM NEW.user_id)
    EXECUTE FUNCTION bakery_bump_authz_epoch();

-- The site role is on `users` itself, so bumping it needs a BEFORE trigger that
-- writes NEW rather than an AFTER trigger that issues another UPDATE -- the latter
-- would recurse.
CREATE FUNCTION bakery_bump_own_authz_epoch() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.authz_epoch := OLD.authz_epoch + 1;

    RETURN NEW;
END;
$$;

-- Fires ONLY when a site-role SOURCE column actually changes.
--
-- The WHEN clause is load-bearing twice over. It stops the membership triggers'
-- own `UPDATE users SET authz_epoch = ...` from bumping a second time, and -- more
-- importantly -- it stops an ordinary login (which rewrites email, display_name,
-- avatar_url and last_login_at on every single request that reconciles) from
-- invalidating the cache for no reason.
--
-- It names site_role_oidc and site_role_local, the two SOURCES. `site_role` itself
-- is GENERATED from them and cannot be written, so watching it directly would be
-- watching a column no statement ever assigns.
CREATE TRIGGER users_site_role_bumps_authz_epoch
    BEFORE UPDATE ON users
    FOR EACH ROW
    WHEN (OLD.site_role_oidc IS DISTINCT FROM NEW.site_role_oidc
       OR OLD.site_role_local IS DISTINCT FROM NEW.site_role_local)
    EXECUTE FUNCTION bakery_bump_own_authz_epoch();
