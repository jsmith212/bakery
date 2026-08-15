package api

import (
	"crypto/sha256"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// Shared helpers for the B2/B3/B4/B6/B7 DB-backed tests (SPA->API wiring wave,
// spec 2026-08-15). They write straight through h.store.Pool() / the embedded
// *repository.Queries -- these are facts only the GC engine or a real cache
// write path produces in production, and this stage tests the READ side.

// backendID reads back a configured backend's row id by (org, project, kind).
func (h *harness) backendID(orgSlug, projectSlug, kind string) int64 {
	h.t.Helper()

	var id int64

	err := h.store.Pool().QueryRow(h.t.Context(), `
		SELECT cb.id
		  FROM cache_backends cb
		  JOIN projects p ON p.id = cb.project_id
		  JOIN organizations o ON o.id = p.org_id
		 WHERE o.slug = $1 AND p.slug = $2 AND cb.kind = $3::backend_kind`,
		orgSlug, projectSlug, kind,
	).Scan(&id)
	if err != nil {
		h.t.Fatalf("backendID(%s/%s/%s): %v", orgSlug, projectSlug, kind, err)
	}

	return id
}

// setUsage writes a cache_backend_usage row directly -- the one write path this
// stage's endpoints (B2) are the FIRST readers of, and which only gc.Engine's
// UpsertBackendUsage produces in production.
func (h *harness) setUsage(backendID, objects, bytes int64) {
	h.t.Helper()

	if _, err := h.store.Pool().Exec(h.t.Context(), `
		INSERT INTO cache_backend_usage (backend_id, objects_count, logical_bytes, measured_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (backend_id) DO UPDATE
		   SET objects_count = EXCLUDED.objects_count,
		       logical_bytes = EXCLUDED.logical_bytes,
		       measured_at   = EXCLUDED.measured_at`,
		backendID, objects, bytes,
	); err != nil {
		h.t.Fatalf("setUsage(%d): %v", backendID, err)
	}
}

// putObject writes a cache_objects row (and the blobs row its FK requires)
// directly -- B3's object browser is a read endpoint; no write path through the
// HTTP API exists in this milestone to populate it with.
func (h *harness) putObject(backendID int64, namespace, key string, content []byte) {
	h.t.Helper()

	sum := sha256.Sum256(content)

	if _, err := h.store.Pool().Exec(h.t.Context(),
		`INSERT INTO blobs (digest, size_bytes) VALUES ($1, $2) ON CONFLICT (digest) DO NOTHING`,
		sum[:], len(content),
	); err != nil {
		h.t.Fatalf("insert blob for %q: %v", key, err)
	}

	if _, err := h.store.Pool().Exec(h.t.Context(),
		`INSERT INTO cache_objects (backend_id, namespace, key, digest, size_bytes)
		 VALUES ($1, $2, $3, $4, $5)`,
		backendID, namespace, key, sum[:], len(content),
	); err != nil {
		h.t.Fatalf("insert cache_objects row for %q: %v", key, err)
	}
}

// startGCRun and recordGCRunBackend drive B7's data directly through the
// embedded *repository.Queries (h.store satisfies it by embedding) -- exactly
// what internal/gc/sweep.go's own StartGCRun/RecordGCRunBackend calls do, and
// the only way gc_run_backends is ever populated outside a real sweep.
func (h *harness) startGCRun() repository.StartGCRunRow {
	h.t.Helper()

	run, err := h.store.StartGCRun(h.t.Context(), repository.StartGCRunParams{
		GracePeriod: pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true},
		Trigger:     "interval", DryRun: false,
	})
	if err != nil {
		h.t.Fatalf("StartGCRun: %v", err)
	}

	return run
}

func (h *harness) recordGCRunBackend(runID, backendID, objectsDeleted, bytesFreed int64) {
	h.t.Helper()

	if err := h.store.RecordGCRunBackend(h.t.Context(), repository.RecordGCRunBackendParams{
		RunID: runID, BackendID: backendID,
		ObjectsDeleted: objectsDeleted, BytesFreed: bytesFreed,
	}); err != nil {
		h.t.Fatalf("RecordGCRunBackend: %v", err)
	}
}
