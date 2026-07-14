-- Yocto hash equivalence (M3). These two tables are the GC ROOT: an sstate object's
-- filename embeds the unihash, so sstate is reachable only FROM a unihash row.
--
-- THERE IS NO UPDATE IN THIS FILE, and there never may be one.
-- (backend_id, method, taskhash) -> unihash is WRITE-ONCE: decided by the first
-- reporter, never revised. Repointing a taskhash at a new unihash renames the sstate
-- object that every build already in flight is fetching -- a cache-wide miss with no
-- error raised anywhere.
--
-- THERE IS NO gc-mark QUERY EITHER. Upstream's client-driven mark-and-sweep is
-- deliberately not implemented (Bakery collects in-process in M6) and the column it
-- would need does not exist. See 000010_hashserv.up.sql.
--
-- Every query is scoped by backend_id, which is the leading column of every index on
-- both tables. A query here that is not so scoped is a cross-tenant read.

-- `get` (all = false), and the `get-stream` hot path: ONE probe on the primary key
-- (backend_id, method, taskhash). No join, no sort, no lock.
--
-- A miss is pgx.ErrNoRows and is NOT an error condition: it is the ordinary state of
-- a task nobody has reported yet, and the handler answers it with a null unihash --
-- or, when chaining is on, with an upstream lookup.
--
-- name: GetUnihash :one
SELECT unihash
  FROM hashserv_unihashes
 WHERE backend_id = $1 AND method = $2 AND taskhash = $3;

-- `get` (all = true): the unihash PLUS the outhash row behind it. A taskhash can have
-- produced several different outputs over time and only one row can be reported back,
-- so upstream takes the OLDEST -- same tie-break as the equivalence itself. Not on any
-- build's path (bitbake always calls `get` with all = false); this serves
-- bitbake-hashclient and the conformance driver.
--
-- backend_id and live_xid are deliberately not selected here or in any other query in
-- this file: internal ids must not reach an RPC response by accident, and a
-- SELECT * would put them one json.Marshal away from the wire.
--
-- name: GetUnihashFull :one
SELECT o.method, o.taskhash, o.outhash, o.created_at,
       o.owner, o.pn, o.pv, o.pr, o.task, o.outhash_siginfo,
       u.unihash
  FROM hashserv_outhashes o
  JOIN hashserv_unihashes u
    ON u.backend_id = o.backend_id
   AND u.method     = o.method
   AND u.taskhash   = o.taskhash
 WHERE o.backend_id = $1 AND o.method = $2 AND o.taskhash = $3
 ORDER BY o.created_at ASC
 LIMIT 1;

-- WRITE-ONCE, and :execrows IS THE SEMANTICS: 1 means this caller minted the unihash,
-- 0 means another writer got there first and theirs stands.
--
-- No RETURNING, on purpose. RETURNING + :one reports the conflict as pgx.ErrNoRows --
-- i.e. as a NOT-FOUND -- and every caller then has to remember that "no row" really
-- means "a successful concurrent insert". Step 4 of the report algorithm re-reads the
-- winning row regardless (another writer may have won the race), so the row is not
-- what the caller needs back from here. Only whether it lost.
--
-- name: InsertUnihash :execrows
INSERT INTO hashserv_unihashes (backend_id, method, taskhash, unihash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (backend_id, method, taskhash) DO NOTHING;

-- THE GATE FOR THE ENTIRE EQUIVALENCE ALGORITHM (spec §6 step 3). rows = 1 means this
-- (method, taskhash, outhash) is NEW and equivalence must be computed; rows = 0 means
-- it is a duplicate report and the step collapses to a cheap re-read. Lose that signal
-- and either every report re-derives equivalence -- correct, but it turns a
-- HEAD-storm-scale path into a sort -- or a genuinely new output skips equivalence and
-- mints a unihash that diverges from the one the identical output already has.
--
-- DO NOTHING, never DO UPDATE: a re-report of the same output must not rewrite
-- created_at, because created_at is the column "oldest wins" is decided on. A client
-- re-reporting an old task would otherwise make its taskhash the youngest and silently
-- change which unihash the NEXT equivalence picks.
--
-- name: InsertOuthash :execrows
INSERT INTO hashserv_outhashes (
    backend_id, method, taskhash, outhash,
    owner, pn, pv, pr, task, outhash_siginfo
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (backend_id, method, taskhash, outhash) DO NOTHING;

-- THE EQUIVALENCE. Two taskhashes that produced the same OUTPUT are the same task as
-- far as sstate is concerned, so the newcomer adopts the incumbent's unihash and reuses
-- its sstate object. That reuse is the entire point of the system.
--
-- `o.taskhash <> $4` excludes the row `report` has just inserted; without it every
-- report is trivially equivalent to itself and no remap ever happens.
--
-- OLDEST WINS (ORDER BY o.created_at ASC): that is what makes a unihash stable across
-- time instead of flapping to whichever build reported most recently. Rides
-- hashserv_outhashes_equiv (backend_id, method, outhash, created_at), which covers both
-- the predicate and the sort, so LIMIT 1 stops after one index tuple.
--
-- name: GetEquivalentForOuthash :one
SELECT o.taskhash, u.unihash
  FROM hashserv_outhashes o
  JOIN hashserv_unihashes u
    ON u.backend_id = o.backend_id
   AND u.method     = o.method
   AND u.taskhash   = o.taskhash
 WHERE o.backend_id = $1
   AND o.method     = $2
   AND o.outhash    = $3
   AND o.taskhash  <> $4
 ORDER BY o.created_at ASC
 LIMIT 1;

-- `get-outhash`. The primary key makes this exactly one row, so no ORDER BY and no
-- LIMIT: there is nothing to tie-break.
--
-- name: GetOuthash :one
--
-- `get-outhash` with with_unihash = false: the raw outhash row.
--
-- IT DOES NOT FILTER ON taskhash, AND THAT IS NOT AN OVERSIGHT. Upstream's
-- sqlite.get_outhash(method, outhash) keys on (method, outhash) alone and takes the OLDEST
-- row. The RPC accepts a taskhash and then ignores it, because the question being asked is
-- "has ANY task ever produced this output?", not "has THIS task produced it?" -- and the
-- answer is what lets a client discover an equivalence it did not already know about.
--
-- Filtering by taskhash here looks tighter and is silently catastrophic: it can only ever
-- return the caller's own row, so report_readonly (the anonymous / read-scoped path) would
-- find an equivalent exactly never, and an open mirror would hand back the client's own
-- unihash forever while appearing to work.
SELECT method, taskhash, outhash, created_at,
       owner, pn, pv, pr, task, outhash_siginfo
  FROM hashserv_outhashes
 WHERE backend_id = $1 AND method = $2 AND outhash = $3
 ORDER BY created_at ASC
 LIMIT 1;

-- `get-outhash` with with_unihash = true -- the form an upstream-chaining server asks
-- of us, and the form we ask of an upstream when `report` is about to mint a brand-new
-- unihash (§6 step 3b).
--
-- The JOIN is the filter, not an embellishment: an outhash whose taskhash has no
-- unihash row yet is not answerable, and must return NOTHING rather than a row with a
-- hole where the unihash goes. A caller that adopted such a row would adopt "".
--
-- name: GetOuthashWithUnihash :one
--
-- Like GetOuthash, this keys on (method, outhash) and takes the OLDEST row -- NOT on
-- taskhash. It mirrors upstream's sqlite.get_unihash_by_outhash(method, outhash), and the
-- comment on GetOuthash explains why the taskhash must not appear in the predicate.
SELECT o.method, o.taskhash, o.outhash, o.created_at,
       o.owner, o.pn, o.pv, o.pr, o.task, o.outhash_siginfo,
       u.unihash
  FROM hashserv_outhashes o
  JOIN hashserv_unihashes u
    ON u.backend_id = o.backend_id
   AND u.method     = o.method
   AND u.taskhash   = o.taskhash
 WHERE o.backend_id = $1 AND o.method = $2 AND o.outhash = $3
 ORDER BY o.created_at ASC
 LIMIT 1;

-- `exists-stream`: one of these per line, on a streaming connection, for as many lines
-- as the client feels like sending. Rides hashserv_unihashes_unihash.
--
-- Scoped by backend_id and NOT by method, mirroring upstream's method-agnostic
-- unihash_exists: a unihash is a sha256 over a whole task signature, so a cross-method
-- collision is not a thing that happens, and adding method to the predicate would
-- answer a question the client did not ask.
--
-- SELECT EXISTS, not SELECT ... LIMIT 1: on this stream a miss is the COMMON answer,
-- and it must come back as `false`, not as pgx.ErrNoRows. Turning the common answer
-- into an error path is how a stream loop learns to swallow errors -- and a stream loop
-- that swallows an error drops a response, which desynchronizes the connection
-- permanently and silently.
--
-- name: UnihashExists :one
SELECT EXISTS (
    SELECT 1
      FROM hashserv_unihashes
     WHERE backend_id = $1 AND unihash = $2
);

-- THE `remove` RPC (spec §1: implemented, project-scoped, write-scoped key only).
--
-- The wire form is an ARBITRARY column filter -- {"where": {col: val}} -- which sqlc
-- cannot express and which we will not hand-assemble into SQL. Upstream applies the
-- filter to each table INDEPENDENTLY, using only the columns that table actually has,
-- and returns the SUM of the two row counts; a filter naming no column of a table
-- deletes nothing from it. So `unihash` reaches only the unihash table and `outhash`
-- only the outhash table, while `taskhash` and `method` reach both -- an asymmetry
-- upstream's own tests depend on, not an accident.
--
-- These four columns are exactly what the upstream conformance suite filters on
-- (bitbake tests.py: test_remove_taskhash / _unihash / _outhash / _method). A filter
-- naming any other column must be REFUSED by the handler, not silently widened: a
-- `remove` that quietly matches everything is data loss, and this is the one RPC that
-- can destroy a GC root.
--
-- Each variant is scoped by backend_id -- the leading column of the primary key on
-- both tables -- so each is an index range scan over exactly one tenant and cannot
-- reach another's rows.

-- name: RemoveUnihashesByTaskhash :execrows
DELETE FROM hashserv_unihashes
 WHERE backend_id = $1 AND taskhash = $2;

-- name: RemoveOuthashesByTaskhash :execrows
DELETE FROM hashserv_outhashes
 WHERE backend_id = $1 AND taskhash = $2;

-- `unihash` is a column of hashserv_unihashes ALONE, so upstream's filter deletes from
-- that table only and the outhash rows stand. Reproduced exactly: test_remove_unihash
-- asserts that `get` on the taskhash now misses -- not that the output record is gone.
--
-- name: RemoveUnihashesByUnihash :execrows
DELETE FROM hashserv_unihashes
 WHERE backend_id = $1 AND unihash = $2;

-- `outhash` is a column of hashserv_outhashes ALONE. The mirror image: the unihash rows
-- stand, which is why test_remove_outhash asserts only that `get-outhash` now misses.
--
-- name: RemoveOuthashesByOuthash :execrows
DELETE FROM hashserv_outhashes
 WHERE backend_id = $1 AND outhash = $2;

-- name: RemoveUnihashesByMethod :execrows
DELETE FROM hashserv_unihashes
 WHERE backend_id = $1 AND method = $2;

-- name: RemoveOuthashesByMethod :execrows
DELETE FROM hashserv_outhashes
 WHERE backend_id = $1 AND method = $2;
