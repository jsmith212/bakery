package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// CreateBackendRequest configures a cache backend on a project.
//
// M1 ships NO backend implementation -- these are config rows and nothing serves
// traffic from them until M2. They exist now because blob.Service keys object
// metadata on cache_backends.id, so the row has to exist before an object can.
type CreateBackendRequest struct {
	// Kind is sstate|downloads|hashserv|bazel|oci. UNIQUE (project_id, kind) is the
	// routing grammar itself -- /cache/{org}/{project}/sstate/... names exactly one
	// mount -- so kind identifies the backend and there is no separate name.
	Kind string `json:"kind"`

	// Enabled defaults to true when absent, which is why it is a pointer: a plain
	// bool cannot tell "the client said false" from "the client said nothing", and
	// silently disabling a backend someone just created is a bad way to find out.
	Enabled *bool `json:"enabled"`

	// ReadAuthRequired defaults to true. There is deliberately no
	// WriteAuthRequired: writes ALWAYS require a key, and "unauthenticated writes"
	// -- a cache-poisoning vector -- is not a state the schema can represent.
	ReadAuthRequired *bool `json:"read_auth_required"`

	Config json.RawMessage `json:"config"`
}

// UpdateBackendRequest patches a backend. Absent fields are left alone; kind is
// immutable (it is the mount point).
type UpdateBackendRequest struct {
	Enabled          *bool           `json:"enabled"`
	ReadAuthRequired *bool           `json:"read_auth_required"`
	Config           json.RawMessage `json:"config"`
}

// handleListBackends lists a project's configured backends.
func (a *API) handleListBackends(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.ListBackendsForProject(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("list backends: %w", err)
	}

	out := make([]Backend, 0, len(rows))
	for _, b := range rows {
		out = append(out, newBackend(b))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handleCreateBackend configures a backend. Project admin.
func (a *API) handleCreateBackend(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	var req CreateBackendRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	kind, err := backendKindOf(req.Kind)
	if err != nil {
		return err
	}

	cfg, err := backendConfig(req.Config)
	if err != nil {
		return err
	}

	backend, err := a.store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        s.ProjectID,
		Kind:             kind,
		Enabled:          boolOr(req.Enabled, true),
		ReadAuthRequired: boolOr(req.ReadAuthRequired, true),
		Config:           cfg,
	})
	if err != nil {
		// UNIQUE (project_id, kind) => a second sstate mount on one project is a
		// 409, not a 500. toAPIError maps 23505 for us.
		return fmt.Errorf("create %s backend: %w", kind, err)
	}

	writeJSON(w, http.StatusCreated, newBackend(backend))

	return nil
}

// handleGetBackend reads one backend by kind.
func (a *API) handleGetBackend(w http.ResponseWriter, r *http.Request) error {
	backend, err := a.backendOf(r)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, newBackend(backend))

	return nil
}

// handleUpdateBackend patches a backend. Project admin.
func (a *API) handleUpdateBackend(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	current, err := a.backendOf(r)
	if err != nil {
		return err
	}

	var req UpdateBackendRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	cfg := current.Config

	if req.Config != nil {
		cfg, err = backendConfig(req.Config)
		if err != nil {
			return err
		}
	}

	// current.ID came from GetBackend(project_id, kind) -- i.e. from the scope the
	// guard authorized, never from the request. UpdateBackend takes a bare id and
	// would happily patch any backend in the installation if handed one.
	backend, err := a.store.UpdateBackend(ctx, repository.UpdateBackendParams{
		ID:               current.ID,
		Enabled:          boolOr(req.Enabled, current.Enabled),
		ReadAuthRequired: boolOr(req.ReadAuthRequired, current.ReadAuthRequired),
		Config:           cfg,
	})
	if err != nil {
		return fmt.Errorf("update backend: %w", err)
	}

	writeJSON(w, http.StatusOK, newBackend(backend))

	return nil
}

// handleDeleteBackend removes a backend config. Project admin.
//
// This deletes the CONFIG ROW. The cache objects hanging off it go with it, by
// cascade; the BYTES are left for the GC to reap, which is the storage-ordering
// invariant working as designed -- metadata first, bytes second. Orphaned bytes
// are recoverable; a dangling metadata row is a permanent 500.
func (a *API) handleDeleteBackend(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	current, err := a.backendOf(r)
	if err != nil {
		return err
	}

	n, err := a.store.DeleteBackend(ctx, current.ID)
	if err != nil {
		return fmt.Errorf("delete backend: %w", err)
	}

	if n == 0 {
		return errNotFound("backend not found")
	}

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}

// backendOf resolves {kind} within the AUTHORIZED project.
//
// The lookup is by (project_id, kind), never by a caller-supplied id. That is what
// makes the {kind} path segment safe: the worst a caller can do with it is name a
// kind, and the project it is looked up in is the one the guard already checked
// them against.
func (a *API) backendOf(r *http.Request) (repository.CacheBackend, error) {
	ctx := r.Context()
	s := scopeFrom(ctx)

	kind, err := backendKindOf(r.PathValue("kind"))
	if err != nil {
		return repository.CacheBackend{}, err
	}

	row, err := a.store.GetBackend(ctx, repository.GetBackendParams{
		ProjectID: s.ProjectID, Kind: kind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repository.CacheBackend{}, errNotFound(
				fmt.Sprintf("this project has no %s backend configured", kind))
		}

		return repository.CacheBackend{}, fmt.Errorf("load backend: %w", err)
	}

	return repository.CacheBackend{
		ID: row.ID, ProjectID: s.ProjectID, Kind: kind,
		Enabled: row.Enabled, ReadAuthRequired: row.ReadAuthRequired, Config: row.Config,
	}, nil
}

// backendConfig validates the jsonb payload.
//
// It must be a JSON OBJECT. `null`, `3` and `"sstate"` are all valid JSON and all
// valid jsonb, and every one of them would be a config row that a future backend's
// unmarshal chokes on at request time rather than at configuration time.
func backendConfig(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errValidation("config", "config must be a JSON object")
	}

	if obj == nil {
		return []byte(`{}`), nil
	}

	return raw, nil
}

// boolOr resolves an optional bool against a default.
func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}

	return *v
}
