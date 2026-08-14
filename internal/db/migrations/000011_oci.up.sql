-- Bakery M5: the Docker/OCI pull-through proxy. TWO metadata-only changes and no
-- new table.
--
-- The proxy stores everything it serves in cache_objects, across three namespaces
-- the PRIMARY KEY already discriminates (000006 reserved `namespace` for exactly
-- this):
--
--   manifests  key = the manifest's sha256 hex. IMMUTABLE and digest-verified: the
--              key is a hash OF THE BYTES WE STORED, computed by blob.Service, never
--              copied out of an upstream Docker-Content-Digest header.
--   blobs      key = the layer/config sha256 hex. Immutable, digest-verified.
--   tags       key = "<normalized-upstream-host>/<name>:<tag>". The one MUTABLE
--              namespace here, repointed by PutObjectOverwritable exactly like /ac.
--              Its row is a second cache_objects name for the manifest blob, so a
--              tag costs one metadata row and zero extra bytes.
--
-- No expires_at column: tag staleness is DERIVED (now > updated_at + tag_ttl, with
-- the TTL in the backend's config jsonb), so retuning the TTL applies instantly to
-- every existing row and there is no migration against the largest table in the
-- system.

-- content_type is the deferral 000006's closing comment planned for "M4/M5", and M5
-- is the milestone that populates it. It is not decoration:
--
--   containerd trusts the manifest response's Content-Type VERBATIM as
--   ocispec.Descriptor.MediaType (resolver.go) and dispatches on it. Sniffing the
--   `mediaType` field back out of the stored manifest bytes is not a substitute --
--   the field has been optional through most of the OCI image-spec's life, so a
--   perfectly legal index would sniff as nothing and the client would mis-dispatch.
--   The only trustworthy copy is the one the registry sent alongside the bytes, so
--   we store it beside the bytes.
--
-- Nullable and with no default, so this is a catalog-only ALTER: no table rewrite on
-- a table that will hold 10^7 rows. Every namespace except manifests/tags leaves it
-- NULL, and blob.Service serves NULL as "" -- the sstate and /cas read paths set
-- their own Content-Type and never look at this column.
ALTER TABLE cache_objects ADD COLUMN content_type text;

-- RESERVE THE `token` SLUG. The OCI backend mounts the Docker Bearer token endpoint
-- at /v2/token, in the same path space as /v2/{org}/{project}/... -- so an org
-- literally named `token` would sit underneath the token endpoint's namespace, and
-- the two would be told apart only by segment count. That is a routing coincidence,
-- not a design, and it is exactly the class of collision the denylist exists for.
--
-- The Go mirror (internal/slug) carries the same string; internal/db's
-- TestSlugMirrorsDatabase asserts the two agree case by case, so a drift between
-- this function and that slice is a failing test rather than a production surprise.
--
-- CREATE OR REPLACE, not a new function: the CHECK constraints on organizations.slug
-- and projects.slug call it BY NAME, so replacing the body re-points both without
-- touching either table. Existing rows are NOT revalidated (PostgreSQL does not
-- re-check a CHECK on replace, and we do not want it to -- an org that predates this
-- migration must not become unwritable). The denylist is a creation-time gate; that
-- is all it has ever been.
CREATE OR REPLACE FUNCTION bakery_slug_ok(s text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT s ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
       AND s <> ALL (ARRAY[
             'blobs', 'uploads', 'actions', 'actionresults', 'operations',
             'capabilities', 'compressed-blobs', 'ac', 'cas', 'v2', 'api', 'cache',
             'token'
           ]);
$$;
