package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// backendFixture puts a single, fully-populated sstate backend on `firmware`,
// including real created_at/updated_at timestamps.
func backendFixture(t *testing.T, created, updated time.Time) *fakeStore {
	t.Helper()

	store := fixtureStore(t)
	firmware := mustUUID(t, projFirmwareID)

	store.backends = []repository.CacheBackend{
		{
			ID: 1, ProjectID: firmware, Kind: repository.BackendKindSstate,
			Enabled: true, ReadAuthRequired: true, Config: []byte(`{"foo":"bar"}`),
			CreatedAt: pgtype.Timestamptz{Time: created, Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
		},
	}

	return store
}

// TestGetBackendCarriesRealTimestamps pins finding: GET one backend must return the
// same created_at/updated_at as the list endpoint, not the zero time.
//
// backendOf used to hand-build a repository.CacheBackend from GetBackend's
// projection, which selects only {id, enabled, read_auth_required, config} -- so the
// timestamps stayed zero and the detail endpoint serialised
// "0001-01-01T00:00:00Z" while the list endpoint (which selects the timestamps)
// returned the real ones. A console rendering "created" on the detail page printed
// the year 1.
func TestGetBackendCarriesRealTimestamps(t *testing.T) {
	created := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 11, 9, 12, 0, 0, time.UTC)

	store := backendFixture(t, created, updated)
	a := testAPI(t, store, nil)

	reader := principals(t)["proj_read"]

	// The detail endpoint.
	one := do(t, a, reader, http.MethodGet,
		Prefix+"/orgs/acme/projects/firmware/backends/sstate", "")
	if one.Code != http.StatusOK {
		t.Fatalf("GET one: status = %d, want 200 (body %s)", one.Code, one.Body.String())
	}

	var got Backend
	if err := json.Unmarshal(one.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode detail: %v", err)
	}

	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("detail endpoint returned a zero timestamp: created=%s updated=%s",
			got.CreatedAt, got.UpdatedAt)
	}

	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %s, want %s", got.CreatedAt, created)
	}

	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %s, want %s", got.UpdatedAt, updated)
	}

	// And it agrees with the list endpoint, which is the whole point: the two views
	// of the same row must not disagree about when it was created.
	all := do(t, a, reader, http.MethodGet,
		Prefix+"/orgs/acme/projects/firmware/backends", "")
	if all.Code != http.StatusOK {
		t.Fatalf("GET list: status = %d, want 200", all.Code)
	}

	var body ListResponse[Backend]
	if err := json.Unmarshal(all.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	if len(body.Items) != 1 {
		t.Fatalf("list returned %d backends, want 1", len(body.Items))
	}

	if !body.Items[0].CreatedAt.Equal(got.CreatedAt) || !body.Items[0].UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("list vs detail disagree: list=%s/%s detail=%s/%s",
			body.Items[0].CreatedAt, body.Items[0].UpdatedAt, got.CreatedAt, got.UpdatedAt)
	}
}

// TestBackendConflictsGetBackendSpecificMessages pins finding: a cache-backend
// constraint violation must not inherit the generic slug/membership wording.
//
//   - A second backend of the same kind trips UNIQUE (project_id, kind) -> 23505.
//     The generic mapping says "that slug is already taken" -- but there is no slug
//     in a create-backend request.
//   - Deleting a backend that still holds objects trips ON DELETE RESTRICT -> 23503.
//     The generic mapping says "that reference does not exist ..." -- the exact
//     opposite of the truth (the reference very much exists).
func TestBackendConflictsGetBackendSpecificMessages(t *testing.T) {
	admin := principals(t)["proj_admin"]

	t.Run("duplicate kind names the backend, not a slug", func(t *testing.T) {
		store := fixtureStore(t)
		store.desiredErr = &pgconn.PgError{
			Code: pgUniqueViolation, ConstraintName: "cache_backends_project_id_kind_key",
		}
		a := testAPI(t, store, nil)

		w := do(t, a, admin, http.MethodPost,
			Prefix+"/orgs/acme/projects/firmware/backends", `{"kind":"sstate"}`)

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", w.Code, w.Body.String())
		}

		detail := decodeErr(t, w)
		if detail.Code != CodeConflict {
			t.Errorf("code = %q, want %q", detail.Code, CodeConflict)
		}

		if detail.Message != "this project already has a sstate backend" {
			t.Errorf("message = %q, want the backend-specific conflict", detail.Message)
		}

		if got := detail.Message; got == "that slug is already taken" {
			t.Error("the generic slug wording leaked onto a backend conflict")
		}
	})

	t.Run("delete-while-nonempty names the objects, not a missing reference", func(t *testing.T) {
		store := backendFixture(t, time.Now(), time.Now())
		store.desiredErr = &pgconn.PgError{
			Code: pgForeignKeyViolation, ConstraintName: "cache_objects_backend_id_fkey",
		}
		a := testAPI(t, store, nil)

		w := do(t, a, admin, http.MethodDelete,
			Prefix+"/orgs/acme/projects/firmware/backends/sstate", "")

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", w.Code, w.Body.String())
		}

		detail := decodeErr(t, w)
		if detail.Code != CodeConflict {
			t.Errorf("code = %q, want %q", detail.Code, CodeConflict)
		}

		if detail.Message != "this backend still holds cache objects and cannot be deleted until it is emptied" {
			t.Errorf("message = %q, want the still-holds-objects conflict", detail.Message)
		}
	})
}
