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

// RETENTION AND QUOTA ARE PATCHABLE, AND AN UNRELATED PATCH MUST NOT CLEAR THEM
// (D: R6#3, and the caller regression F1 flagged when UpdateBackend grew the two
// columns).
//
// 000012's UpdateBackend is a PLAIN nullable UPDATE: it sets retention_window and
// quota_bytes on every call, because NULL is a legitimate value for both ("retain
// forever", "no cap") and a query-level "leave alone" would need a sentinel a
// nullable interval has no room for. The three-state PATCH semantics therefore live
// in the handler, and the failure they prevent is silent: `PATCH {"enabled":false}`
// turning a backend's retention off for good.
func TestUpdateBackendPatchesRetentionAndQuota(t *testing.T) {
	admin := principals(t)["proj_admin"]

	tests := []struct {
		name string
		body string

		wantStatus    int
		wantRetention *string
		wantQuota     *int64
	}{
		{
			name: "an unrelated patch keeps both columns", body: `{"enabled":false}`,
			wantStatus: http.StatusOK, wantRetention: ptr("2160h0m0s"), wantQuota: ptr(int64(1024)),
		},
		{
			name: "a duration string sets the window", body: `{"retention_window":"720h"}`,
			wantStatus: http.StatusOK, wantRetention: ptr("720h0m0s"), wantQuota: ptr(int64(1024)),
		},
		{
			name: "explicit null clears the window to retain-forever",
			body: `{"retention_window":null}`,
			// nil retention, quota untouched: null is a REAL state, distinct from absent.
			wantStatus: http.StatusOK, wantRetention: nil, wantQuota: ptr(int64(1024)),
		},
		{
			name: "explicit null clears the quota", body: `{"quota_bytes":null}`,
			wantStatus: http.StatusOK, wantRetention: ptr("2160h0m0s"), wantQuota: nil,
		},
		{
			name: "a non-duration window is a 422", body: `{"retention_window":"90 days"}`,
			wantStatus: 422, wantRetention: nil, wantQuota: nil,
		},
		{
			name: "a zero window is a 422", body: `{"retention_window":"0s"}`,
			wantStatus: 422, wantRetention: nil, wantQuota: nil,
		},
		{
			name: "a non-positive quota is a 422", body: `{"quota_bytes":0}`,
			wantStatus: 422, wantRetention: nil, wantQuota: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := backendFixture(t, time.Now(), time.Now())
			store.backends[0].RetentionWindow = pgtype.Interval{
				Microseconds: (90 * 24 * time.Hour).Microseconds(), Days: 0, Months: 0, Valid: true,
			}
			store.backends[0].QuotaBytes = pgtype.Int8{Int64: 1024, Valid: true}

			a := testAPI(t, store, nil)

			w := do(t, a, admin, http.MethodPatch,
				Prefix+"/orgs/acme/projects/firmware/backends/sstate", tc.body)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			if tc.wantStatus != http.StatusOK {
				return
			}

			var got Backend
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if !equalStringPtr(got.RetentionWindow, tc.wantRetention) {
				t.Errorf("retention_window = %v, want %v", deref(got.RetentionWindow), deref(tc.wantRetention))
			}

			if !equalInt64Ptr(got.QuotaBytes, tc.wantQuota) {
				t.Errorf("quota_bytes = %v, want %v", got.QuotaBytes, tc.wantQuota)
			}
		})
	}
}

// A QUOTA IS REFUSED ON hashserv AND oci, WITH A SENTENCE (D: R6#3).
//
// hashserv is structural: it owns no cache_objects rows, so the quota histogram has
// nothing to run over and the cap would read 0 forever -- a silent lie, and
// 000012's cache_backends_hashserv_no_quota CHECK refuses it anyway (this is the
// 422 that explains the refusal instead of a 500 naming a constraint). oci is the
// product decision (spec §1.3): a pull-through proxy is bounded by its retention
// window.
func TestQuotaIsRefusedOnHashservAndOCI(t *testing.T) {
	admin := principals(t)["proj_admin"]

	for _, kind := range []string{"hashserv", "oci"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			a := testAPI(t, store, nil)

			w := do(t, a, admin, http.MethodPost, Prefix+"/orgs/acme/projects/firmware/backends",
				`{"kind":"`+kind+`","quota_bytes":1024}`)

			if w.Code != 422 {
				t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
			}

			// Refused BEFORE the insert: a rejected payload must not leave a backend behind.
			if len(store.backends) != 0 {
				t.Errorf("a refused create left %d backends behind", len(store.backends))
			}
		})
	}
}

// CREATE ACCEPTS AN OVERRIDE AND KEEPS THE SEED OTHERWISE (D: R6#3).
//
// CreateBackend computes the opinionated window in SQL (spec §1.1/§4), so a create
// that specifies nothing must come back seeded -- 30 days for bazel -- and a create
// that specifies a window must come back with that one instead.
func TestCreateBackendRetentionOverrideAndSeed(t *testing.T) {
	admin := principals(t)["proj_admin"]

	t.Run("no override keeps the seeded window", func(t *testing.T) {
		store := fixtureStore(t)
		a := testAPI(t, store, nil)

		w := do(t, a, admin, http.MethodPost,
			Prefix+"/orgs/acme/projects/firmware/backends", `{"kind":"bazel"}`)

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
		}

		var got Backend
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if got.RetentionWindow == nil || *got.RetentionWindow != "720h0m0s" {
			t.Errorf("retention_window = %v, want the seeded 30 days", deref(got.RetentionWindow))
		}
	})

	t.Run("an override wins", func(t *testing.T) {
		store := fixtureStore(t)
		a := testAPI(t, store, nil)

		w := do(t, a, admin, http.MethodPost, Prefix+"/orgs/acme/projects/firmware/backends",
			`{"kind":"bazel","retention_window":"48h","quota_bytes":4096}`)

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
		}

		var got Backend
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if got.RetentionWindow == nil || *got.RetentionWindow != "48h0m0s" {
			t.Errorf("retention_window = %v, want 48h0m0s", deref(got.RetentionWindow))
		}

		if got.QuotaBytes == nil || *got.QuotaBytes != 4096 {
			t.Errorf("quota_bytes = %v, want 4096", got.QuotaBytes)
		}
	})
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}

	return *s
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}
