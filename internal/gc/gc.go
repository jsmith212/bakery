// Package gc is the M6 sweep engine: retention, quota eviction and byte
// reclamation. docs/design/specs/2026-08-14-m6-gc-retention-quotas.md is the
// contract and the section numbers in these comments refer to it.
//
// # Two layers, and this package only ORCHESTRATES them
//
// Layer A is metadata: namespace-scoped sweeps of cache_objects,
// hashserv_unihashes and hashserv_outhashes. Layer B is bytes: the already-built
// MarkBlobsPendingDelete -> ReapDigest machinery. Nothing here re-implements
// either. Every universal predicate -- the write barrier's two halves, the
// coalesce(accessed_at, created_at) liveness rule -- lives in the SQL
// (internal/db/query/gc.sql); this package decides WHICH rows a stage considers,
// in what ORDER the stages run, and how fast.
//
// # The order is the design
//
// hashserv unihashes are the GC ROOT and are swept BEFORE sstate, because an
// sstate object's filename embeds the unihash. OCI tags are swept before
// manifests before OCI blobs, because a tag names a manifest and a manifest names
// blobs. ac/ac-grpc/sccache are swept before cas, because an ActionResult names
// CAS digests. Layer B runs last, globally, and only ever sees digests Postgres
// has already agreed are unreferenced. Getting this order wrong does not produce
// an error; it produces a cache that answers "present" for an object whose bytes
// are gone, which Bazel reports as a build abort and rewind.
//
// # Deletes go through blob.Service.DeleteBatch and nowhere else
//
// DeleteObjectsChunk is forbidden here (spec §2): it does not invalidate the LRU,
// and the LRU serves POSITIVE answers with zero database contact. A GC that
// deleted rows behind it would leave a process-wide memory of objects that no
// longer exist, and no query would ever be issued that could correct it.
//
// # Single writer, and that is load-bearing three times
//
// The loop refuses to start under --allow-multi-instance (spec §9.4): the LRU
// invalidation is process-local, the boot reaper would otherwise fail another
// instance's live sweep, and §6.2's pending-touch veto is only sound because THIS
// process's record of unflushed reads is complete.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Trigger is what started a run. It is a CLOSED set and it is written to
// gc_runs.trigger, which carries a CHECK constraint of the same two values: a
// third value here is a constraint violation at run start, not a bad label.
type Trigger string

const (
	// TriggerInterval is the in-process loop.
	TriggerInterval Trigger = "interval"
	// TriggerAPI is POST /api/v1/gc/run.
	TriggerAPI Trigger = "api"
	// TriggerUsage is the lightweight --gc-usage-interval measurement pass
	// (MeasureUsage), which mints its own gc_runs row purely so the measurement is
	// auditable (R7#12). It is a THIRD value, not a reuse of TriggerInterval: that
	// pass runs every six hours forever, independent of whether retention is even
	// enabled, and a runs list that cannot tell those rows from real sweeps is a
	// list an operator stops reading. GET /api/v1/gc/runs excludes them unless
	// asked (ListGCRuns' include_usage).
	TriggerUsage Trigger = "usage"
)

// ErrAlreadyRunning means a real sweep is already in flight in this process. The
// API trigger renders it 409.
//
// It guards REAL runs only. A dry run writes nothing, holds no gc_runs slot
// (000012 replaced the unique index predicate with `status = 'running' AND NOT
// dry_run`), and may therefore overlap freely -- refusing one would block an
// operator's `--dry-run` behind an unrelated sweep for no safety benefit.
var ErrAlreadyRunning = errors.New("gc: a sweep is already running")

// ErrMultiInstance means a sweep was asked for under --allow-multi-instance. See
// the package doc for the three reasons. The API trigger renders it 409.
//
// IT GUARDS TriggerAsync AS WELL AS THE LOOP (R7#1). Loop's refusal alone is not
// the invariant: POST /api/v1/gc/run reaches the engine directly, so an operator
// under --allow-multi-instance could start -- by hand, against a live second
// instance -- exactly the sweep the loop refuses to schedule, with the same three
// consequences and none of the warnings.
var ErrMultiInstance = errors.New("gc: refusing to run under --allow-multi-instance")

// ErrDisabled means a sweep was asked for while --gc-enabled is off. The API
// trigger renders it 409, distinctly from ErrMultiInstance: "this deployment has
// GC turned off" and "this deployment cannot run GC safely" are different
// answers, and only the first is something the operator can fix with a flag they
// already know about.
//
// The trigger is refused rather than quietly honoured because --gc-enabled off is
// a deliberate operational state (an upgrade window, an incident) that a POST
// must not silently override.
var ErrDisabled = errors.New("gc: refusing to run because gc is disabled")

const (
	// disabledBackendCeiling caps a DISABLED backend's retention window (spec §3,
	// finding 10). Disabled is a STRONGER retention signal, not an exemption: a
	// backend serving no traffic never touches accessed_at, so its rows would sit at
	// their created_at forever pinning deduped digests that other, live backends
	// have long stopped naming. There is deliberately no "disabled but preserved"
	// state -- that would have to be a new explicit column, not an inferred meaning
	// of `enabled = false`.
	disabledBackendCeiling = 30 * 24 * time.Hour

	// siginfoWindowFactor is W_siginfo = 4 x W_hashserv (spec §4). outhash_siginfo is
	// ~128 KiB of TOASTed text read only by RPCs that are on no build's path, so
	// nulling it is pure space reclamation and can afford to be four times as patient
	// as the rows it hangs off.
	siginfoWindowFactor = 4

	// casWindowFactor is W_cas = 2 x W_ac, and W_manifests = W_ociblobs = 2 x W_tags
	// (spec §4). The named must outlive the namer: an ActionResult names CAS digests,
	// a tag names a manifest, a manifest names blobs. It is defence in depth behind
	// the §6.3 reachability touch, not a substitute for it.
	casWindowFactor = 2
)

const (
	// chunkTimeout bounds ONE statement -- a scan page, a batch delete, a sweep. It
	// is per chunk rather than per run on purpose: a run legitimately takes hours, so
	// a run-wide deadline would either be uselessly large or would kill a healthy
	// sweep, while a single chunk that takes half a minute means something is wrong
	// with THAT chunk.
	chunkTimeout = 30 * time.Second

	// finishTimeout bounds the terminal FinishGCRun, which runs under
	// context.WithoutCancel because the ordinary reason for reaching it is that ctx
	// was cancelled.
	finishTimeout = 5 * time.Second

	// reapRounds bounds the pending_delete drain. The loop already stops as soon as a
	// round makes no progress; this is the second bound, for the pathological case
	// where every round reaps one row out of a permanently failing set.
	reapRounds = 1000
)

// Queries is the sqlc surface the engine needs. *db.Store satisfies it by
// embedding *repository.Queries.
//
// Every write here is a SINGLE statement that is atomic on its own, so none of
// them is routed through db.Store.Tx: there is no multi-statement protocol in this
// package to protect. The one place that DOES need a transaction -- the reap's
// lock/verify/unlink/delete sequence -- lives in blob.Service.ReapDigest, behind
// the Blobs interface below, exactly where the ordering invariant is enforced.
type Queries interface {
	StartGCRun(ctx context.Context, arg repository.StartGCRunParams) (repository.StartGCRunRow, error)
	FinishGCRun(ctx context.Context, arg repository.FinishGCRunParams) (int64, error)
	MarkOrphanedGCRunsFailed(ctx context.Context) (int64, error)

	ListBackendsForGC(ctx context.Context) ([]repository.ListBackendsForGCRow, error)
	ScanObjectsForGC(
		ctx context.Context, arg repository.ScanObjectsForGCParams,
	) ([]repository.ScanObjectsForGCRow, error)

	UnihashesExistBatch(ctx context.Context, arg repository.UnihashesExistBatchParams) ([]string, error)
	HashservBackendHasUnihashes(ctx context.Context, backendID int64) (bool, error)

	SweepUnihashes(ctx context.Context, arg repository.SweepUnihashesParams) (int64, error)
	DryRunSweepUnihashes(ctx context.Context, arg repository.DryRunSweepUnihashesParams) (int64, error)
	SweepOrphanedOuthashes(ctx context.Context, arg repository.SweepOrphanedOuthashesParams) (int64, error)
	DryRunSweepOrphanedOuthashes(
		ctx context.Context, arg repository.DryRunSweepOrphanedOuthashesParams,
	) (int64, error)
	NullOrphanSiginfo(ctx context.Context, arg repository.NullOrphanSiginfoParams) (int64, error)
	DryRunNullOrphanSiginfo(ctx context.Context, arg repository.DryRunNullOrphanSiginfoParams) (int64, error)

	// SweepUnreferencedManifests is stage 8's ONLY deletion path (E: R6#4/R7#9). It
	// evaluates the write barrier AND the tag anti-join at DELETE time, in one
	// statement, and returns the keys it deleted so the caller can invalidate them
	// from the LRU.
	SweepUnreferencedManifests(
		ctx context.Context, arg repository.SweepUnreferencedManifestsParams,
	) ([]string, error)
	DryRunSweepUnreferencedManifests(
		ctx context.Context, arg repository.DryRunSweepUnreferencedManifestsParams,
	) (int64, error)

	// GetGCState reads the one-row touch-staleness ramp clock (spec §6.4). Read ONCE,
	// at boot -- see Engine.LoadTouchRamp.
	GetGCState(ctx context.Context) (pgtype.Timestamptz, error)

	MarkBlobsPendingDelete(
		ctx context.Context, arg repository.MarkBlobsPendingDeleteParams,
	) ([]repository.MarkBlobsPendingDeleteRow, error)
	ListPendingDeleteBlobs(ctx context.Context, limit int32) ([]repository.ListPendingDeleteBlobsRow, error)

	UpsertBackendUsage(ctx context.Context, arg repository.UpsertBackendUsageParams) error
	InstancePhysicalBytes(ctx context.Context) (int64, error)
}

// Blobs is the blob.Service surface. It is an interface for the same reason
// Queries is: so the engine's orchestration can be tested without a byte store,
// not because a second implementation is expected.
type Blobs interface {
	// DeleteBatch is the ONLY keyed deletion path the GC uses. It is digest-ordered
	// and it invalidates the LRU; DeleteObjectsChunk is neither.
	//
	// runID is the write barrier and is REQUIRED: DeleteObjectsByKeys re-derives
	// `created_at < run.started_at AND pg_visible_in_snapshot(...)` from the gc_runs
	// row at DELETE time, because the doomed set was computed by an earlier scan and a
	// build can overwrite any key in it in the gap.
	DeleteBatch(ctx context.Context, runID int64, refs []blob.DeleteRef) (int64, error)

	// InvalidateKeys drops LRU entries for keys deleted by a statement that did not
	// go through DeleteBatch -- today, stage 8's SweepUnreferencedManifests, whose
	// anti-join must be evaluated in SQL at delete time and which therefore reports
	// what it deleted rather than being told. It is the same obligation DeleteBatch
	// discharges internally: manifests are served through blob.Get, so a surviving
	// positive entry answers "present" for a row that is gone.
	InvalidateKeys(backendID int64, namespace string, keys []string)

	// ReapDigest unlinks one blob's bytes inside the transaction that deletes its
	// row. Returns false when the blob was revived or already reaped -- both normal.
	ReapDigest(ctx context.Context, digest blob.Digest) (bool, error)

	// PendingTouch is §6.2's veto: "does this process hold an unflushed read of this
	// key?". A pending touch is INVISIBLE to the sweep's SELECT, so a key read
	// seconds ago can be selected as ninety days cold.
	PendingTouch(backendID int64, namespace, key string) bool
}

// Config is the §9.8 knob set, already resolved from the CLI.
type Config struct {
	// Enabled is --gc-enabled. Off stops the loop, the usage pass and the API
	// trigger; it does not stop the toucher, which only maintains accessed_at.
	Enabled bool

	// AllowMultiInstance mirrors the server flag. When set the loop refuses to
	// start -- loudly. See the package doc.
	AllowMultiInstance bool

	Interval      time.Duration // --gc-interval
	UsageInterval time.Duration // --gc-usage-interval
	GracePeriod   time.Duration // --gc-grace-period, frozen per run at start
	BatchSize     int           // --gc-batch-size
	BatchPause    time.Duration // --gc-batch-pause

	// DisableRetention is --gc-disable-retention: the brake. It halts Layer A AND
	// Layer B's MARK (spec §9.6) -- the incident it serves is "we deleted things we
	// wanted", and those bytes are still sitting in the grace window; leaving the
	// mark running would convert a recoverable window into permanent loss at maximum
	// speed. Stage 0's re-drive of blobs ALREADY marked keeps running: those are past
	// recovery, and stalling them reclaims nothing.
	DisableRetention bool

	// TouchInterval is --gc-touch-interval, F: how often the access toucher flushes.
	// The toucher lives in blob.Service, but its two knobs are normalized here so a
	// ServeCmd assembled in Go rather than parsed by Kong -- a test, an embedder --
	// cannot hand it a zero interval, which is a panic in time.NewTicker rather than a
	// disabled flusher.
	TouchInterval time.Duration

	// TouchStaleness is --gc-touch-staleness, the STEADY-state T. The ramp
	// (spec §6.4) can only lengthen it. See Engine.TouchStaleness.
	TouchStaleness time.Duration
}

// AccessFlusher forces every pending cache_objects accessed_at mark to disk,
// synchronously, and reports the rows written. *blob.Service satisfies it.
//
// IT IS THE PRIMARY HALF OF §6.2 (A: R6#1/R7#3). A sweep stage reads accessed_at
// off the DATABASE row; a mark that is still only in this process's memory is
// invisible to that SELECT, so a key read five minutes ago is selected as ninety
// days cold. PendingTouch's veto catches that per chunk, but only for the blob
// path and only for marks that exist when the chunk is decided -- forcing the
// flush BEFORE the stage turns every mark older than the sweep into a real
// accessed_at that the coalesce() predicate spares on its own.
type AccessFlusher interface {
	FlushAccessNow(ctx context.Context) (int64, error)
}

// UnihashFlusher is AccessFlusher's hashserv_unihashes twin: *hashserv.Backend
// satisfies it. Stage 1 needs it MORE than the cache_objects stages need theirs,
// because stage 1 is a self-contained SQL DELETE with no per-chunk veto at all --
// there is no Go-side pass over its rows in which a pending mark could be
// consulted, so the flush is the only mechanism there is.
type UnihashFlusher interface {
	FlushNow(ctx context.Context) (int64, error)
}

// Deps is everything the engine needs from outside its own configuration.
type Deps struct {
	DB      Queries
	Blobs   Blobs
	Metrics *metrics.Metrics
	Log     *slog.Logger

	// Access and Unihash are the pre-sweep force-flushers (§6.2 mechanism 1).
	//
	// Both are OPTIONAL and nil-tolerant, because a harness that drives the engine
	// over fakes has no toucher to flush -- but a nil one in production means the
	// sweep decides on accessed_at values up to --gc-touch-staleness stale, so New
	// says so, loudly, once.
	Access  AccessFlusher
	Unihash UnihashFlusher
}

// Engine runs sweeps. One per process.
type Engine struct {
	db      Queries
	blobs   Blobs
	access  AccessFlusher
	unihash UnihashFlusher
	rec     *metrics.GCRecorder
	log     *slog.Logger
	cfg     Config

	// lifetime is the SERVER's context, captured at construction (R7#8), and it is
	// the one place in this codebase a context is deliberately held in a struct.
	//
	// TriggerAsync's detached sweep needs a context that outlives the HTTP request
	// that started it (the request's dies with the response, often the instant the
	// handler returns) but NOT the process. context.WithoutCancel(request) gave the
	// first property and lost the second: the sweep became uncancellable, so
	// shutdown could neither stop it nor wait for it, and it kept deleting rows
	// while the pool it deletes through was closing. Deriving from the server's own
	// lifetime instead gives a sweep that shuts down exactly like the interval
	// loop's -- finish the chunk, write the terminal gc_runs row under
	// WithoutCancel -- and asyncDone() is what lets Boot wait for that to happen.
	lifetime context.Context //nolint:containedctx // the process lifetime, see above

	// async counts detached (TriggerAsync) sweeps in flight so shutdown can wait for
	// their terminal FinishGCRun. The interval loop is waited on through its own
	// done channel in Boot; this is the same guarantee for the runs an operator
	// started by hand.
	async sync.WaitGroup

	// running guards REAL runs. The database's partial unique index is the real
	// authority; this exists so the API trigger can answer 409 without first writing
	// a row that is going to violate it.
	running atomic.Bool

	// measured remembers when each backend's usage was last written, so the usage
	// pass can skip a backend a full sweep just measured (spec §8, finding 7b). It is
	// in memory rather than read back from cache_backend_usage.measured_at because
	// the only process allowed to write that row is this one -- the same
	// single-writer property §6.2 leans on -- and a restart losing it costs one
	// redundant measurement.
	//
	// Mutex-guarded because the sweep loop and the usage loop are separate goroutines
	// that both write it.
	measuredMu sync.Mutex
	measured   map[int64]time.Time

	// lastUsage is the last measurement published for each backend. It exists because
	// the storage gauge vecs are RESET before a sweep republishes them (a vec is a
	// map, not a snapshot, so a deleted backend would otherwise export its last value
	// forever) and the reset is indiscriminate: without this, every backend the sweep
	// declines to scan would silently disappear from the scrape.
	lastUsage map[int64]snapshot

	// rampUntil is gc_state.touch_ramp_until as unix nanos, read ONCE at boot
	// (LoadTouchRamp) and read on every toucher tick by TouchStaleness. Zero means
	// "not read yet", which resolves to the RAMPED (conservative, fewer writes)
	// end -- see TouchStaleness.
	rampUntil atomic.Int64
}

// New validates deps and cfg and builds the engine. Zero-valued durations and
// batch sizes take the spec's shipped defaults rather than degenerating into a
// zero interval or an empty chunk.
//
// ctx is the SERVER LIFETIME context, not a boot context (R7#8): it is what
// TriggerAsync's detached sweeps run under, so a context that ends when Boot
// finishes wiring would cancel every API-triggered sweep the instant it started.
func New(ctx context.Context, deps Deps, cfg Config) (*Engine, error) {
	switch {
	case ctx == nil:
		return nil, errors.New("gc: a lifetime context is required")
	case deps.DB == nil:
		return nil, errors.New("gc: Deps.DB is required")
	case deps.Blobs == nil:
		return nil, errors.New("gc: Deps.Blobs is required")
	case deps.Metrics == nil:
		return nil, errors.New("gc: Deps.Metrics is required")
	}

	log := deps.Log
	if log == nil {
		log = slog.Default()
	}

	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}

	if cfg.UsageInterval <= 0 {
		cfg.UsageInterval = 6 * time.Hour
	}

	if cfg.GracePeriod < 0 {
		cfg.GracePeriod = 24 * time.Hour
	}

	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}

	if cfg.TouchInterval <= 0 {
		cfg.TouchInterval = time.Minute
	}

	if cfg.TouchStaleness <= 0 {
		cfg.TouchStaleness = time.Hour
	}

	if deps.Access == nil || deps.Unihash == nil {
		log.Warn("gc: a pre-sweep access flusher is not wired: the sweep will decide on " +
			"accessed_at values up to --gc-touch-staleness stale, and stage 1 has no veto " +
			"of its own to fall back on")
	}

	e := &Engine{
		db:         deps.DB,
		blobs:      deps.Blobs,
		access:     deps.Access,
		unihash:    deps.Unihash,
		rec:        deps.Metrics.GC(),
		log:        log,
		cfg:        cfg,
		lifetime:   ctx,
		async:      sync.WaitGroup{},
		running:    atomic.Bool{},
		measuredMu: sync.Mutex{},
		measured:   map[int64]time.Time{},
		lastUsage:  map[int64]snapshot{},
		rampUntil:  atomic.Int64{},
	}

	return e, nil
}

// interval renders a Go duration as the pgtype.Interval the sweep queries take.
// Microseconds only: a duration has no months or days, and expressing 90 days as
// Days: 90 would hand Postgres a calendar interval whose length depends on which
// DST boundaries it spans.
func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Days: 0, Months: 0, Valid: true}
}

// durationOf converts a retention_window read from the database into a Go
// duration for the LADDER's arithmetic (max, least, doubling).
//
// Months and Days are folded at 30 and 24h. That approximation is confined to the
// ladder: the value that reaches SQL is always an interval built by interval()
// from the result, so a window is compared against now() exactly once, in
// Postgres, with Postgres' own arithmetic.
func durationOf(v pgtype.Interval) (time.Duration, bool) {
	if !v.Valid {
		return 0, false
	}

	d := time.Duration(v.Microseconds) * time.Microsecond
	d += time.Duration(v.Days) * 24 * time.Hour
	d += time.Duration(v.Months) * 30 * 24 * time.Hour

	if d <= 0 {
		return 0, false
	}

	return d, true
}

// pause is the inter-chunk pacing, cancellable. A plain time.Sleep here would make
// a shutdown wait out the whole pause on every chunk boundary.
func (e *Engine) pause(ctx context.Context) error {
	if e.cfg.BatchPause <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(e.cfg.BatchPause)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// chunkCtx bounds one statement. See chunkTimeout.
func chunkCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, chunkTimeout)
}

// backendOf maps a schema backend kind onto the metrics label. They are the same
// closed set by construction (the enum and metrics.Backend mirror each other), so
// this is a widening rather than a lookup that can fail.
func backendOf(kind repository.BackendKind) metrics.Backend {
	return metrics.Backend(kind)
}

// errShutdown is the gc_runs.error a sweep that lost its context records. It is a
// distinct string from any query failure because "the operator restarted the
// server" and "a statement failed" are different incidents and the runs list is
// where an operator tells them apart.
const errShutdown = "shutdown"

// runError renders a sweep failure for gc_runs.error, collapsing a cancelled
// context to the shutdown marker.
//
// IT IS NEVER EMPTY. gc_runs carries `CHECK ((status = 'failed') = (error IS NOT
// NULL))`, so a failure whose error renders as "" would be written as NULL and the
// terminal UPDATE would abort -- leaving the run 'running' and holding the active
// slot, which is the exact state the finisher exists to prevent.
func runError(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errShutdown
	case err == nil || err.Error() == "":
		return "unknown error"
	default:
		return fmt.Sprintf("%.400s", err.Error())
	}
}
