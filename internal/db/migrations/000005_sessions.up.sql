-- Bakery M1: browser sessions for alexedwards/scs.
--
-- scs's canonical schema, VERBATIM. Do not "improve" it. `data` is an opaque gob
-- that scs owns; scs.Store only ever sees (token, data, expiry), so a user_id column
-- here could only be populated by an out-of-band UPDATE -- a nullable column that
-- nothing reliably fills is a lie in table form.
--
-- The consequence -- no "log out every session for user X" as a DB operation -- is
-- cheap, because the session payload carries only the user id and EVERY authorization
-- fact (site_role, org memberships, project roles) is read live from the tables above
-- on each console request. A demotion or an org removal takes effect on the user's
-- very next request with no session-invalidation machinery at all. That is the
-- asymmetry with API keys, which must be self-contained for speed and therefore need
-- the cascade in 000004 instead.
--
-- scs's stock postgresstore is database/sql and is FORBIDDEN. This table exists so a
-- hand-written pgxpool adapter can implement scs.Store over these three columns.
--
-- Sessions are NEVER on the cache hot path. Cache traffic authenticates with api_keys;
-- only the human console touches this table.
CREATE TABLE sessions (
    token  text PRIMARY KEY,
    data   bytea NOT NULL,
    expiry timestamptz NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);
