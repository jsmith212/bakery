-- Reverse of 000011. Dropping content_type discards the stored media types, which is
-- data loss the OCI backend recovers from by re-fetching from upstream -- there is no
-- correctness hazard, only a cold cache.
ALTER TABLE cache_objects DROP COLUMN IF EXISTS content_type;

-- Restore the pre-M5 denylist. `token` becomes a legal slug again; any org created
-- under M5 keeps working either way, because the denylist has only ever been a
-- creation-time gate.
CREATE OR REPLACE FUNCTION bakery_slug_ok(s text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT s ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
       AND s <> ALL (ARRAY[
             'blobs', 'uploads', 'actions', 'actionresults', 'operations',
             'capabilities', 'compressed-blobs', 'ac', 'cas', 'v2', 'api', 'cache'
           ]);
$$;
