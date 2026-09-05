package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/gc"
)

// ---------------------------------------------------------------------------
// Usage (B2, spec docs/design/specs/2026-08-15-spa-api-wiring.md). The FIRST
// readers of cache_backend_usage (000012): it has had exactly one writer,
// gc.Engine's own UpsertBackendUsage, since it landed, and no query anywhere
// read it back until these two.
//
// Both LEFT JOIN it, deliberately: a backend cache_backend_usage has no row for
// yet -- newly created, or hashserv, which structurally never gets one (M6's
// plan.go gives it no stages, so MeasureUsage and the sweep both skip it) -- and
// a backend genuinely idle at zero bytes are DIFFERENT facts, and a client must
// be able to tell them apart. measured_at is therefore ALWAYS on the wire, and
// it is nil (never a live "0 B") exactly when nothing has reported yet.
// ---------------------------------------------------------------------------

// OrgProjectUsage is one row of GET /orgs/{org}/usage (B2a): one project's
// LOGICAL storage, summed across every backend it has configured.
type OrgProjectUsage struct {
	ProjectSlug  string `json:"project_slug"`
	ObjectsCount int64  `json:"objects_count"`
	LogicalBytes int64  `json:"logical_bytes"`

	// MeasuredAt is nil when NOT ONE backend of this project has ever reported
	// usage (query/usage.sql: MIN() over an all-NULL group is NULL). When
	// present, it is the OLDEST contributing measurement -- the conservative
	// choice for a SUM: the total is only as fresh as its stalest part.
	MeasuredAt *time.Time `json:"measured_at"`
}

func newOrgProjectUsage(r repository.GetOrgUsageByProjectRow) OrgProjectUsage {
	return OrgProjectUsage{
		ProjectSlug: r.ProjectSlug, ObjectsCount: r.ObjectsCount, LogicalBytes: r.LogicalBytes,
		MeasuredAt: timePtr(r.MeasuredAt),
	}
}

// handleGetOrgUsage is B2a. OrgView -- the same floor as GET /orgs/{org}/projects,
// which this is meant to sit beside on the org projects screen.
//
// IT DOES NOT RE-MEASURE, deliberately. The freshness rule below is per PROJECT, and
// an org's projects screen is one request that would fan out into one measurement per
// project -- an aggregate over every cache_objects row the org owns, on a page load,
// unbounded in the number of projects. That is the wrong trade at exactly the scale
// where it starts to matter. The org grid stays on the periodic backstop (now one
// hour rather than six), and the moment a human opens a project the per-project rule
// takes over. Every figure here carries measured_at, so the staleness is stated
// rather than hidden.
func (a *API) handleGetOrgUsage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.GetOrgUsageByProject(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("get org usage: %w", err)
	}

	out := make([]OrgProjectUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, newOrgProjectUsage(row))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// ProjectBackendUsage is one row of GET .../{project}/usage (B2b): one backend's
// (kind's) OWN measurement, unaggregated, with its quota/retention alongside so
// the console can render "212 GB / 500 GB cap" without a second round trip.
type ProjectBackendUsage struct {
	// Kind is sstate|downloads|hashserv|bazel|oci.
	Kind string `json:"kind"`

	// ObjectsCount/LogicalBytes are nil exactly when this backend has no
	// cache_backend_usage row yet -- see the package doc above. Never rendered
	// as a live zero.
	ObjectsCount *int64     `json:"objects_count"`
	LogicalBytes *int64     `json:"logical_bytes"`
	MeasuredAt   *time.Time `json:"measured_at"`

	// QuotaBytes / RetentionWindow are the backend's OWN configured columns
	// (Backend carries the same two; repeated here so the usage response is
	// self-contained for a caller that only fetched this endpoint).
	QuotaBytes      *int64  `json:"quota_bytes"`
	RetentionWindow *string `json:"retention_window"`
}

func newProjectBackendUsage(r repository.GetProjectBackendUsageRow) ProjectBackendUsage {
	return ProjectBackendUsage{
		Kind:            string(r.Kind),
		ObjectsCount:    int64Ptr(r.ObjectsCount),
		LogicalBytes:    int64Ptr(r.LogicalBytes),
		MeasuredAt:      timePtr(r.MeasuredAt),
		QuotaBytes:      int64Ptr(r.QuotaBytes),
		RetentionWindow: durationString(r.RetentionWindow),
	}
}

// usageMeasurer is the slice of *gc.Engine this package needs to refresh one
// project's usage. An interface for the same reason gcTrigger is: the freshness
// decision is ordinary handler logic and must be testable without an engine, a
// database or a sweep.
type usageMeasurer interface {
	MeasureProject(ctx context.Context, projectID pgtype.UUID) error
}

// *gc.Engine must keep satisfying usageMeasurer.
var _ usageMeasurer = (*gc.Engine)(nil)

// handleGetProjectUsage is B2b, now with READ-TRIGGERED MEASUREMENT.
//
// WHY A READ MEASURES AT ALL. These figures had exactly one writer -- the periodic
// pass on --gc-usage-interval, defaulting to six hours -- so to anyone using the
// console they never moved: create a backend, push a gigabyte, and the dashboard says
// "not yet measured" for the rest of the working day. The rejected alternative, a
// trigger-maintained live counter, would put a row-lock convoy on the hottest write
// path in the product for the sake of a dashboard figure (see internal/gc/measure.go).
//
// SO THE STALENESS RULE IS THE WHOLE FEATURE, and it is three guards deep: nothing
// happens unless --usage-freshness is non-zero AND the newest measurement on file is
// older than it; the engine collapses concurrent measurements of one project into a
// single query; and the measurement itself is bounded, past which the STALE row is
// served unchanged. That last one is not a fallback, it is the contract: measured_at
// rides on every figure, so an old answer is an honest answer and a hung dashboard
// is not.
//
// Nil-tolerant: an embedder or a test with no engine wired serves what is on file,
// which is exactly the behaviour this endpoint had before.
func (a *API) handleGetProjectUsage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.GetProjectBackendUsage(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("get project usage: %w", err)
	}

	if a.staleEnough(rows, a.usageFreshness) {
		rows, err = a.remeasure(ctx, s.ProjectID, rows)
		if err != nil {
			return err
		}
	}

	writeJSON(w, http.StatusOK, list(newProjectBackendUsageList(rows)))

	return nil
}

// handleMeasureProjectUsage is the EXPLICIT refresh: POST .../usage/measure.
//
// ProjectRead, the same floor as the GET it refreshes -- it writes only
// cache_backend_usage, which is a derived figure about data the caller can already
// see, and putting it behind ProjectAdmin would mean a reader staring at a number
// they cannot make correct.
//
// It always answers 200 with the current rows, whether or not it measured. The rate
// limit is a server-side floor on how often the work happens, not a condition the
// client has to handle: a 429 here would give a dashboard nothing to do except show
// the same rows it would have got anyway, with an error attached.
//
// THE RATE LIMIT IS THE ENGINE'S, and it used to be this handler's. The handler asked
// staleEnough(rows, 10s) -- a question about measured_at, which is written only when a
// measurement SUCCEEDS. A project whose aggregate exceeds the engine's ten-second
// ceiling therefore passed that gate on every single click, forever, and re-ran the
// most expensive query in the installation each time. gc.MinMeasureInterval keys off
// the last ATTEMPT instead and covers both entry points, so there is one floor rather
// than two that can disagree; refusing there is a silent no-op, and this endpoint
// answers with the rows on file exactly as it always did.
func (a *API) handleMeasureProjectUsage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.GetProjectBackendUsage(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("get project usage: %w", err)
	}

	if a.measurer != nil {
		rows, err = a.remeasure(ctx, s.ProjectID, rows)
		if err != nil {
			return err
		}
	}

	writeJSON(w, http.StatusOK, list(newProjectBackendUsageList(rows)))

	return nil
}

// staleEnough reports whether a measurement is worth making.
//
// A NEVER-MEASURED project is stale (that is the case the whole feature exists for:
// a backend created a moment ago, whose figures a human is looking at right now).
// Otherwise the NEWEST measurement decides -- not the oldest, which is what the org
// endpoint's own SUM uses. The two questions differ: an aggregate is only as fresh as
// its stalest part, but "should I measure this project again" is answered by the last
// time anything measured it, and using the minimum would re-measure the whole project
// forever on account of one backend that will never report (there is none today --
// hashserv is excluded from the measurement AND from this list's join in practice --
// but the rule should not depend on that).
//
// A zero window disables the check entirely, which is what --usage-freshness=0 means.
func (a *API) staleEnough(rows []repository.GetProjectBackendUsageRow, window time.Duration) bool {
	if a.measurer == nil || window <= 0 {
		return false
	}

	newest := time.Time{}

	for _, row := range rows {
		if row.MeasuredAt.Valid && row.MeasuredAt.Time.After(newest) {
			newest = row.MeasuredAt.Time
		}
	}

	return newest.IsZero() || time.Since(newest) > window
}

// remeasure measures and re-reads, and NEVER fails the request because of the
// measurement.
//
// A timeout, a cancelled request or a database hiccup during the refresh leaves the
// caller with the rows already in hand -- stale, and honestly labelled as such by
// measured_at. The alternative, 500ing a read because a derived figure could not be
// recomputed, would take the whole screen down over the freshest of its numbers.
func (a *API) remeasure(
	ctx context.Context, projectID pgtype.UUID, current []repository.GetProjectBackendUsageRow,
) ([]repository.GetProjectBackendUsageRow, error) {
	if err := a.measurer.MeasureProject(ctx, projectID); err != nil {
		a.log.WarnContext(ctx, "could not refresh project usage; serving the last measurement",
			slog.Any("error", err))

		return current, nil
	}

	rows, err := a.store.GetProjectBackendUsage(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("re-read project usage: %w", err)
	}

	return rows, nil
}

// newProjectBackendUsageList maps a row set for the wire.
func newProjectBackendUsageList(rows []repository.GetProjectBackendUsageRow) []ProjectBackendUsage {
	out := make([]ProjectBackendUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, newProjectBackendUsage(row))
	}

	return out
}
