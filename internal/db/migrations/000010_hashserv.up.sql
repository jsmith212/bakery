-- Bakery M3: Yocto hash equivalence. Two tables, and they are the GC ROOT.
--
-- The sstate object's filename EMBEDS the unihash, so an sstate object is reachable
-- only FROM a unihash row: delete the unihash and the object it names becomes both
-- unreferenced and unfindable, in that order. hashserv is therefore not "one more
-- backend" -- it roots the one that matters, and cache_backends' UNIQUE
-- (project_id, kind) is what makes the sstate <-> hashserv pairing 1:1 by
-- construction. Always sweep hashserv before sstate.
--
-- WHAT IS DELIBERATELY ABSENT (spec 2026-07-13-m3-hashserv.md §1):
--
--   NO gc_mark COLUMN AND NO config TABLE. Upstream hashserv ships its own
--   mark-and-sweep GC -- `gc-mark` / `gc-sweep`, with the current mark parked in a
--   `config` table -- and it is driven BY THE CACHE CLIENT, over the RPC. Bakery
--   collects in-process in M6, against this database, under the write barrier below.
--   Building upstream's mechanism too would put a SECOND, COMPETING collector on
--   these exact rows, reachable by anyone holding a cache credential, in exchange for
--   nothing: bitbake never calls those RPCs (grepped -- zero call sites in lib/bb).
--   Two collectors that disagree about what is reachable delete a live build's
--   unihashes, and the bill for that is a four-hour rebuild. So the RPCs are refused
--   AND the columns they would need do not exist, which is what stops someone
--   quietly reintroducing the second collector one column at a time.
--
--   NO SURROGATE id. Upstream carries one because SQLAlchemy wants one. Every access
--   here is by (backend_id, method, taskhash[, outhash]) -- the natural key -- so an
--   id would be a second identity that can drift from the first, plus a second btree
--   on a table nothing references by id.

CREATE TABLE hashserv_unihashes (
    -- The multi-tenancy boundary, and the LEADING COLUMN of every index here. Two
    -- projects reporting the same taskhash are unrelated facts and must not see each
    -- other. RESTRICT for the reason cache_objects has it: dropping a backend that
    -- still roots sstate objects is refused, never cascaded.
    backend_id bigint NOT NULL REFERENCES cache_backends (id) ON DELETE RESTRICT,
    method     text   NOT NULL,
    taskhash   text   NOT NULL,
    unihash    text   NOT NULL,

    -- THE M6 WRITE BARRIER, BOTH HALVES, as blobs and cache_objects carry it
    -- (000006). It lands WITH the table rather than with the sweep that reads it,
    -- because retrofitting a barrier onto a table that has been accumulating rows
    -- without one is not a migration, it is an outage.
    --
    -- created_at is the documented half (CLAUDE.md: created_at < gc_run.started_at).
    -- It is the cheap indexable prefilter, and ON ITS OWN IT IS NOT THE INVARIANT.
    -- now() is TRANSACTION-START time, so a build that BEGINs before a GC run starts
    -- and COMMITs after it mints a unihash whose created_at PREDATES
    -- gc_runs.started_at while being invisible to the GC's snapshot: the timestamp
    -- predicate selects the row, and a sweep that trusts it eats the fresh unihash of
    -- a running build -- and with it the sstate object whose filename embeds that
    -- unihash. This table is the one CLAUDE.md names when it states the rule.
    --
    --   pg_visible_in_snapshot(live_xid::text::xid8, gc_runs.snapshot::pg_snapshot)
    --
    -- closes the hole exactly. bigint and not xid8 because pgx and sqlc have no
    -- dependable codec for xid8; the ::text:: round-trip is exact for every value a
    -- real cluster holds. (The M3 spec's §5 sketch shows created_at alone. That is
    -- the half-barrier CLAUDE.md forbids, so both halves land here.)
    created_at timestamptz NOT NULL DEFAULT now(),
    live_xid   bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),

    -- WRITE-ONCE, and the PK is what enforces it: (backend_id, method, taskhash) ->
    -- unihash is decided by the first reporter and never changes. Every writer is
    -- INSERT ... ON CONFLICT DO NOTHING. An UPDATE path would repoint a taskhash at a
    -- different unihash, which RENAMES the sstate object that every build already in
    -- flight is fetching -- a cache-wide miss storm with no error anywhere. There is
    -- deliberately no query in query/hashserv.sql that updates this table.
    PRIMARY KEY (backend_id, method, taskhash)
);

-- `exists-stream` (a per-line probe on the streaming hot path) and remove-by-unihash.
-- Without it both scan the whole of one tenant's unihash space, per line.
CREATE INDEX hashserv_unihashes_unihash ON hashserv_unihashes (backend_id, unihash);


CREATE TABLE hashserv_outhashes (
    backend_id      bigint NOT NULL REFERENCES cache_backends (id) ON DELETE RESTRICT,
    method          text   NOT NULL,
    taskhash        text   NOT NULL,
    outhash         text   NOT NULL,

    -- Both halves again, and here created_at is load-bearing TWICE: it is the GC
    -- prefilter AND it is what "oldest wins" is decided on (see the equiv index).
    created_at      timestamptz NOT NULL DEFAULT now(),
    live_xid        bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),

    -- Provenance. Nullable because the client sends only the keys it has, and
    -- upstream copies only the keys present in the report. Read exclusively by
    -- `get-outhash` and `get(all=true)`; never on a build's path. The wire spells
    -- three of these PN / PV / PR in UPPERCASE -- the columns are lowercase and the
    -- handler maps.
    owner           text,
    pn              text,
    pv              text,
    pr              text,
    task            text,

    -- A PLAIN text COLUMN, TOASTed by Postgres, exactly as upstream stores it -- and
    -- emphatically NOT a blob.Service object. Routing it through the refcounted store
    -- would make the unihash -- the GC ROOT -- depend on the refcounted object store
    -- that the unihash is supposed to root, and a cycle in the reachability graph is
    -- how a GC eats its own roots. A ~128 KiB siginfo (upstream's test_huge_message
    -- reports one) is precisely what TOAST exists for, and it is fetched out of line
    -- only when something actually asks for it.
    outhash_siginfo text,

    -- Upstream's UNIQUE (method, taskhash, outhash), promoted to the primary key
    -- because it IS the identity. It is also the gate for the whole equivalence
    -- algorithm: `report` computes equivalence only when an INSERT here CREATED a row
    -- (§6 step 3), so this constraint is what turns a duplicate report into a cheap
    -- lookup instead of a re-derivation on every reported task of every build.
    PRIMARY KEY (backend_id, method, taskhash, outhash)
);

-- THE EQUIVALENCE QUERY, and the reason a unihash is stable over time: given a newly
-- reported (method, outhash), find the OLDEST OTHER taskhash that produced the same
-- output; that row's unihash wins. This index covers the predicate
-- (backend_id, method, outhash) AND the ORDER BY (created_at), so the sort is free and
-- the LIMIT 1 stops after a single index tuple. Ordering by anything else, or leaving
-- created_at out of the index, makes the winner depend on physical row order -- i.e.
-- makes a unihash flap on VACUUM.
CREATE INDEX hashserv_outhashes_equiv
    ON hashserv_outhashes (backend_id, method, outhash, created_at);
