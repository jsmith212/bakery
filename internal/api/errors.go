package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/slug"
)

// Error codes. These are a CLOSED, stable vocabulary: the SPA and the CLI branch
// on `error.code`, never on the prose in `error.message`, so the message may be
// reworded at any time and the code may not.
const (
	CodeBadRequest     = "bad_request"
	CodeValidation     = "validation_failed"
	CodeReservedSlug   = "reserved_slug"
	CodeInvalidSlug    = "invalid_slug"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeClaimDerived   = "claim_derived_role"
	CodeUnsupported    = "unsupported_media_type"
	CodeScopeExceeded  = "scope_exceeds_role"
	CodeInternal       = "internal_error"
	CodeNotImplemented = "not_implemented"
)

// ErrorBody is the error envelope. Every non-2xx response from /api/v1 has
// exactly this shape -- there is no endpoint that invents its own.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the envelope's payload.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Field names the offending request field, when there is exactly one.
	Field string `json:"field,omitempty"`
}

// apiError is the internal error type. Handlers return it (or wrap a cause in
// it) and the guard renders it; a handler never writes a status code by hand,
// which is what keeps the envelope and the code/status pairing consistent.
type apiError struct {
	status  int
	code    string
	message string
	field   string
	cause   error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("api: %s: %s: %v", e.code, e.message, e.cause)
	}

	return fmt.Sprintf("api: %s: %s", e.code, e.message)
}

// Unwrap exposes the cause so errors.Is/As reach through the envelope.
func (e *apiError) Unwrap() error { return e.cause }

func errBadRequest(msg string, cause error) *apiError {
	return &apiError{status: http.StatusBadRequest, code: CodeBadRequest, message: msg, field: "", cause: cause}
}

func errValidation(field, msg string) *apiError {
	return &apiError{
		status: http.StatusUnprocessableEntity, code: CodeValidation,
		message: msg, field: field, cause: nil,
	}
}

func errNotFound(msg string) *apiError {
	return &apiError{status: http.StatusNotFound, code: CodeNotFound, message: msg, field: "", cause: nil}
}

func errForbidden(msg string) *apiError {
	return &apiError{status: http.StatusForbidden, code: CodeForbidden, message: msg, field: "", cause: nil}
}

func errUnauthorized(msg string) *apiError {
	return &apiError{status: http.StatusUnauthorized, code: CodeUnauthorized, message: msg, field: "", cause: nil}
}

func errConflict(code, msg string) *apiError {
	return &apiError{status: http.StatusConflict, code: code, message: msg, field: "", cause: nil}
}

func errInternal(msg string, cause error) *apiError {
	return &apiError{
		status: http.StatusInternalServerError, code: CodeInternal,
		message: msg, field: "", cause: cause,
	}
}

// errSlug turns a slug.Check failure into the right 4xx. The database CHECK is
// the authority (bakery_slug_ok); this only buys a friendly message instead of a
// raw 23514, so it must never be the ONLY place a slug is validated.
func errSlug(field string, err error) *apiError {
	switch {
	case errors.Is(err, slug.ErrReserved):
		return &apiError{
			status: http.StatusUnprocessableEntity, code: CodeReservedSlug, field: field,
			message: fmt.Sprintf("%q is reserved by the cache URL grammar and cannot be used as a slug", field),
			cause:   err,
		}
	case errors.Is(err, slug.ErrInvalid):
		return &apiError{
			status: http.StatusUnprocessableEntity, code: CodeInvalidSlug, field: field,
			message: "a slug must be 1-63 lowercase alphanumerics or hyphens, " +
				"and may not start or end with a hyphen",
			cause: err,
		}
	default:
		return errValidation(field, err.Error())
	}
}

// toAPIError maps any error a handler produced onto the envelope.
//
// The mapping is CENTRAL on purpose. A handler that leaks pgx.ErrNoRows as a 500,
// or a unique-violation as a 500, is the classic way an API becomes both noisy
// and information-leaking; doing it once here means a new endpoint gets the
// correct 404/409 for free.
func toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("not found")
	}

	// Auth's sentinels reach here when a handler calls into auth.Service.
	switch {
	case errors.Is(err, auth.ErrScopeExceedsRole):
		return &apiError{
			status: http.StatusForbidden, code: CodeScopeExceeded, field: "scope",
			message: "the requested scope exceeds your role in this project", cause: err,
		}
	case errors.Is(err, auth.ErrKeyInvalid):
		return errForbidden("an API key may not be used to manage API keys")
	case errors.Is(err, auth.ErrUnauthenticated):
		return errUnauthorized("authentication required")
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case pgUniqueViolation:
			return errConflict(CodeConflict, "that slug is already taken")
		case pgCheckViolation:
			// bakery_slug_ok is the only CHECK a control-plane write can trip.
			return &apiError{
				status: http.StatusUnprocessableEntity, code: CodeInvalidSlug, field: "slug",
				message: "the database refused that slug", cause: err,
			}
		case pgForeignKeyViolation:
			return errConflict(CodeConflict,
				"that reference does not exist, or the user is not a member of the organization")
		}
	}

	return errInternal("internal error", err)
}

// Postgres SQLSTATE codes we translate. Named, because "23505" in a switch is
// unreadable and one transposed digit turns a 409 into a 500.
const (
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
	pgForeignKeyViolation = "23503"
)

// writeError renders an error onto the wire and logs it.
//
// The Error/Warn split is by class: a 5xx is OUR bug and gets Error with the
// wrapped cause; a 4xx is the caller's and gets Debug, because otherwise a
// scanner walking the API fills the log with noise at Warn.
func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae := toAPIError(err)

	if ae.status >= http.StatusInternalServerError {
		a.log.ErrorContext(r.Context(), "request failed",
			slog.String("pattern", r.Pattern),
			slog.Int("status", ae.status),
			slog.String("code", ae.code),
			slog.Any("error", err),
		)
	} else {
		a.log.DebugContext(r.Context(), "request refused",
			slog.String("pattern", r.Pattern),
			slog.Int("status", ae.status),
			slog.String("code", ae.code),
			slog.Any("error", err),
		)
	}

	// A 5xx message never carries the cause: it is for us, in the log, not for
	// the caller, on the wire.
	body := ErrorBody{Error: ErrorDetail{Code: ae.code, Message: ae.message, Field: ae.field}}

	if ae.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bakery"`)
	}

	writeJSON(w, ae.status, body)
}

// writeJSON writes a JSON body.
//
// Headers, then WriteHeader, then body. A header set after WriteHeader is a
// silent no-op, and encoding before WriteHeader flushes an implicit 200 -- both
// mistakes are in kbi's handlers and both are invisible until they are not.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The control plane is private data; no shared cache may ever hold it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if v == nil {
		return
	}

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already on the wire; there is nothing to do but say so.
		slog.Error("encode response body", slog.Any("error", err))
	}
}

// decodeJSON reads a request body into v.
//
// DisallowUnknownFields is deliberate: a client that sends `{"role": "admin"}` to
// an endpoint whose field is `project_role` should be told, not silently given
// the zero value. Silent field-drop on an AUTHORIZATION payload is how a
// downgrade becomes a no-op that everyone believes worked.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return errBadRequest("the request body is not valid JSON for this endpoint", err)
	}

	return nil
}

// maxBodyBytes caps a control-plane request body. Every payload here is a handful
// of short strings; a megabyte is already absurd.
const maxBodyBytes = 1 << 20
