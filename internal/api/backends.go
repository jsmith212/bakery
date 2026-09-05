package api

import (
	"context"
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

	// A backend under teardown (000018) is not patchable, and the refusal is not
	// cosmetic: UpdateBackend sets `enabled` unconditionally, so a PATCH of
	// {"enabled": true} against a marked row would re-enable a backend whose objects
	// the GC is in the middle of deleting -- serving hits that turn into misses row
	// by row. deleting_at is deliberately NOT in that query's SET list, so the mark
	// itself survives; this is what stops the rest of the row from drifting under it.
	if current.DeletingAt.Valid {
		return errConflict(CodeConflict,
			"this backend is being torn down and can no longer be changed")
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

// handleDeleteBackend tears a backend down. Project admin.
//
// TWO OUTCOMES, AND THE SLOW ONE IS THE NORMAL ONE. An EMPTY backend is deleted
// outright, 204, exactly as before -- that is the "configured it by mistake" case and
// it should not park a row in `deleting` for up to a GC interval. A backend that has
// ever served a build is MARKED (000018's deleting_at) and answered 202 with its own
// JSON, because the deletion is a bulk row removal the GC owns.
//
// WHY THE ROW IS NOT DRAINED HERE. cache_objects -> cache_backends is ON DELETE
// RESTRICT, so before 000018 this endpoint answered 409 forever on any backend that
// held objects: the backend could never be deleted at all. The obvious fix -- delete
// the objects in this request -- is the one this codebase already refuses everywhere
// else. A five-thousand-object backend times out the request; a ten-million-object
// one holds a single transaction across the whole delete, pinning a snapshot on the
// hottest table in the schema. And the delete would have to bypass
// blob.Service.DeleteBatch (the only sanctioned path: digest-ordered blob locks, the
// write barrier re-derived at delete time, shard-grouped LRU invalidation) or
// reimplement it. The mark costs one UPDATE and hands the work to the machine that
// already does exactly this, at exactly this pace, under exactly these rules.
//
// The moment the mark lands the backend is UNCONFIGURED: GetBackend (the route
// resolver's own probe) filters on deleting_at IS NULL, so every cache mount 404s
// like a kind that was never created, and the snippet generator refuses it. Only the
// control-plane list/get still return the row, so the console can say what is
// happening.
//
// IDEMPOTENT. A second DELETE on a marked backend is another 202 carrying the same
// deleting_at the first one minted (MarkBackendDeleting coalesces), never a 404 and
// never a restarted clock -- a console that retries a request whose response it lost
// must not be told the backend is gone while the GC is still emptying it.
func (a *API) handleDeleteBackend(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	current, err := a.backendOf(r)
	if err != nil {
		return err
	}

	if current.DeletingAt.Valid {
		writeJSON(w, http.StatusAccepted, newBackend(current))

		return nil
	}

	done, err := a.deleteEmptyBackend(ctx, current.ID)
	if err != nil {
		return err
	}

	if done {
		writeJSON(w, http.StatusNoContent, nil)

		return nil
	}

	marked, err := a.store.MarkBackendDeleting(ctx, current.ID)
	if err != nil {
		return fmt.Errorf("mark the %s backend for teardown: %w", current.Kind, err)
	}

	writeJSON(w, http.StatusAccepted, newBackend(marked))

	return nil
}

// deleteEmptyBackend deletes a backend that holds nothing, and reports whether it
// did. false means "fall back to the mark" -- never an error.
//
// The probe and the delete are two statements, deliberately not one transaction: the
// gap between them is a build writing its first object, and the ONLY consequence of
// losing that race is a 23503 that lands the caller on the teardown path -- which is
// the same answer the probe would have produced had it seen the row. Wrapping them
// would buy a stricter answer to a question whose two answers are already both
// correct, at the cost of holding a transaction open across a control-plane request.
//
// hashserv is why the probe asks three tables and not one: it owns no cache_objects
// rows at all, and its RESTRICT comes from hashserv_unihashes / hashserv_outhashes
// (000010). A probe on cache_objects alone would declare every hashserv backend empty
// and walk it straight into the foreign-key violation this function exists to avoid.
func (a *API) deleteEmptyBackend(ctx context.Context, id int64) (bool, error) {
	has, err := a.store.BackendHasObjects(ctx, id)
	if err != nil {
		return false, fmt.Errorf("check whether the backend still holds objects: %w", err)
	}

	if has {
		return false, nil
	}

	n, err := a.store.DeleteBackend(ctx, id)
	if err != nil {
		// The lost race described above. Anything else is a real failure.
		if isPGCode(err, pgForeignKeyViolation) {
			return false, nil
		}

		return false, fmt.Errorf("delete backend: %w", err)
	}

	// n == 0 means the row vanished between backendOf and here -- a concurrent
	// delete. Reporting "deleted" is the truthful answer to DELETE: it is gone.
	return n >= 0, nil
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
//
// registry -- the OTHER OCI-shaped kind -- is DELIBERATELY NOT in this switch (spec
// buildkit-cache-export §5): it has no upstream to fall back to, `mode=max` exports
// run multi-GB by design, and a byte quota is the operator's only ceiling. Same
// storage shape as oci, opposite product decision.
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
