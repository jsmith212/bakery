-- Bakery M1: THE CORE.
--
--   blobs         content-addressed bytes, deduped across ALL projects, refcounted.
--                 blob.Service is the only writer.
--   cache_objects the per-project NAMED keys that reference a blob. This is the
--                 table the sstate HEAD storm hits.
--
-- THE ORDERING INVARIANT, EXPRESSED IN DDL. `state = 'live'` MEANS "the bytes are
-- durably in storage.Store". There is deliberately NO state meaning "servable, but
-- the bytes may be missing". Every transition INTO 'live' happens strictly after the
-- storage write returns; the only transition OUT of it is into 'pending_delete',
-- which is not servable. Dangling metadata is not a bug we avoid by convention -- it
-- is a row this schema cannot hold.
--
-- WHY 'pending_delete' EXISTS AT ALL (the hole the obvious reading leaves open).
-- Taken literally, "on delete: metadata first, then bytes" is NOT sufficient for a
-- deduped, refcounted store:
--     GC:     DELETE FROM blobs WHERE digest = D;  COMMIT
--     PUT(D): sees no row -> writes the (bit-identical) bytes -> INSERTs a live row
--     GC:     storage.Delete(D)   <-- destroys the bytes the new row names
--   => live metadata, no bytes. A permanent 500, reached by obeying the invariant.
-- Content-addressing does not save you (the bytes are identical). ON CONFLICT does
-- not save you (the GC's DELETE removed the row there was to conflict with). And
-- FOR UPDATE cannot lock a row that does not exist yet.
--
-- So: the TOMBSTONE MUST OUTLIVE THE BYTES. mark -> unlink -> reap, with the unlink
-- INSIDE the reaping transaction, and BOTH the writer and the GC serialised on a
-- lock keyed by the DIGEST (bakery_blob_lock_key), not by the row. A crash between
-- the unlink and the reap leaves a durable 'pending_delete' row, which boot and every
-- GC run re-drive -- and which a concurrent PUT can see and refuse to trust, forcing
-- a re-upload instead of a dedup fast-path onto bytes that are already gone.
--
-- FULL PROTOCOL (blob.Service, one transaction, READ COMMITTED):
--   PUT: pg_advisory_xact_lock(bakery_blob_lock_key(d))
--        SELECT ... FOR UPDATE -> absent or state='pending_delete'?
--          yes -> storage.Put(bytes)            [BYTES FIRST]
--        INSERT INTO blobs ... ON CONFLICT DO UPDATE (revive)
--        INSERT INTO cache_objects ...          [THEN METADATA; trigger bumps refcount]
--   DEL: DELETE FROM cache_objects ...          [METADATA FIRST; trigger decrements]
--        bytes are never touched here. Only the GC deletes bytes.
--   GC:  mark (refcount=0 + grace + write barrier, FOR UPDATE SKIP LOCKED) -> COMMIT
--        then per digest: advisory lock; recheck refcount=0 AND state='pending_delete'
--        FOR UPDATE (0 rows => revived => abort); storage.Delete; DELETE; COMMIT.
-- READ COMMITTED is REQUIRED: under REPEATABLE READ the snapshot is taken before the
-- advisory lock is granted and the post-lock SELECT reads a stale world.

CREATE TABLE blobs (
    -- The digest IS the identity. No surrogate uuid: cache_objects has to carry the
    -- digest anyway for the join-free hot path, so a uuid PK would cost 16 bytes per
    -- OBJECT and buy nothing but a second identity that can drift from the first.
    --
    -- 32 raw bytes, not 64 hex chars: half the index entry, memcmp comparison, and Go
    -- hands us a [32]byte already.
    --
    -- The algorithm is not a column. There is exactly one, and it is OURS: this is the
    -- sha256 WE computed over the bytes, independent of what the client called its key.
    -- That is precisely what lets one blob table serve /cas/ (where key == sha256(body)
    -- is verified) and /ac/ (an OPAQUE byte store where it emphatically is not).
    -- Verification is a per-call policy flag in blob.Service; the schema takes no
    -- position. If sha512 ever arrives: add hash_algo, widen the CHECK, re-key.
    digest             bytea PRIMARY KEY CHECK (octet_length(digest) = 32),
    -- >= 0, not > 0. The REAPI empty blob (e3b0c442..., size 0) MUST always report as
    -- present; CHECK (size_bytes > 0) is a classic way to break every Bazel client at
    -- once.
    size_bytes         bigint NOT NULL CHECK (size_bytes >= 0),
    -- Maintained ONLY by the cache_objects trigger below. Nothing else may write it.
    refcount           bigint NOT NULL DEFAULT 0 CHECK (refcount >= 0),
    state              blob_state NOT NULL DEFAULT 'live',
    -- Set exactly when refcount reaches 0, cleared when it leaves 0. The grace period
    -- is measured from here.
    unreferenced_since timestamptz DEFAULT now(),
    delete_started_at  timestamptz,
    -- THE WRITE BARRIER, in two forms; the sweep predicate ANDs both.
    --
    -- created_at is the documented barrier (CLAUDE.md: `created_at < gc_run.started_at`).
    -- It is indexable and cheap, and it is the prefilter.
    --
    -- live_xid is the barrier that ACTUALLY HOLDS, and the timestamp form alone does
    -- not. now() is TRANSACTION-START time: a build that BEGINs before a GC run starts
    -- and COMMITs after it produces a row whose created_at PREDATES gc_runs.started_at
    -- while being invisible to the GC's snapshot -- the timestamp barrier says "sweep
    -- it" and eats a freshly-minted blob of a running build. That is exactly the
    -- failure the invariant exists to prevent, and clock_timestamp() only narrows the
    -- window, it does not close it.
    --   pg_visible_in_snapshot(live_xid::text::xid8, gc_runs.snapshot::pg_snapshot)
    -- closes it exactly: a row is sweepable only if the transaction that made it live
    -- was committed AND visible when the GC took its snapshot.
    --
    -- Stored as bigint / text rather than xid8 / pg_snapshot because pgx and sqlc have
    -- no dependable codec for those types, and breaking codegen to win a type-purity
    -- point is a bad trade. The ::text:: round-trip is exact for every value a real
    -- cluster will ever hold.
    --
    -- RESET (not preserved) when a 'pending_delete' blob is revived by a PUT: a revived
    -- blob is NEW to the GC. Resetting can only ever spare more rows, never fewer.
    created_at         timestamptz NOT NULL DEFAULT now(),
    live_xid           bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    -- Backs the composite FK from cache_objects, which is what makes the denormalised
    -- (digest, size_bytes) on the hot table structurally incapable of drifting.
    CONSTRAINT blobs_digest_size_key UNIQUE (digest, size_bytes),

    -- A refcount that hits zero without stamping unreferenced_since is invisible to GC
    -- forever: the bytes leak silently and nothing reports it. Conversely, a
    -- resurrection that increments refcount but forgets to CLEAR unreferenced_since
    -- leaves a LIVE blob on the GC candidate list -- and GC then deletes the bytes out
    -- from under a live object. This CHECK makes both a constraint violation.
    CONSTRAINT blobs_unreferenced_consistent
        CHECK ((refcount = 0) = (unreferenced_since IS NOT NULL)),

    -- ANTI-RESURRECTION, structural. A blob queued for physical deletion while
    -- something still references it is not a row this database will hold. A future
    -- refactor that "helpfully" bumps a refcount without flipping the state back to
    -- 'live' gets an aborted transaction instead of a 200 OK followed by an unlink of
    -- the bytes it just handed out. Silent data loss becomes a loud error.
    CONSTRAINT blobs_pending_delete_is_unreferenced
        CHECK (state <> 'pending_delete' OR refcount = 0),

    CONSTRAINT blobs_delete_started_only_when_pending
        CHECK (state = 'pending_delete' OR delete_started_at IS NULL)
)
-- Measured: refcount is in no index and in no index PREDICATE, which is what lets
-- refcount++ be a HOT update -- but only while the page has free space. fillfactor 70
-- is what keeps it that way under a sustained PUT burst.
WITH (fillfactor = 70);

-- The GC candidate scan, and nothing else. PARTIAL, so it holds ONLY unreferenced live
-- blobs -- a rounding error next to the table -- and stays resident even when `blobs`
-- does not.
--
-- THE PREDICATE IS DELIBERATELY `unreferenced_since IS NOT NULL`, NOT `refcount = 0`.
-- The two select exactly the same rows (blobs_unreferenced_consistent makes them
-- equivalent), but PostgreSQL treats any column appearing in an index PREDICATE as
-- HOT-blocking. With `refcount = 0` in the predicate, EVERY refcount increment rewrites
-- an index entry and no update on this table is ever HOT. MEASURED: 0.0% HOT with
-- `refcount = 0` in the predicate; 99.8% HOT with `unreferenced_since IS NOT NULL`.
-- The dedup path (refcount 1 -> N, unreferenced_since already NULL, state unchanged) is
-- the common case and it must not touch an index.
CREATE INDEX blobs_gc_candidates_idx ON blobs (unreferenced_since)
    WHERE state = 'live' AND unreferenced_since IS NOT NULL;

-- Crash recovery. A process that dies between the mark and the unlink leaves a durable
-- 'pending_delete' row; boot and every GC run re-drive these. This is why
-- 'pending_delete' is a persisted state and not an in-memory work queue.
CREATE INDEX blobs_pending_delete_idx ON blobs (delete_started_at)
    WHERE state = 'pending_delete';

-- NO storage_path column: the path is a pure function of the digest (aa/bb/aabb...),
-- and storing it makes a second copy of a fact that can then disagree with the first.
-- NO storage_backend column: storage is LOCAL ONLY in M1 (S3 explicitly deferred).
-- Adding it when S3 lands is a metadata-only ALTER with no table rewrite.


CREATE TABLE cache_objects (
    backend_id  bigint NOT NULL REFERENCES cache_backends (id) ON DELETE RESTRICT,

    -- WHY THIS COLUMN EXISTS, AND WHY OMITTING IT IS A CONTENT-INTEGRITY BUG: a bazel
    -- backend serves TWO key spaces, /ac/ and /cas/, and BOTH are 64 hex characters.
    -- Without a discriminator they collide on (backend_id, key) -- and /ac/ is
    -- overwritable and UNVERIFIED while /cas/ is immutable and digest-VERIFIED, so a
    -- ccache write to /ac/<h> would silently repoint the CAS blob at <h> at unrelated
    -- content. OCI (M5) needs the same split (blobs / manifests / tags). '' for sstate
    -- and downloads.
    --
    -- It is part of the PRIMARY KEY, which is why it lands in M1 even though the first
    -- backend that needs it is M4: adding a column to the PK of the largest table in
    -- the system later is a rebuild, not a migration. (Contrast gc_root/content_type,
    -- which are ordinary nullable columns and are therefore deliberately deferred to
    -- the milestone that actually populates them.)
    namespace   text NOT NULL DEFAULT '' CHECK (namespace ~ '^[a-z-]{0,32}$'),

    -- The CLIENT's opaque key. NEVER assumed to equal the digest.
    --
    -- The 1024-byte cap is not arbitrary: a btree entry may not exceed ~2704 bytes, so
    -- without it a sufficiently long key is not a clean 400 -- it is a RUNTIME INDEX
    -- FAILURE on INSERT, discovered in production. Observed sstate keys are ~120-160
    -- bytes, so this is ~7x headroom.
    key         text NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 1024),

    -- Denormalised from blobs so that HEAD -- THE hot path -- is a SINGLE PRIMARY-KEY
    -- PROBE WITH NO JOIN. Both are immutable functions of the content, and the composite
    -- FK below means the database will not even let them try to drift.
    digest      bytea  NOT NULL,
    size_bytes  bigint NOT NULL CHECK (size_bytes >= 0),

    -- Write barrier, same two forms as blobs. Reset by an /ac/ overwrite, which makes
    -- the object new to the GC: conservative, therefore safe.
    created_at  timestamptz NOT NULL DEFAULT now(),
    live_xid    bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- THE PRIMARY KEY IS THE URL. There is no surrogate id: a uuid here would add 16
    -- bytes AND a second btree to a 10^7-row table in exchange for nothing -- nothing
    -- references an object by id. The hot path resolves {org}/{project}/{kind} to a
    -- backend_id from the in-process route cache, then does ONE btree probe on this key.
    PRIMARY KEY (backend_id, namespace, key),

    -- RESTRICT is the single most valuable constraint in this schema, and it is
    -- load-bearing twice over:
    --  (1) The invariant says dangling metadata is a permanent 500 while orphaned bytes
    --      are merely wasteful. This FK makes dangling metadata IMPOSSIBLE, not merely
    --      unlikely: the database refuses to delete a blob row that any object still
    --      names, no matter what the refcount says and no matter which code path asks.
    --      The refcount is the fast path; the FK is the TRUTH. Refcount drift becomes a
    --      loud foreign-key violation, not a silent corruption.
    --  (2) It is why orgs/projects/backends are RESTRICT-protected rather than
    --      soft-deleted: a CASCADE from project down to cache_objects would delete
    --      millions of rows and (with the trigger) fire millions of blob updates in one
    --      transaction -- or, without one, leak every blob permanently. The DB refuses
    --      the shortcut and forces an explicit chunked purge, which the trigger then
    --      makes arithmetically correct for free.
    CONSTRAINT cache_objects_blob_fk FOREIGN KEY (digest, size_bytes)
        REFERENCES blobs (digest, size_bytes) ON DELETE RESTRICT
)
WITH (fillfactor = 95);  -- insert-mostly; only /ac/ overwrites in place.

-- MANDATORY. PostgreSQL does NOT index the referencing side of a foreign key. Without
-- this, every blob deletion in the GC sequentially scans the largest table in the
-- database to prove the RESTRICT holds. This one index is the difference between a GC
-- that finishes and a GC that never does.
CREATE INDEX cache_objects_digest_idx ON cache_objects (digest);

-- Deliberately ABSENT: last_accessed_at (updating it on read turns the HEAD storm --
-- thousands of parallel READS -- into thousands of parallel WRITES on the hottest table
-- we own); content_type (no backend exists to populate it until M4/M5); created_by
-- (16 bytes plus an FK on the largest table for a query nobody runs -- attribution lives
-- in the metrics and the access log); gc_root (the M3 unihash root -- M1 ships ZERO cache
-- backends, so there are no rows to backfill; it lands in M2 with sstate, before any
-- production rows exist).


-- REFCOUNTS ARE MAINTAINED BY THE DATABASE, not by Go arithmetic. A refcount is a
-- materialized aggregate over cache_objects, and the only way to keep a materialized
-- aggregate honest under a mutating population is to derive it FROM the mutation.
-- Three things fall out free, each of which the hand-written version gets wrong:
--   1. THE /ac/ OVERWRITE. /ac/ is the one MUTABLE namespace: a PUT repoints a key at a
--      new blob, which must decrement the old and increment the new ATOMICALLY. This is
--      the single easiest refcount bug to write in Go; it leaks bytes forever, silently,
--      and only on the ccache path -- i.e. not in your test, and yes in production.
--   2. Chunked project purges keep every refcount exact with no bookkeeping in the caller.
--   3. Any future ON DELETE CASCADE that reaches cache_objects fires this per row, so
--      refcounts survive teardown paths nobody has written yet.
--
-- This does NOT weaken "cache.Deps must not carry *repository.Queries" and it does not
-- weaken "blob.Service is the only writer of object metadata". That rule is about
-- LAYERING. This is about ARITHMETIC -- and arithmetic is what a database is for. The
-- BYTE ORDERING (bytes-then-metadata, metadata-then-bytes) stays entirely in
-- blob.Service, where it belongs, because only blob.Service can sequence storage I/O
-- against the digest lock.
CREATE FUNCTION cache_objects_refcount() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE blobs
           SET refcount = refcount + 1,
               unreferenced_since = NULL,
               updated_at = now()
         WHERE digest = NEW.digest;
        RETURN NULL;
    END IF;

    IF TG_OP = 'DELETE' THEN
        UPDATE blobs
           SET refcount = refcount - 1,
               -- `refcount` on the RHS is the PRE-update value, so this asks
               -- "is the NEW count 0?".
               unreferenced_since = CASE WHEN refcount - 1 = 0 THEN now() ELSE NULL END,
               updated_at = now()
         WHERE digest = OLD.digest;
        RETURN NULL;
    END IF;

    -- UPDATE, i.e. the /ac/ overwrite.
    IF NEW.digest IS DISTINCT FROM OLD.digest THEN
        -- Take BOTH row locks in a deterministic (digest) order FIRST. Without this,
        -- two transactions swapping the same pair of digests in opposite directions
        -- deadlock -- an ABBA that surfaces only under concurrent ccache writes and
        -- would be blamed on anything but this trigger.
        PERFORM 1 FROM blobs
          WHERE digest IN (OLD.digest, NEW.digest)
          ORDER BY digest
            FOR NO KEY UPDATE;

        UPDATE blobs
           SET refcount = refcount - 1,
               unreferenced_since = CASE WHEN refcount - 1 = 0 THEN now() ELSE NULL END,
               updated_at = now()
         WHERE digest = OLD.digest;

        UPDATE blobs
           SET refcount = refcount + 1,
               unreferenced_since = NULL,
               updated_at = now()
         WHERE digest = NEW.digest;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER cache_objects_refcount_aiud
    AFTER INSERT OR DELETE OR UPDATE OF digest ON cache_objects
    FOR EACH ROW EXECUTE FUNCTION cache_objects_refcount();
