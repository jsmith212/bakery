-- Feedback wave 1, item 4: robots (`bkro_`) -- org-owned machine identities.
--
-- THE CENTRAL DECISION IS THAT THESE ARE SEPARATE TABLES, NOT `users` ROWS.
--
-- Three grounds, and the first two are structural rather than stylistic:
--
--  1. `users` cannot honestly hold a robot. `email` is NOT NULL with a non-blank
--     CHECK and there is a UNIQUE INDEX on lower(email); a robot row would need a
--     fabricated, globally unique address. The table's own comment is "One human,
--     one row", and fabricating identity data to satisfy an identity table is the
--     smell, not the workaround.
--
--  2. RECONCILER SAFETY IS THEN STRUCTURAL. The login path's statements --
--     UpsertUser, ReconcileOrgMembershipsDelete/ClearOIDC/Upsert -- do not name
--     `robots` or `org_tokens` anywhere. A login cannot touch a robot because
--     there is no SQL on the login path that can ADDRESS one. The alternative
--     ("nothing ever calls Reconcile with a robot's user_id") is a whole-program
--     claim a refactor can break silently. TestReconcileNeverNamesRobotTables
--     asserts the property this schema makes true.
--
--  3. "NO TOUCHY ON ANYTHING ELSE" IS ENFORCED BY ABSENCE. Give a robot an
--     org_memberships row and it becomes reachable from CanReadProject's
--     `p.orgs[orgID]` branch -- one refactor away from failing open into full org
--     membership. With no membership row, that branch is unreachable for a robot
--     no matter what code anyone writes later.

CREATE TABLE robots (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    description text NOT NULL DEFAULT '',
    -- AUDIT, not authorization. A HUMAN created this machine identity, so the FK
    -- points at users -- but ON DELETE SET NULL, because the robot deliberately
    -- OUTLIVES its creator (see org_tokens.expires_at below for the countervailing
    -- control).
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The audit trail that survives the human. created_by goes NULL the day its
    -- referent is deleted; without this snapshot the ROBOTS card -- which exists
    -- precisely to be the audit surface for a write-everywhere credential -- would
    -- show a blank for the one row an auditor most wants attributed.
    created_by_email text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT robots_org_name_key UNIQUE (org_id, name),
    -- Redundant against the primary key, and it is here for exactly one reason:
    -- it is the target org_tokens' COMPOSITE foreign key hangs off (see below).
    -- Without it `FOREIGN KEY (robot_id, org_id) REFERENCES robots (id, org_id)`
    -- cannot be declared at all, and org_tokens.org_id -- which IS the entire
    -- authorization decision for a robot -- would be free to disagree with the
    -- robot's actual org.
    CONSTRAINT robots_id_org_key UNIQUE (id, org_id)
);

CREATE INDEX robots_org_id_idx ON robots (org_id);

CREATE TRIGGER robots_touch BEFORE UPDATE ON robots
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();

CREATE TABLE org_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    robot_id     uuid NOT NULL,
    -- DENORMALIZED ON PURPOSE. It is what the hot probe returns, so the
    -- authorization decision ("is the routed org this token's org?") is answered
    -- from the index tuple alone, with no lookup through `robots`. The FK to
    -- organizations keeps it honest, and the cascade means deleting an org kills
    -- its robots' tokens by either path.
    --
    -- Denormalized is not the same as unconstrained. This column IS the
    -- authorization decision -- principal.robotGrants compares it to the routed
    -- org and reads nothing else -- so the composite FK below (not this one) is
    -- what makes "this token's org is its robot's org" a property of the SCHEMA
    -- rather than of one INSERT's shape. Two independent FKs would let any future
    -- writer that supplies org_id as a parameter mint a live token granting
    -- org-wide write on org B backed by a robot in org A, with nothing dangling
    -- and no test to notice. Same leg api_keys' composite FK onto
    -- project_memberships provides, and for the same reason.
    org_id       uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (length(btrim(name)) > 0),
    token_sha256 bytea NOT NULL CHECK (octet_length(token_sha256) = 32),
    token_prefix text NOT NULL CHECK (token_prefix ~ '^bkro_[A-Za-z0-9_-]{6,12}$'),
    scope        api_key_scope NOT NULL,
    -- NOT NULL, unlike every other expiry in this schema, AND THAT IS THE RULING.
    --
    -- A robot is an ORG-OWNED identity, not a delegation: it survives its creator
    -- on purpose, so that a CI pipeline does not break the day the engineer who
    -- set it up leaves. But the two revocation legs a delegation gets for free --
    -- the mint-time cap and the membership cascade -- do not exist here. Deleting
    -- the robot or the org is the only structural leg left, and both require a
    -- deliberate human act that nobody will remember to perform.
    --
    -- Expiry is the countervailing control, and a machine identity is exactly the
    -- case where it is cheap: the rotation is a scripted `bakery org robot create`
    -- against a token whose whole job is to sit in a CI secret store. The API and
    -- console cap it at 365 days.
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    -- Same audit pair as robots above: WHO MINTED THIS TOKEN, which need not be
    -- the human who created the robot.
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_email text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT org_tokens_expires_after_created
        CHECK (expires_at > created_at),
    -- Parity with user_tokens and api_keys. Its absence in the design memo was an
    -- oversight, not a decision: a revoked_at before created_at is nonsense on
    -- every one of these tables, and only two of the three said so.
    CONSTRAINT org_tokens_revoked_after_created
        CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    -- THE CROSS-TENANT GATE. It replaces a plain `robot_id REFERENCES robots (id)`
    -- and it carries the same ON DELETE CASCADE, so deleting a robot still kills
    -- every token it holds -- but a row whose org_id disagrees with its robot's
    -- org can no longer be REPRESENTED, by any writer, ever.
    CONSTRAINT org_tokens_robot_org_fkey
        FOREIGN KEY (robot_id, org_id) REFERENCES robots (id, org_id) ON DELETE CASCADE
);

-- The hot index, third and last clone of api_keys_token_sha256_key. robot_id,
-- org_id and scope are all INCLUDE-d, so a robot's authorization is decided from
-- ONE Index Only Scan with zero joins and zero principal load -- cheaper than a
-- user token, identical to an api_keys probe. The `bkro_` prefix routes straight
-- here, so a miss never costs a second probe against another table.
CREATE UNIQUE INDEX org_tokens_token_sha256_key
    ON org_tokens (token_sha256)
    INCLUDE (id, robot_id, org_id, scope, expires_at, revoked_at);

CREATE INDEX org_tokens_robot_id_idx ON org_tokens (robot_id);
CREATE INDEX org_tokens_org_id_idx ON org_tokens (org_id);

CREATE UNIQUE INDEX org_tokens_active_name_key
    ON org_tokens (robot_id, name) WHERE revoked_at IS NULL;

CREATE TRIGGER org_tokens_touch BEFORE UPDATE ON org_tokens
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();
