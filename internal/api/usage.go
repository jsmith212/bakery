package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/jsmith212/bakery/internal/db/repository"
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

// handleGetProjectUsage is B2b. ProjectRead.
func (a *API) handleGetProjectUsage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.GetProjectBackendUsage(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("get project usage: %w", err)
	}

	out := make([]ProjectBackendUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, newProjectBackendUsage(row))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}
