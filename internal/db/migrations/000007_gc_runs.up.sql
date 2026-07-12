-- Bakery M1: GC bookkeeping.
--
-- The GC LOOP is M6. The WRITE BARRIER it depends on is a property of the SCHEMA, and
-- it lands NOW, with its tests, because retrofitting a barrier onto tables that have
-- been accumulating rows without one is not a migration -- it is an outage.
--
-- A gc_runs row is INSERTed and COMMITTED before any scanning begins. Its started_at
-- and snapshot are frozen as of that transaction, and every sweep statement afterwards
-- filters against them.
CREATE TABLE gc_runs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- THE WRITE BARRIER, documented form. Every sweep predicate carries
    -- `<table>.created_at < gc_runs.started_at`. Indexable, cheap, the prefilter.
    started_at      timestamptz NOT NULL DEFAULT now(),

    -- THE WRITE BARRIER, correct form. The timestamp form alone is NOT sufficient:
    -- now() is transaction-start time, so a build that began before the run started and
    -- commits during it produces a row whose created_at predates started_at. The sweep
    -- predicate MUST also carry
    --     pg_visible_in_snapshot(<table>.live_xid::text::xid8, gc_runs.snapshot::pg_snapshot)
    -- which is true precisely when the transaction that made the row live was committed
    -- and visible when this run took its snapshot. Their conjunction is at least as
    -- conservative as either alone, and it converges (a later run correctly sweeps the
    -- row the earlier run spared).
    --
    -- text, not pg_snapshot: pgx/sqlc have no codec for pg_snapshot. It is NEVER selected
    -- into Go -- it is only ever referenced inside SQL predicates, where it is cast back.
    snapshot        text NOT NULL DEFAULT (pg_current_snapshot()::text),

    -- Frozen into the row so a run is reproducible and auditable, and so that changing
    -- the configured grace period mid-run cannot shorten the grace of a sweep already in
    -- flight.
    grace_period    interval NOT NULL CHECK (grace_period >= interval '0'),

    status          gc_run_status NOT NULL DEFAULT 'running',
    finished_at     timestamptz,
    error           text,

    objects_deleted bigint NOT NULL DEFAULT 0 CHECK (objects_deleted >= 0),
    blobs_marked    bigint NOT NULL DEFAULT 0 CHECK (blobs_marked    >= 0),
    blobs_deleted   bigint NOT NULL DEFAULT 0 CHECK (blobs_deleted   >= 0),
    bytes_reclaimed bigint NOT NULL DEFAULT 0 CHECK (bytes_reclaimed >= 0),

    -- A run is finished IFF it is not running, and has an error IFF it failed. Neither
    -- nullable may drift out of agreement with `status`, so neither can come to mean two
    -- things.
    CONSTRAINT gc_runs_finished_iff_not_running
        CHECK ((status = 'running') = (finished_at IS NULL)),
    CONSTRAINT gc_runs_error_iff_failed
        CHECK ((status = 'failed') = (error IS NOT NULL)),
    CONSTRAINT gc_runs_finished_after_started
        CHECK (finished_at IS NULL OR finished_at >= started_at)
);

-- At most ONE running GC at a time, ENFORCED rather than assumed. Two concurrent sweeps
-- would each hold a snapshot and each believe the other's in-flight writes were
-- sweepable: mark-sweep with a live mutator, where the mutator is the other GC. Every
-- row matching the predicate has the same value in the indexed column, so a second
-- concurrent run is a unique violation rather than a race.
CREATE UNIQUE INDEX gc_runs_single_active_idx ON gc_runs (status) WHERE status = 'running';

CREATE INDEX gc_runs_started_at_idx ON gc_runs (started_at DESC);

-- SCOPE: GLOBAL. Blobs are deduped across projects, so their sweep is inherently global,
-- and the boot-time pg_try_advisory_lock already guarantees a single writing instance.
-- A per-project scope column today would be a nullable meaning "all" -- a nullable
-- encoding two things, the one thing this schema refuses to do. M6 adds real scoping (and
-- the per-scope unique index) when it adds the per-backend retention sweep it needs it for.
