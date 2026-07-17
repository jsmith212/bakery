-- Cache objects and blobs: the hot path, and the blob.Service write protocol.
--
-- REFCOUNTS ARE NOT IN THIS FILE, and their absence is deliberate. There is no
-- increment query and no decrement query: the trigger cache_objects_refcount_aiud
-- owns the arithmetic (see 000006_blobs_and_objects.up.sql). blob.Service owns the
-- byte ORDERING and the digest lock; the database owns the COUNTING. The /ac/
-- overwrite -- the one mutable namespace -- is why: a PUT that repoints a key must
-- decrement the old blob and increment the new one atomically, which is the single
-- easiest refcount bug to write in Go and it leaks bytes silently.

-- THE hot path: the sstate HEAD storm. One primary-key probe on
-- cache_objects_pkey (backend_id, namespace, key). No join to blobs (digest and
-- size are denormalised and pinned by a composite FK), no join to projects
-- (backend_id comes from the in-process route cache), no lock, no write.
--
-- A miss returns pgx.ErrNoRows. The handler renders that as 404 -- NEVER 403, and
-- never a 200 with an empty body: BitBake retries a 403 as a full-body GET.
--
-- name: StatObject :one
SELECT digest, size_bytes, updated_at
  FROM cache_objects
 WHERE backend_id = $1 AND namespace = $2 AND key = $3;

-- The BATCH probe: REAPI FindMissingBlobs asks "which of these N digests do you
-- have" in ONE RPC, and moon repeats a digest within a single request. This turns
-- the whole question into ONE round-trip and ONE index scan on cache_objects_pkey
-- (backend_id, namespace, key = ANY(...)) -- not N StatObject probes, which would
-- put the FindMissingBlobs storm straight onto Postgres.
--
-- Returns ONLY the keys that exist. Every requested key ABSENT from the result is a
-- miss, and blob.Service negative-caches it -- a cold moon build has every digest
-- missing, so a positive-only fill re-queries every digest on every request.
--
-- key = ANY($3::text[]): text[] is a BUILTIN, so pgx encodes a plain []string with
-- NO AfterConnect registration (unlike the enum arrays in internal/db/enums.go).
--
-- name: StatObjectsBatch :many
SELECT key, digest, size_bytes, updated_at
  FROM cache_objects
 WHERE backend_id = $1
   AND namespace  = $2
   AND key = ANY(sqlc.arg(keys)::text[]);

-- Step 1 of the blob.Service PUT transaction, and step 1 of the GC's physical
-- delete. Both take the SAME lock, keyed on the DIGEST rather than on a row --
-- which is the point, because the row is exactly what may not exist yet. Without
-- it: the GC commits a blob DELETE, a concurrent PUT sees no row and re-uploads
-- bit-identical bytes and inserts a live row, and then the GC unlinks the bytes
-- the new row names. Live metadata, no bytes, a permanent 500 -- reached by
-- obeying the ordering invariant. FOR UPDATE cannot lock a row that is not there.
--
-- name: LockBlobDigest :exec
SELECT pg_advisory_xact_lock(bakery_blob_lock_key($1));

-- Step 2 of the PUT. Absent, or state = 'pending_delete', means the bytes may
-- already be gone: MUST upload. state = 'live' means the bytes are durably in
-- storage.Store (that is what 'live' MEANS) and the write can be elided.
--
-- READ COMMITTED IS REQUIRED. Under REPEATABLE READ the snapshot is taken at the
-- first statement -- the advisory-lock acquisition, BEFORE the lock is granted --
-- so this SELECT would read a stale world and the whole protocol is unsafe. pgx
-- defaults to READ COMMITTED; a pgx.TxOptions{IsoLevel: RepeatableRead} anywhere
-- on a blob path is a silent correctness bug.
--
-- name: GetBlobForWrite :one
SELECT digest, size_bytes, refcount, state
  FROM blobs
 WHERE digest = $1
   FOR UPDATE;

-- Step 4 of the PUT: create-or-revive, AFTER storage.Put returned. [BYTES FIRST,
-- THEN METADATA.] Reviving resets created_at and live_xid because a revived blob
-- is NEW to the GC -- conservative, therefore safe. refcount is untouched here;
-- the trigger owns it.
--
-- The WHERE on the DO UPDATE means a same-digest-different-size row updates zero
-- rows rather than silently corrupting the size: sha256 broke, or we lied. Fail
-- loudly.
--
-- name: CreateOrReviveBlob :execrows
INSERT INTO blobs (digest, size_bytes)
VALUES ($1, $2)
ON CONFLICT (digest) DO UPDATE
   SET state             = 'live',
       delete_started_at = NULL,
       created_at        = now(),
       live_xid          = pg_current_xact_id()::text::bigint,
       updated_at        = now()
 WHERE blobs.size_bytes = EXCLUDED.size_bytes;

-- Step 5 of the PUT: the metadata. The trigger fires and does refcount + 1 and
-- unreferenced_since = NULL.
--
-- Immutable namespaces (sstate, downloads, /cas/, OCI blobs) use DO NOTHING;
-- :execrows tells the caller whether it won the race.
--
-- name: PutObjectImmutable :execrows
INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (backend_id, namespace, key) DO NOTHING;

-- The /ac/ OVERWRITE: the ONLY mutable namespace (ccache, sccache,
-- moon-over-HTTP). The trigger's UPDATE branch decrements the old blob and
-- increments the new one atomically, taking both row locks in digest order so two
-- transactions swapping the same pair of digests cannot deadlock. An idempotent
-- re-PUT of identical content is a no-op and does not disturb the count.
--
-- name: PutObjectOverwritable :execrows
INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (backend_id, namespace, key) DO UPDATE
   SET digest     = EXCLUDED.digest,
       size_bytes = EXCLUDED.size_bytes,
       created_at = now(),
       live_xid   = pg_current_xact_id()::text::bigint,
       updated_at = now();

-- Deleting the object metadata IS the refcount decrement -- the trigger does it.
-- [METADATA FIRST.] The bytes are never touched here: ONLY the GC deletes bytes,
-- and cache_objects_blob_fk (ON DELETE RESTRICT) guarantees the blob row, and
-- therefore the bytes, cannot vanish while any object still names it.
--
-- name: DeleteObject :execrows
DELETE FROM cache_objects
 WHERE backend_id = $1 AND namespace = $2 AND key = $3;

-- The chunked purge that ON DELETE RESTRICT forces on us when a project is torn
-- down. Every row deleted fires the trigger, so the refcounts stay exact for free
-- and the caller keeps no bookkeeping.
--
-- name: DeleteObjectsChunk :execrows
DELETE FROM cache_objects o
 USING (
     SELECT c.backend_id, c.namespace, c.key
       FROM cache_objects c
      WHERE c.backend_id = $1
      LIMIT $2
 ) doomed
 WHERE o.backend_id = doomed.backend_id
   AND o.namespace  = doomed.namespace
   AND o.key        = doomed.key;
