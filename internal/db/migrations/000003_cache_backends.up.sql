-- Bakery M1: cache backend registry. M1 ships NO backend implementation; this is
-- the routing/metadata anchor that blob.Service and every M2..M5 backend hang off.

CREATE TABLE cache_backends (
    -- BIGINT, not uuid, and it is deliberate: this id is the LEADING COLUMN of the
    -- cache_objects primary key -- the one index in this system that will hold
    -- 10^7+ entries -- so 8 bytes vs 16 is paid per object, not per backend.
    -- Backends are never addressed by id in a URL (they are addressed by
    -- {org}/{project}/{kind}), so a sequential id is not externally enumerable and
    -- leaks nothing.
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- RESTRICT: dropping a project that still has backends (and therefore objects,
    -- and therefore refcounts) must be refused. See projects.org_id.
    project_id         uuid NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    kind               backend_kind NOT NULL,
    enabled            boolean NOT NULL DEFAULT true,
    -- Reads may be opened up per backend. WRITES ALWAYS REQUIRE A KEY: there is
    -- deliberately no write_auth_required column, so "unauthenticated writes" -- a
    -- cache-poisoning vector -- is not a state this database can represent.
    -- The name says "for reads" so the asymmetry cannot be misread.
    read_auth_required boolean NOT NULL DEFAULT true,
    -- The one schemaless column in the schema. M1 has no backend serving traffic,
    -- so the per-kind config shapes are not designed yet; inventing five per-kind
    -- tables would be speculative stubbing. Read ONLY on the cold path (route-cache
    -- fill), never on a cache request. Promote fields to real columns in M2..M5.
    config             jsonb NOT NULL DEFAULT '{}'::jsonb
                       CHECK (jsonb_typeof(config) = 'object'),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- Exactly one backend of each kind per project. This is not a nicety, it is the
    -- routing grammar: /cache/{org}/{project}/sstate/... names ONE mount. It is also
    -- what makes the sstate<->hashserv coupling 1:1 by construction -- without it,
    -- "which hashserv roots which sstate?" has no answer and the M3 GC is
    -- structurally impossible to write correctly.
    CONSTRAINT cache_backends_project_id_kind_key UNIQUE (project_id, kind)
);
CREATE TRIGGER cache_backends_touch BEFORE UPDATE ON cache_backends
    FOR EACH ROW EXECUTE FUNCTION bakery_touch_updated_at();
