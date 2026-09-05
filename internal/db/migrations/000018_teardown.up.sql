-- Backend and project TEARDOWN (the deleting_at soft-delete marks).
--
-- WHAT WAS BROKEN. cache_objects -> cache_backends is ON DELETE RESTRICT (000006),
-- and so are hashserv_unihashes and hashserv_outhashes (000010) -- deliberately, so
-- that dropping a backend can never cascade away metadata without decrementing a
-- single blob refcount. The consequence was that DELETE /backends/{kind} answered
-- 23503 forever on any backend that had ever served a build, and DELETE /projects/
-- {project} inherited the same wall one level up (projects -> cache_backends is
-- RESTRICT too). A project with cached data could not be deleted at all.
--
-- WHY A MARK AND NOT A DRAIN. The alternative is deleting the rows inside the HTTP
-- request. A five-thousand-object backend times out the request; a ten-million-object
-- one holds one transaction open across the whole delete, pinning a snapshot against
-- the hottest table in the schema and blocking autovacuum on it for the duration.
-- Neither is a shape this codebase already has anywhere else, and both fail worst on
-- exactly the installations that most need the feature.
--
-- So the mark is the whole of the API's work, and the deletion is the GC's -- through
-- blob.Service.DeleteBatch, the one sanctioned path (digest-ordered locks, both
-- write-barrier halves re-derived at delete time, shard-grouped LRU invalidation).
-- The row itself goes only once its last object is gone, at which point RESTRICT has
-- nothing left to refuse.
--
-- NULL means LIVE, in every predicate, on every path -- the same posture
-- retention_window's NULL has. A NOT NULL deleting_at is what makes a backend read as
-- UNCONFIGURED to the route resolver (GetBackend, query/backends.sql) and a project
-- read as absent to ResolveRoute (query/identity.sql): both 404 exactly as they do
-- for a row that was never created, which is the sstate rule ("a project/kind with no
-- cache_backends row 404s -- it never mounts a mount point it cannot serve").

-- No DEFAULT and no NOT NULL: every existing row is live, and adding a nullable
-- column with no default is a catalog-only change on PG11+ -- no table rewrite, no
-- lock held while ten million cache_objects are copied. The lock_timeout dance
-- 000012's cache_objects ALTERs needed is not required here for the same reason:
-- these two tables are small (one row per backend, one per project) and neither ALTER
-- rewrites anything.
ALTER TABLE cache_backends ADD COLUMN deleting_at timestamptz;
ALTER TABLE projects       ADD COLUMN deleting_at timestamptz;

COMMENT ON COLUMN cache_backends.deleting_at IS
    'NULL = live. Non-NULL = torn down: unconfigured to every route resolver, swept by the GC teardown stage, row deleted once its last object is gone.';
COMMENT ON COLUMN projects.deleting_at IS
    'NULL = live. Non-NULL = torn down: hidden from every listing, routes 404, row deleted by the GC once its last backend is gone.';

-- PARTIAL indexes, on the predicate rather than the column. The teardown stage runs
-- FIRST in every GC tick and asks "which rows are marked?" -- a question whose answer
-- is empty on every healthy installation, forever. A partial index on
-- `deleting_at IS NOT NULL` is a few pages that stay empty, and it keeps that
-- per-tick probe off a sequential scan of every backend and every project in the
-- installation. A full index on deleting_at would instead index the NULL-valued
-- majority for a query that never asks about them.
CREATE INDEX cache_backends_deleting_idx ON cache_backends (id) WHERE deleting_at IS NOT NULL;
CREATE INDEX projects_deleting_idx       ON projects       (id) WHERE deleting_at IS NOT NULL;
