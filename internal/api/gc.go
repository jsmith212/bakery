package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	Trigger string `json:"trigger"` // interval|api
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
		if errors.Is(err, gc.ErrAlreadyRunning) {
			return errConflict(CodeConflict, "a gc sweep is already running")
		}

		return fmt.Errorf("trigger gc run: %w", err)
	}

	a.log.InfoContext(ctx, "gc run triggered via api",
		slog.Int64("run", id), slog.Bool("dry_run", req.DryRun))

	writeJSON(w, http.StatusAccepted,
		TriggerGCRunResponse{ID: id, Status: string(repository.GcRunStatusRunning)})

	return nil
}
