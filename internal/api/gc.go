package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/gc"
)

// ---------------------------------------------------------------------------
// GC (spec §9.10, docs/design/specs/2026-08-14-m6-gc-retention-quotas.md). The
// operator surface over the M6 sweep engine: history, and a trigger.
//
// AccessSiteAdmin, all three routes -- the same level the site-admins routes sit
// on, for the same reason. A sweep can delete a tenant's cache wholesale (retention
// SHIPS ON, opinionated), so triggering one is at least as dangerous as granting a
// site role, and the guard's rule holds identically: an API-key principal never
// reaches AccessSiteAdmin, so a delegated key can never start or stop a sweep.
// ---------------------------------------------------------------------------

// gcTrigger is the slice of *gc.Engine the control plane needs: start a sweep and
// learn its run id WITHOUT waiting for the sweep to finish. It is an interface, as
// Store is, so the 202/409 paths are testable without a database or a real sweep --
// not because a second implementation of the engine is expected.
type gcTrigger interface {
	TriggerAsync(ctx context.Context, trigger gc.Trigger, dryRun bool) (int64, error)
}

// *gc.Engine must keep satisfying gcTrigger. If TriggerAsync's signature ever
// drifts, this fails to compile rather than at the first POST /gc/run.
var _ gcTrigger = (*gc.Engine)(nil)

// GCRun is one row of the sweep history: GET /gc/runs and GET /gc/runs/{id}.
//
// Every field mirrors gc_runs (000007, 000012) directly. There is no derived
// summary beyond what the sweep itself wrote: this row and gc.Engine.Summary come
// from the SAME terminal FinishGCRun.
type GCRun struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"`  // running|succeeded|failed
	Trigger string `json:"trigger"` // interval|api|usage
	DryRun  bool   `json:"dry_run"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Error      string     `json:"error,omitempty"`

	ObjectsDeleted int64 `json:"objects_deleted"`
	// HashservRowsDeleted is broken out from ObjectsDeleted, mirroring the schema
	// (000012): hashserv is the GC root (spec §3 stage 1), so a run's summary can
	// answer "did the root move" separately from "how much sstate came with it".
	HashservRowsDeleted int64 `json:"hashserv_rows_deleted"`
	BlobsMarked         int64 `json:"blobs_marked"`
	BlobsDeleted        int64 `json:"blobs_deleted"`
	BytesReclaimed      int64 `json:"bytes_reclaimed"`
}

// GCRunList is the paginated envelope for GET /gc/runs.
//
// A dedicated envelope rather than ListResponse[GCRun]: gc_runs carries no
// retention or per-scope limit of its own (spec §11 lists both as non-goals) and
// accumulates for the installation's whole life, so unlike every other listing in
// this API -- orgs, projects, keys, members, all of which fit one response -- this
// one needs a cursor from day one. NextCursor is the id to pass as ?before= for
// the next (older) page; nil means this was the last page.
type GCRunList struct {
	Items      []GCRun `json:"items"`
	NextCursor *int64  `json:"next_cursor"`
}

// TriggerGCRunRequest is POST /api/v1/gc/run's body.
type TriggerGCRunRequest struct {
	// DryRun requests a genuinely read-only sweep: it writes an auditable gc_runs
	// row (dry_run = true) but deletes nothing anywhere (spec §9.7). Its zero
	// value, false, is a REAL sweep -- there is no safer default to fall back to
	// here, because omitting the field is exactly what `bakery gc run` with no
	// flags means.
	DryRun bool `json:"dry_run"`
}

// TriggerGCRunResponse is 202's body: just enough to poll GET /gc/runs/{id}.
type TriggerGCRunResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func newGCRunFromList(r repository.ListGCRunsRow) GCRun {
	return GCRun{
		ID: r.ID, Status: string(r.Status), Trigger: r.Trigger, DryRun: r.DryRun,
		StartedAt: r.StartedAt.Time, FinishedAt: timePtr(r.FinishedAt), Error: r.Error.String,
		ObjectsDeleted: r.ObjectsDeleted, HashservRowsDeleted: r.HashservRowsDeleted,
		BlobsMarked: r.BlobsMarked, BlobsDeleted: r.BlobsDeleted, BytesReclaimed: r.BytesReclaimed,
	}
}

func newGCRunFromGet(r repository.GetGCRunRow) GCRun {
	return GCRun{
		ID: r.ID, Status: string(r.Status), Trigger: r.Trigger, DryRun: r.DryRun,
		StartedAt: r.StartedAt.Time, FinishedAt: timePtr(r.FinishedAt), Error: r.Error.String,
		ObjectsDeleted: r.ObjectsDeleted, HashservRowsDeleted: r.HashservRowsDeleted,
		BlobsMarked: r.BlobsMarked, BlobsDeleted: r.BlobsDeleted, BytesReclaimed: r.BytesReclaimed,
	}
}

// gcRunDefaultLimit and gcRunMaxLimit bound GET /gc/runs' page size. The default
// is what the console's runs screen wants above the fold; the ceiling exists
// because ?limit= is caller-controlled and an unbounded one is a way to force a
// large scan of a table with no per-scope limit of its own (spec §11).
const (
	gcRunDefaultLimit = 20
	gcRunMaxLimit     = 100
)

// gcRunStatusFilter parses ?status=. Empty means "every status", which is how a
// first request and an explicit reset both read.
func gcRunStatusFilter(s string) (repository.NullGcRunStatus, error) {
	if s == "" {
		return repository.NullGcRunStatus{}, nil
	}

	switch repository.GcRunStatus(s) {
	case repository.GcRunStatusRunning, repository.GcRunStatusSucceeded, repository.GcRunStatusFailed:
		return repository.NullGcRunStatus{GcRunStatus: repository.GcRunStatus(s), Valid: true}, nil
	default:
		return repository.NullGcRunStatus{},
			errValidation("status", `status must be "running", "succeeded" or "failed"`)
	}
}

// gcRunCursor parses ?before=, the keyset page cursor (a gc_runs.id). Empty means
// "start from the newest run".
func gcRunCursor(s string) (pgtype.Int8, error) {
	if s == "" {
		return pgtype.Int8{}, nil
	}

	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return pgtype.Int8{}, errValidation("before", "before must be a positive integer run id")
	}

	return pgtype.Int8{Int64: id, Valid: true}, nil
}

// gcRunLimit parses ?limit=, clamped to gcRunMaxLimit rather than refused: a
// caller asking for too much gets the ceiling, not an error, because the ceiling
// is an implementation bound, not a validation rule the caller needs to learn.
func gcRunLimit(s string) (int32, error) {
	if s == "" {
		return gcRunDefaultLimit, nil
	}

	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, errValidation("limit", "limit must be a positive integer")
	}

	if n > gcRunMaxLimit {
		n = gcRunMaxLimit
	}

	return int32(n), nil
}

// gcIncludeUsage decides whether the lightweight usage-measurement runs appear in
// the listing (R7#12).
//
// THE DEFAULT IS TO HIDE THEM. MeasureUsage mints a gc_runs row on every
// --gc-usage-interval tick -- every six hours, forever, whether or not retention is
// even enabled -- purely so the measurement is auditable. They delete nothing and
// are indistinguishable at a glance from a sweep that found nothing to do, so a
// default-visible listing would bury real sweeps under bookkeeping within a week.
//
// Two spellings, one meaning: ?include_usage=true, and ?trigger=usage for a caller
// who thinks in terms of the column. There is deliberately NO general ?trigger=
// filter -- 'usage' is the only value anyone needs to select on, and the other two
// (interval, api) are already visible and already in every row -- so a
// ?trigger=api would be a filter that silently did nothing. Anything else is
// ignored rather than refused, for the same reason ?limit= clamps instead of
// erroring: it is a display preference, not a validation rule the caller must
// learn.
func gcIncludeUsage(q url.Values) bool {
	return q.Get("include_usage") == "true" || q.Get("trigger") == string(gc.TriggerUsage)
}

// parseGCRunID parses the {id} path segment: a gc_runs.id, a plain bigint, never a
// uuid -- unlike every other path identifier in this API.
func parseGCRunID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errBadRequest("that is not a valid gc run id", err)
	}

	return id, nil
}

// handleListGCRuns lists recent GC runs, most recent first. Site admin only.
//
// KEYSET-paginated on id (ListGCRuns, query/gc.sql): gc_runs.id only ever grows,
// so a cursor of the last id on THIS page resumes exactly where it left off even
// if a new run starts between two requests -- an OFFSET page would silently skip
// or repeat a row, which for an endpoint whose purpose is watching runs start is
// not a rare case.
func (a *API) handleListGCRuns(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	status, err := gcRunStatusFilter(r.URL.Query().Get("status"))
	if err != nil {
		return err
	}

	before, err := gcRunCursor(r.URL.Query().Get("before"))
	if err != nil {
		return err
	}

	limit, err := gcRunLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return err
	}

	rows, err := a.store.ListGCRuns(ctx, repository.ListGCRunsParams{
		Status: status, BeforeID: before, PageLimit: limit,
		IncludeUsage: gcIncludeUsage(r.URL.Query()),
	})
	if err != nil {
		return fmt.Errorf("list gc runs: %w", err)
	}

	out := make([]GCRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, newGCRunFromList(row))
	}

	// A FULL page (exactly `limit` rows) means there may be more; a short page is
	// proof there is not. This is the same "stop as soon as a page comes back
	// short" signal the sweep's own scan uses (internal/gc, ScanObjectsForGC).
	var next *int64

	if int32(len(rows)) == limit && len(rows) > 0 {
		id := rows[len(rows)-1].ID
		next = &id
	}

	writeJSON(w, http.StatusOK, GCRunList{Items: out, NextCursor: next})

	return nil
}

// handleGetGCRun fetches one run by id. Site admin only.
func (a *API) handleGetGCRun(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	id, err := parseGCRunID(r.PathValue("id"))
	if err != nil {
		return err
	}

	row, err := a.store.GetGCRun(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("gc run not found")
		}

		return fmt.Errorf("get gc run: %w", err)
	}

	writeJSON(w, http.StatusOK, newGCRunFromGet(row))

	return nil
}

// handleTriggerGCRun starts a sweep and answers as soon as it has a run id --
// NEVER once the sweep finishes (spec §9.10). A sweep can legitimately run for
// hours (the shipped six-hour interval; a ten-million-row backend paces at ~17
// minutes of pause alone at the default batch settings), so this is 202 with an
// id to poll, never 200 with a result.
//
// gc.Engine.TriggerAsync detaches the sweep from THIS request's context itself
// (context.WithoutCancel, internal to the engine): a client that stops polling, or
// a proxy that times out the connection, must not cancel a sweep already under
// way. A second real trigger while one is in flight is ErrAlreadyRunning, which
// renders 409 -- the same CompareAndSwap the interval loop's own Run uses, so the
// API can never observe a 202 that turns out to have started nothing.
func (a *API) handleTriggerGCRun(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	if a.gc == nil {
		// Only reachable when Config.GC was never wired -- a protocol-test harness
		// that assembles a partial API, never server.Boot, which always constructs a
		// real engine. Refuse explicitly rather than a nil-pointer 500.
		return errInternal("gc is not configured on this server", nil)
	}

	var req TriggerGCRunRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	id, err := a.gc.TriggerAsync(ctx, gc.TriggerAPI, req.DryRun)
	if err != nil {
		// THREE DISTINCT 409s, and the distinction is the whole value of the response
		// (R7#1). "Already running" is a wait-and-retry; "gc is disabled" is a flag the
		// operator can change and probably meant to; "multi-instance" is a refusal that
		// will not go away until the deployment does, and telling those apart is the
		// difference between an operator retrying for an hour and one reading the docs.
		switch {
		case errors.Is(err, gc.ErrAlreadyRunning):
			return errConflict(CodeConflict, "a gc sweep is already running")
		case errors.Is(err, gc.ErrDisabled):
			return errConflict(CodeConflict,
				"gc is disabled on this server (--gc-enabled=false): nothing is swept, and a "+
					"triggered sweep would silently override a deliberate operational state")
		case errors.Is(err, gc.ErrMultiInstance):
			return errConflict(CodeConflict,
				"gc cannot run under --allow-multi-instance: the sweep's LRU invalidation, its "+
					"boot reaper and its pending-read veto are all process-local, and each of "+
					"them silently deletes data another instance is still serving")
		}

		return fmt.Errorf("trigger gc run: %w", err)
	}

	a.log.InfoContext(ctx, "gc run triggered via api",
		slog.Int64("run", id), slog.Bool("dry_run", req.DryRun))

	writeJSON(w, http.StatusAccepted,
		TriggerGCRunResponse{ID: id, Status: string(repository.GcRunStatusRunning)})

	return nil
}

// ---------------------------------------------------------------------------
// GC org visibility (B7, spec docs/design/specs/2026-08-15-spa-api-wiring.md,
// product decision 2): per-backend sweep attribution, surfaced to an ORG VIEWER
// rather than only a site admin -- "swept, nothing eligible" (a gc_run_backends
// row at 0/0) is distinguishable from "not swept" (no row), which is the whole
// point of migration 000013 and internal/gc/sweep.go's publishRunBackend. The
// full /gc operations screen (runs above, the trigger) stays SITE-ADMIN-ONLY;
// this is deliberately narrower -- an org's own history, nothing instance-wide.
// ---------------------------------------------------------------------------

// GCActivityBackend is one backend's row within one run, scoped to the CALLER's
// org (a run can span many orgs; only this org's slice is ever returned).
type GCActivityBackend struct {
	ProjectSlug string `json:"project_slug"`
	// Kind is sstate|downloads|hashserv|bazel|oci.
	Kind           string `json:"kind"`
	ObjectsDeleted int64  `json:"objects_deleted"`
	// BytesFreed is LOGICAL bytes -- see migration 000013's own comment and
	// sweep.go's Summary.LogicalBytesFreed: full charge to this backend at
	// delete time, not a physical Layer-B reclaim figure, and undercounted by
	// design for OCI manifest deletions specifically (stage 8 deletes without
	// reading size_bytes back).
	BytesFreed int64 `json:"bytes_freed"`
}

// GCActivityRun is one run's org-scoped summary: the run's own identity plus
// every backend row of THIS org's projects it touched.
type GCActivityRun struct {
	RunID      int64      `json:"run_id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Status     string     `json:"status"` // running|succeeded|failed

	Backends []GCActivityBackend `json:"backends"`
}

// GCActivityList is the paginated envelope for GET /orgs/{org}/gc/activity.
// NextCursor is the oldest run id on this page to pass as ?before= for the
// next (older) page; nil means this was the last page -- gc_runs' own
// convention (handleListGCRuns), reused rather than reinvented.
type GCActivityList struct {
	Items      []GCActivityRun `json:"items"`
	NextCursor *int64          `json:"next_cursor"`
}

// groupGCActivity folds ListOrgGCActivity's flat (run, backend) rows into one
// entry PER RUN. It relies on the query's own ORDER BY (gr.id DESC, p.slug,
// cb.kind) to keep runs newest-first and each run's backend rows contiguous and
// stably ordered, so this is a single linear pass -- no second sort, no map.
func groupGCActivity(rows []repository.ListOrgGCActivityRow) []GCActivityRun {
	items := make([]GCActivityRun, 0, len(rows))

	for _, row := range rows {
		b := GCActivityBackend{
			ProjectSlug: row.ProjectSlug, Kind: string(row.Kind),
			ObjectsDeleted: row.ObjectsDeleted, BytesFreed: row.BytesFreed,
		}

		if n := len(items); n > 0 && items[n-1].RunID == row.RunID {
			items[n-1].Backends = append(items[n-1].Backends, b)

			continue
		}

		items = append(items, GCActivityRun{
			RunID: row.RunID, StartedAt: row.StartedAt.Time, FinishedAt: timePtr(row.FinishedAt),
			Status: string(row.Status), Backends: []GCActivityBackend{b},
		})
	}

	return items
}

// handleGetOrgGCActivity is B7. OrgView -- an org VIEWER, not an admin: this is
// read-only history about the caller's own tenant, the same floor as GET
// /orgs/{org}/projects. limit bounds the number of RUNS returned (not the
// number of flat backend rows the query joins), clamped by gcRunLimit exactly
// like GET /gc/runs -- never rejected, per gc.go:155-173's convention.
func (a *API) handleGetOrgGCActivity(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	before, err := gcRunCursor(r.URL.Query().Get("before"))
	if err != nil {
		return err
	}

	limit, err := gcRunLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return err
	}

	rows, err := a.store.ListOrgGCActivity(ctx, repository.ListOrgGCActivityParams{
		OrgID: s.OrgID, BeforeID: before, RunLimit: limit,
	})
	if err != nil {
		return fmt.Errorf("list org gc activity: %w", err)
	}

	items := groupGCActivity(rows)

	// A FULL page of RUNS means there may be more; a short one proves there is
	// not -- items, not rows, because the clamp bounds the number of distinct
	// runs, and a single wide run's backend rows must never be mistaken for
	// "more runs to page through".
	var next *int64

	if int32(len(items)) == limit && len(items) > 0 {
		id := items[len(items)-1].RunID
		next = &id
	}

	writeJSON(w, http.StatusOK, GCActivityList{Items: items, NextCursor: next})

	return nil
}
