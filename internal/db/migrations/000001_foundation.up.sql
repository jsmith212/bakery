-- Bakery M1: foundation. Enums, shared helper functions, no extensions.
-- gen_random_uuid() is built into PostgreSQL 13+; do NOT install uuid-ossp/pgcrypto.
-- Minimum server: PostgreSQL 14.

CREATE TYPE backend_kind  AS ENUM ('sstate', 'downloads', 'hashserv', 'bazel', 'oci');
CREATE TYPE site_role     AS ENUM ('user', 'admin');
CREATE TYPE org_role      AS ENUM ('member', 'admin', 'owner');
CREATE TYPE project_role  AS ENUM ('reader', 'writer', 'admin');
CREATE TYPE api_key_scope AS ENUM ('read', 'write');
CREATE TYPE blob_state    AS ENUM ('live', 'pending_delete');
CREATE TYPE gc_run_status AS ENUM ('running', 'succeeded', 'failed');

-- Slug grammar + the reserved-slug denylist, in ONE place, shared by organizations
-- and projects. IMMUTABLE so it is legal inside a CHECK.
--
-- The reserved list is a ROUTING fact (the URL grammar and the ByteStream
-- resource-name scanner), not tunable policy: it must hold for every writer --
-- the API, the dev seeder, a migration, a psql session. Not a lookup table (that
-- would invite "just delete the row to unblock a customer"), not a DOMAIN (sqlc's
-- domain handling is unreliable).
--
-- NOTE: the spec spells one entry `actionResults` (camelCase). The grammar below
-- forbids uppercase, so that exact string is already unrepresentable; the string a
-- user COULD create is `actionresults`, so that is what we reserve.
CREATE FUNCTION bakery_slug_ok(s text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT s ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
       AND s <> ALL (ARRAY[
             'blobs', 'uploads', 'actions', 'actionresults', 'operations',
             'capabilities', 'compressed-blobs', 'ac', 'cas', 'v2', 'api', 'cache'
           ]);
$$;

-- Advisory-lock key for a blob digest: first 8 bytes of the sha256 as a signed
-- bigint. Used by blob.Service and the GC to serialise byte I/O per digest.
--
-- This is the SINGLE-bigint advisory key space. The boot lock
-- (pg_try_advisory_lock, --allow-multi-instance) MUST use the TWO-int4 key space,
-- which PostgreSQL keeps strictly separate, so a digest can never collide with the
-- process-lifetime boot lock and wedge a PUT.
CREATE FUNCTION bakery_blob_lock_key(digest bytea) RETURNS bigint
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT ('x' || encode(substring(digest FROM 1 FOR 8), 'hex'))::bit(64)::bigint;
$$;

-- updated_at maintenance for the CONTROL-PLANE tables only. Deliberately NOT
-- applied to blobs / cache_objects: those are the hot path and set timestamps
-- explicitly, so we never pay a plpgsql invocation on a refcount update.
CREATE FUNCTION bakery_touch_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
