package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

	// RetentionWindow and QuotaBytes OVERRIDE the seeded defaults (M6, spec §4/§7).
	// CreateBackend already computes an opinionated window from the org default and
	// the kind, so leaving these absent is the normal case; supplying one is an
	// operator overriding that seed at creation time.
	//
	// json.RawMessage, not a pointer, because these fields have THREE meanings and a
	// pointer can only carry two: absent (keep whatever was seeded), explicit null
	// (retain forever / no cap -- a real, reachable state), and a value. A *string
	// collapses the first two, which would make "retain forever" unexpressible.
	//
	// omitempty on the encoding side, same reasoning as UpdateBackendRequest's
	// identical fields immediately below.
	RetentionWindow json.RawMessage `json:"retention_window,omitempty"`
	QuotaBytes      json.RawMessage `json:"quota_bytes,omitempty"`
}

// UpdateBackendRequest patches a backend. Absent fields are left alone; kind is
// immutable (it is the mount point).
type UpdateBackendRequest struct {
	Enabled          *bool           `json:"enabled"`
	ReadAuthRequired *bool           `json:"read_auth_required"`
	Config           json.RawMessage `json:"config"`

	// RetentionWindow and QuotaBytes are the M6 knobs, with the same three-state
	// encoding CreateBackendRequest documents: absent keeps the current column,
	// explicit null clears it to "retain forever" / "no cap", a value sets it.
	//
	// The PATCH semantics live HERE, in the handler, and not in the query: 000012's
	// UpdateBackend sets both columns unconditionally (a plain nullable UPDATE),
	// because NULL is already a meaningful value for both and a query-level
	// "leave alone" would need a sentinel a nullable interval has no room for.
	//
	// omitempty on the ENCODING side only (decodeJSON never re-marshals this
	// struct; it reads the request body's raw bytes). Without it, a Go caller
	// that builds an UpdateBackendRequest{Enabled: &x} and leaves these two
	// nil would have json.Marshal emit an explicit `"retention_window":null`,
	// which is indistinguishable on the wire from a DELIBERATE clear -- see
	// UpdateOrgRequest's identical fields (orgs.go) for the caller
	// (internal/cli's RenameOrg) that hit exactly this trap.
	RetentionWindow json.RawMessage `json:"retention_window,omitempty"`
	QuotaBytes      json.RawMessage `json:"quota_bytes,omitempty"`
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

	// Parsed and VALIDATED before the insert: a malformed window must not leave a
	// backend behind. Applied after it, because CreateBackend computes the seeded
	// defaults in SQL and this is an override of whatever it chose.
	window, setWindow, err := backendRetentionPatch(req.RetentionWindow)
	if err != nil {
		return err
	}

	quota, setQuota, err := backendQuotaPatch(req.QuotaBytes, kind)
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
		// 409. The generic 23505 mapping in toAPIError says "that slug is already
		// taken", which is nonsense here -- there is no slug in this request -- so
		// name the real conflict before it reaches the generic mapping.
		if isPGCode(err, pgUniqueViolation) {
			return errConflict(CodeConflict,
				fmt.Sprintf("this project already has a %s backend", kind))
		}

		return fmt.Errorf("create %s backend: %w", kind, err)
	}

	// TWO STATEMENTS, and deliberately not one transaction. CreateBackend derives
	// the seed from the org row in SQL (so a new backend is never left outside the
	// opinionated defaults), which leaves no room in its parameter list for an
	// override; this patches the seed afterwards. The failure mode of the split is
	// benign and self-correcting: the backend exists with its SEEDED window, the
	// caller sees the error, and a PATCH sets what they asked for. The failure mode
	// of doing it the other way round -- no seed unless the client sends one --
	// is a backend silently outside retention forever.
	if setWindow || setQuota {
		backend, err = a.store.UpdateBackend(ctx, repository.UpdateBackendParams{
			ID:               backend.ID,
			Enabled:          backend.Enabled,
			ReadAuthRequired: backend.ReadAuthRequired,
			Config:           backend.Config,
			RetentionWindow:  pickInterval(setWindow, window, backend.RetentionWindow),
			QuotaBytes:       pickInt8(setQuota, quota, backend.QuotaBytes),
		})
		if err != nil {
			return fmt.Errorf("apply retention/quota to the new %s backend: %w", kind, err)
		}
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

	window, setWindow, err := backendRetentionPatch(req.RetentionWindow)
	if err != nil {
		return err
	}

	quota, setQuota, err := backendQuotaPatch(req.QuotaBytes, current.Kind)
	if err != nil {
		return err
	}

	// current.ID came from GetBackend(project_id, kind) -- i.e. from the scope the
	// guard authorized, never from the request. UpdateBackend takes a bare id and
	// would happily patch any backend in the installation if handed one.
	//
	// EVERY COLUMN IS PASSED, INCLUDING THE TWO THIS REQUEST DID NOT MENTION. The
	// query sets retention_window and quota_bytes unconditionally, so an omitted
	// field that resolved to a zero pgtype value would CLEAR the column -- a PATCH
	// of `{"enabled": false}` would silently turn a backend's retention off. Reading
	// the current row and passing it back is the same read-modify-write that gives
	// enabled/read_auth_required/config their PATCH semantics.
	backend, err := a.store.UpdateBackend(ctx, repository.UpdateBackendParams{
		ID:               current.ID,
		Enabled:          boolOr(req.Enabled, current.Enabled),
		ReadAuthRequired: boolOr(req.ReadAuthRequired, current.ReadAuthRequired),
		Config:           cfg,
		RetentionWindow:  pickInterval(setWindow, window, current.RetentionWindow),
		QuotaBytes:       pickInt8(setQuota, quota, current.QuotaBytes),
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
		// ON DELETE RESTRICT from cache_objects => a 23503 while the backend still
		// holds objects. The generic mapping says "that reference does not exist, or
		// the user is not a member of the organization", which is the exact opposite
		// of the truth (the reference very much exists), so name it here.
		if isPGCode(err, pgForeignKeyViolation) {
			return errConflict(CodeConflict,
				"this backend still holds cache objects and cannot be deleted until it is emptied")
		}

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
//
// It returns the FULL row (via ListBackendsForProject), not a struct hand-built
// from a partial one. An earlier version synthesised a repository.CacheBackend from
// GetBackend's projection, which omits created_at/updated_at -- so the detail
// endpoint serialised "0001-01-01T00:00:00Z" while the list endpoint (which selects
// the timestamps) returned the real ones. A project configures at most five
// backends, so scanning the project's list to find the kind is a bounded,
// single-query lookup, and it carries every column.
func (a *API) backendOf(r *http.Request) (repository.CacheBackend, error) {
	ctx := r.Context()
	s := scopeFrom(ctx)

	kind, err := backendKindOf(r.PathValue("kind"))
	if err != nil {
		return repository.CacheBackend{}, err
	}

	backends, err := a.store.ListBackendsForProject(ctx, s.ProjectID)
	if err != nil {
		return repository.CacheBackend{}, fmt.Errorf("load backend: %w", err)
	}

	for _, b := range backends {
		if b.Kind == kind {
			return b, nil
		}
	}

	return repository.CacheBackend{}, errNotFound(
		fmt.Sprintf("this project has no %s backend configured", kind))
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

// maxRetentionWindow bounds a retention_window. Ten years is not a policy, it is a
// typo guard: `retention_window` is an interval Postgres will happily store as
// 100000h, and a window longer than the installation will exist is
// indistinguishable from null except that it looks like a real setting.
const maxRetentionWindow = 10 * 365 * 24 * time.Hour

// backendRetentionPatch parses a backend's own three-state retention_window
// field. See retentionWindowPatch, which this and the org-level
// default_retention_window field (orgs.go) both use -- the encoding is
// identical, only the JSON field name in a 422 differs.
func backendRetentionPatch(raw json.RawMessage) (pgtype.Interval, bool, error) {
	return retentionWindowPatch(raw, "retention_window")
}

// retentionWindowPatch parses ANY three-state retention_window-shaped field:
// absent (not set), explicit null ("retain forever" -- a real state, spec §4,
// and the shipped state of every downloads backend), or a duration string.
//
// field names the JSON key in the 422 it might return, so ONE parser serves a
// backend's own retention_window and an org's default_retention_window
// (000012's default_retention_window column, B4) with the right field name in
// each.
//
// Returns (value, set): set=false means the field was ABSENT and the caller must
// keep the current column; set=true with an invalid pgtype.Interval means the
// explicit null.
func retentionWindowPatch(raw json.RawMessage, field string) (pgtype.Interval, bool, error) {
	if len(raw) == 0 {
		return pgtype.Interval{}, false, nil
	}

	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return pgtype.Interval{}, false,
			errValidation(field, field+` must be a duration string like "720h", or null`)
	}

	if s == nil {
		return pgtype.Interval{}, true, nil
	}

	d, err := time.ParseDuration(*s)
	if err != nil {
		return pgtype.Interval{}, false,
			errValidation(field, field+` must be a duration string like "720h", or null`)
	}

	// > 0 mirrors cache_backends_retention_window_positive. A zero or negative
	// window would mean "delete everything on the next sweep", which nobody types on
	// purpose and which the CHECK refuses anyway -- refusing it here makes it a 422
	// with a sentence instead of a 500 with a constraint name.
	if d <= 0 || d > maxRetentionWindow {
		return pgtype.Interval{}, false, errValidation(field,
			field+" must be positive and no more than 10 years, or null to retain forever")
	}

	return pgtype.Interval{
		Microseconds: d.Microseconds(), Days: 0, Months: 0, Valid: true,
	}, true, nil
}

// backendQuotaPatch parses the three-state quota_bytes field and enforces the two
// kinds that may not have one.
//
// hashserv is refused because it is STRUCTURALLY unenforceable: hashserv owns no
// cache_objects rows, the quota histogram runs over cache_objects, so the quota
// would read 0 forever -- a silent lie rather than an honest "no cap". 000012's
// cache_backends_hashserv_no_quota CHECK is the backstop; this is the 422 that
// explains it.
//
// oci is refused because it is a PRODUCT decision (spec §1.3): a pull-through proxy
// is bounded by its retention window. Unlike hashserv there is no CHECK -- an OCI
// quota is representable and the sweep enforces it correctly if a row ever carries
// one (that is why internal/gc still evicts OCI namespaces in stage order) -- so
// this validation is the whole of the rule, and relaxing it later requires no
// migration.
func backendQuotaPatch(
	raw json.RawMessage, kind repository.BackendKind,
) (pgtype.Int8, bool, error) {
	v, set, err := quotaBytesPatch(raw, "quota_bytes")
	if err != nil || !set || !v.Valid {
		// Not set, an error, or the explicit-null ("no cap") case: none of those needs
		// the kind-specific refusal below, which only ever fires on a concrete cap.
		return v, set, err
	}

	switch kind {
	case repository.BackendKindHashserv:
		return pgtype.Int8{}, false, errValidation("quota_bytes",
			"a hashserv backend cannot have a quota: it stores no cache objects, so the cap "+
				"would never be reached and would always read as unused")
	case repository.BackendKindOci:
		return pgtype.Int8{}, false, errValidation("quota_bytes",
			"an oci backend cannot have a quota: a pull-through proxy is bounded by its "+
				"retention window")
	default:
		return v, true, nil
	}
}

// quotaBytesPatch parses ANY three-state quota_bytes-shaped field: absent (not
// set), explicit null (no cap), or a positive integer. field names the JSON key
// in the 422 it might return, so this one parser serves a backend's own
// quota_bytes (with backendQuotaPatch's kind-specific refusal layered on top)
// and an org's unrestricted default_quota_bytes (000012, B4) alike.
func quotaBytesPatch(raw json.RawMessage, field string) (pgtype.Int8, bool, error) {
	if len(raw) == 0 {
		return pgtype.Int8{}, false, nil
	}

	var n *int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return pgtype.Int8{}, false,
			errValidation(field, field+" must be a positive integer, or null for no cap")
	}

	if n == nil {
		return pgtype.Int8{}, true, nil
	}

	if *n <= 0 {
		return pgtype.Int8{}, false,
			errValidation(field, field+" must be a positive integer, or null for no cap")
	}

	return pgtype.Int8{Int64: *n, Valid: true}, true, nil
}

// pickInterval resolves a patched-or-current interval. See handleUpdateBackend for
// why "current" is passed rather than a zero value.
func pickInterval(set bool, patched, current pgtype.Interval) pgtype.Interval {
	if set {
		return patched
	}

	return current
}

// pickInt8 is pickInterval for quota_bytes.
func pickInt8(set bool, patched, current pgtype.Int8) pgtype.Int8 {
	if set {
		return patched
	}

	return current
}

// boolOr resolves an optional bool against a default.
func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}

	return *v
}
