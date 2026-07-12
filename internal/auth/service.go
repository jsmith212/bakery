package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Session keys. Values are strings so the default gob codec stays safe.
const (
	sessionStateKey    = "oidc_state"
	sessionNonceKey    = "oidc_nonce"
	sessionVerifierKey = "oidc_verifier"
)

// ErrUnauthenticated means the request carried no usable credential.
var ErrUnauthenticated = errors.New("auth: the request carried no credential")

// Deps is what the server hands the auth service.
//
// Note what is NOT here: no *repository.Queries. The service takes the Store, so
// the transaction boundary for login reconciliation stays inside this package.
type Deps struct {
	Store    *db.Store
	Sessions *scs.SessionManager
	Provider *Provider        // nil when no OIDC issuer is configured
	Groups   *config.GroupMap // nil when no mapping file is configured
	Metrics  *metrics.Metrics // may be nil
	Log      *slog.Logger

	// DevLogin mirrors DEV_LOGIN_ENABLED. It arrives from the Kong flag / env var
	// and NOWHERE else. There is no setter, no API route and no database column
	// that can turn it on; see devlogin.go.
	DevLogin bool
}

// Service is the auth layer: verification, sessions, reconciliation, keys.
type Service struct {
	store    *db.Store
	sessions *scs.SessionManager
	provider *Provider
	groups   *config.GroupMap
	metrics  *metrics.Metrics
	log      *slog.Logger
	devLogin bool

	keys    keyStore
	toucher *keyToucher
}

// New builds the auth service.
func New(d Deps) (*Service, error) {
	if d.Store == nil {
		return nil, errors.New("auth: Deps.Store is required")
	}

	if d.Sessions == nil {
		return nil, errors.New("auth: Deps.Sessions is required")
	}

	log := d.Log
	if log == nil {
		log = slog.Default()
	}

	keys := pgKeyStore{pool: d.Store.Pool()}

	return &Service{
		store:    d.Store,
		sessions: d.Sessions,
		provider: d.Provider,
		groups:   d.Groups,
		metrics:  d.Metrics,
		log:      log,
		devLogin: d.DevLogin,
		keys:     keys,
		toucher:  newKeyToucher(keys),
	}, nil
}

// Sessions exposes the scs manager. Prefer LoadAndSave below for mounting.
func (s *Service) Sessions() *scs.SessionManager { return s.sessions }

// sessionLoadedKey marks a context that has been through LoadAndSave.
//
// This exists because scs's accessors PANIC on a context it has not loaded:
// SessionManager.GetString calls getSessionDataFromContext, which panics with
// "scs: no session data in context". Authenticate is deliberately usable from the
// cache path -- /cache/*, hashserv and gRPC, which MUST stay outside LoadAndSave
// (it does a database Find per cookie-bearing request, adds `Vary: Cookie`, and
// its ResponseWriter wrapper drops io.ReaderFrom, killing the sendfile fast path
// on blob responses). So Authenticate has to be able to ask "is there a session
// here?" without detonating when the answer is "this request never had one".
//
// scs offers no safe way to ask -- Token, Status and Exists all panic identically
// -- so we record the fact ourselves, on the way in.
type sessionLoadedKey struct{}

// LoadAndSave is how the session middleware must be mounted. Mount it on the
// /api/v1 subtree ONLY, never on the root mux, and never over the cache routes.
//
// A subtree wrapped in the raw scs.SessionManager.LoadAndSave instead of this one
// still works, but its requests will not be recognised as session-bearing and will
// authenticate as anonymous. That is a fail-CLOSED failure (a 401, not a bypass),
// which is the right direction for a mistake to fall.
func (s *Service) LoadAndSave(next http.Handler) http.Handler {
	return s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), sessionLoadedKey{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	}))
}

// sessionLoaded reports whether scs's data is present on this context, and is
// therefore safe to read.
func sessionLoaded(ctx context.Context) bool {
	loaded, ok := ctx.Value(sessionLoadedKey{}).(bool)

	return ok && loaded
}

// AuthConfig renders /auth/config: what the SPA and the CLI need to configure
// themselves without redoing OIDC discovery. The API agent serves it; this
// produces it.
func (s *Service) AuthConfig() AuthConfig {
	cfg := AuthConfig{
		Issuer: "", ClientID: "", Scopes: nil,
		AuthorizationEndpoint: "", TokenEndpoint: "", DeviceAuthorizationEndpoint: "",
		OIDCEnabled: false, DevLoginEnabled: s.devLogin,
	}

	if s.provider != nil {
		cfg = s.provider.AuthConfig()
		cfg.DevLoginEnabled = s.devLogin
	}

	return cfg
}

// HandleAuthConfig serves the /auth/config document.
func (s *Service) HandleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.AuthConfig())
}

// HandleLogin begins the browser authorization-code flow.
//
// state, nonce and the PKCE verifier are minted here and stashed in the session,
// which is the only place they can safely live: they must survive the redirect to
// the IdP and come back bound to THIS browser.
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		http.NotFound(w, r)

		return
	}

	req, err := s.provider.AuthCodeURL()
	if err != nil {
		s.log.ErrorContext(r.Context(), "build authorization URL", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "authentication is unavailable")

		return
	}

	ctx := r.Context()

	// Renew before stashing, so a pre-existing (possibly attacker-planted) session
	// token cannot be the one that ends up carrying the completed login.
	if err := s.sessions.RenewToken(ctx); err != nil {
		s.log.ErrorContext(ctx, "renew session token", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "authentication is unavailable")

		return
	}

	s.sessions.Put(ctx, sessionStateKey, req.State)
	s.sessions.Put(ctx, sessionNonceKey, req.Nonce)
	s.sessions.Put(ctx, sessionVerifierKey, req.Verifier)

	http.Redirect(w, r, req.URL, http.StatusFound)
}

// HandleCallback completes the browser flow.
func (s *Service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		http.NotFound(w, r)

		return
	}

	ctx := r.Context()

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.observe(MethodSession, "denied")
		writeAuthError(w, http.StatusForbidden, codeForbidden, "the identity provider refused the login")

		return
	}

	wantState := s.sessions.GetString(ctx, sessionStateKey)
	gotState := r.URL.Query().Get("state")

	// Constant-time: state is a secret we generated, and this compares it against
	// attacker-supplied bytes.
	if wantState == "" || subtle.ConstantTimeCompare([]byte(wantState), []byte(gotState)) != 1 {
		s.observe(MethodSession, "error")
		s.log.WarnContext(ctx, "oidc callback state mismatch")
		writeAuthError(w, http.StatusBadRequest, codeBadRequest, ErrStateMismatch.Error())

		return
	}

	nonce := s.sessions.GetString(ctx, sessionNonceKey)
	verifier := s.sessions.GetString(ctx, sessionVerifierKey)

	// One-shot. Whatever happens next, these three must never be reusable.
	s.sessions.Remove(ctx, sessionStateKey)
	s.sessions.Remove(ctx, sessionNonceKey)
	s.sessions.Remove(ctx, sessionVerifierKey)

	code := r.URL.Query().Get("code")
	if code == "" {
		s.observe(MethodSession, "error")
		writeAuthError(w, http.StatusBadRequest, codeBadRequest, "the callback carried no authorization code")

		return
	}

	id, err := s.provider.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		s.observe(MethodSession, "error")
		s.log.WarnContext(ctx, "oidc code exchange failed", slog.Any("error", err))
		writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "authentication failed")

		return
	}

	userID, err := s.Reconcile(ctx, id)
	if err != nil {
		if errors.Is(err, ErrLoginNotAllowed) {
			s.observe(MethodSession, "denied")
			s.log.WarnContext(ctx, "login refused",
				slog.String("subject", id.Subject), slog.Any("error", err))
			writeAuthError(w, http.StatusForbidden, codeForbidden, ErrLoginNotAllowed.Error())

			return
		}

		s.observe(MethodSession, "error")
		s.log.ErrorContext(ctx, "login reconciliation failed", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "authentication failed")

		return
	}

	if err := s.establish(ctx, userID); err != nil {
		s.observe(MethodSession, "error")
		s.log.ErrorContext(ctx, "establish session", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "authentication failed")

		return
	}

	s.observe(MethodSession, "ok")
	http.Redirect(w, r, "/", http.StatusFound)
}

// establish renews the session token and records the user. RenewToken closes
// session fixation: the token the browser held before it authenticated is not the
// token it holds afterwards.
func (s *Service) establish(ctx context.Context, userID pgtype.UUID) error {
	if err := s.sessions.RenewToken(ctx); err != nil {
		return fmt.Errorf("renew session token: %w", err)
	}

	s.sessions.Put(ctx, sessionUserKey, userID.String())

	return nil
}

// HandleLogout destroys the session. Idempotent: logging out twice is a success.
func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		s.log.ErrorContext(r.Context(), "destroy session", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "logout failed")

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Authenticate resolves the request's credential to a verified Principal.
//
// Order: an explicit Authorization header wins over an ambient cookie, because a
// caller that presented a credential meant to use it, and silently falling back
// to whatever session the browser happened to carry would make the effective
// identity depend on which one the server preferred.
//
// Exported so the cache backends (M2+) can authenticate the same way without
// re-deriving what a Principal is.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if token, ok := bearerToken(r); ok {
		// One header, two credential kinds, told apart by the `bkry_` prefix. A key
		// is not a JWT and a JWT is not a key, so there is no ambiguity to resolve.
		if looksLikeAPIKey(token) {
			p, err := s.authenticateKey(ctx, token)
			s.observeErr(MethodAPIKey, err)

			return p, err
		}

		p, err := s.authenticateBearer(ctx, token)
		s.observeErr(MethodBearer, err)

		return p, err
	}

	// Only touch scs on a context it actually loaded. On the cache path it has not,
	// and asking would panic rather than return "no session".
	if sessionLoaded(ctx) {
		raw := s.sessions.GetString(ctx, sessionUserKey)
		if raw == "" {
			return nil, ErrUnauthenticated
		}

		var userID pgtype.UUID
		if err := userID.Scan(raw); err != nil {
			return nil, ErrUnauthenticated
		}

		p, err := s.loadPrincipal(ctx, userID, s.sessionMethod())
		if err != nil {
			// The session names a user who is gone. Not an error the caller can act
			// on -- it is simply not authenticated any more.
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrUnauthenticated
			}

			return nil, err
		}

		return p, nil
	}

	return nil, ErrUnauthenticated
}

// sessionMethod distinguishes a DEV_LOGIN session from a real OIDC one, so the
// method a Principal reports is the truth about how it was obtained.
func (s *Service) sessionMethod() Method {
	if s.provider == nil && s.devLogin {
		return MethodDev
	}

	return MethodSession
}

// authenticateBearer verifies an OIDC ID token presented by the CLI.
//
// The device grant has no server-side callback -- the CLI talks to the IdP
// directly -- so the FIRST time the server ever sees a user is on a Bearer
// request. JIT provisioning and group reconciliation therefore have to happen
// here, on the request, or a device-grant user could never exist.
//
// Reconciling on EVERY bearer request would be a write per API call. Instead we
// reconcile when there is no user yet, or when the ID token was issued after the
// login we last recorded -- i.e. when the IdP has freshly re-asserted this user's
// groups, which is exactly what a new device grant or an offline_access refresh
// produces. That makes "reconciled on every login" true for the CLI, where the
// token issuance IS the login.
func (s *Service) authenticateBearer(ctx context.Context, raw string) (Principal, error) {
	if s.provider == nil {
		return nil, ErrUnauthenticated
	}

	id, err := s.provider.VerifyIDToken(ctx, raw)
	if err != nil {
		return nil, err
	}

	user, err := s.store.GetUserByIssuerSubject(ctx, repository.GetUserByIssuerSubjectParams{
		Issuer:  id.Issuer,
		Subject: id.Subject,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// First contact: JIT-provision, gated by the allowed-groups list.
		userID, rerr := s.Reconcile(ctx, id)
		if rerr != nil {
			return nil, rerr
		}

		return s.loadPrincipal(ctx, userID, MethodBearer)

	case err != nil:
		return nil, fmt.Errorf("look up user: %w", err)
	}

	if staleLogin(user.LastLoginAt, id.IssuedAt) {
		if _, rerr := s.Reconcile(ctx, id); rerr != nil {
			return nil, rerr
		}
	}

	return s.loadPrincipal(ctx, user.ID, MethodBearer)
}

// staleLogin reports whether the ID token is newer than the login we recorded.
func staleLogin(lastLogin pgtype.Timestamptz, issuedAt time.Time) bool {
	if !lastLogin.Valid {
		return true
	}

	return issuedAt.After(lastLogin.Time)
}

// Middleware puts a VERIFIED Principal in the request context, or refuses the
// request with a 401.
//
// It never calls next with a missing principal, and there is no zero-value
// Principal for it to leave behind if it did: FromContext returns (nil, false),
// and a nil Principal has no roles that could read as "some valid user".
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Authenticate(r.Context(), r)
		if err != nil {
			s.deny(w, r, err)

			return
		}

		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

func (s *Service) deny(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrLoginNotAllowed):
		http.Error(w, ErrLoginNotAllowed.Error(), http.StatusForbidden)
	case errors.Is(err, ErrUnauthenticated),
		errors.Is(err, ErrTokenInvalid),
		errors.Is(err, ErrKeyInvalid):
		w.Header().Set("WWW-Authenticate", `Bearer realm="bakery"`)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	default:
		s.log.ErrorContext(r.Context(), "authentication failed", slog.Any("error", err))
		http.Error(w, "authentication failed", http.StatusInternalServerError)
	}
}

// CreateKeyInput is a request to mint an API key.
//
// A key is minted for the CALLER, always. There is no user_id here on purpose:
// "issue a key on behalf of another user" is a credential-forging primitive, and
// the schema's per-user key model exists precisely so that a key is traceable to
// one human.
type CreateKeyInput struct {
	OrgID     pgtype.UUID
	ProjectID pgtype.UUID
	Name      string
	Scope     Scope
	ExpiresAt *time.Time
}

// CreateAPIKey mints a project-scoped key for the calling principal and returns
// the plaintext EXACTLY ONCE.
//
// The scope is capped at the authority of the caller's project role. This cap has
// to be applied here, at creation, because validation deliberately does not join
// project_memberships -- checking the role on every cache request would put a
// second probe on the sstate HEAD storm. Paid once, at human speed, instead of on
// every request forever.
func (s *Service) CreateAPIKey(ctx context.Context, p Principal, in CreateKeyInput) (NewAPIKey, repository.CreateAPIKeyRow, error) {
	if p == nil {
		return NewAPIKey{}, repository.CreateAPIKeyRow{}, ErrUnauthenticated
	}

	// An API key cannot mint another API key. Otherwise a read-scoped key for one
	// project becomes a self-service credential factory.
	if p.Method() == MethodAPIKey {
		return NewAPIKey{}, repository.CreateAPIKeyRow{}, ErrKeyInvalid
	}

	// The membership FK means a key for a non-member cannot exist at all; reading
	// the role first turns that from a 23503 into a real error, and gives us the
	// ceiling to cap the scope against.
	member, err := s.store.GetProjectMembership(ctx, repository.GetProjectMembershipParams{
		UserID:    p.UserID(),
		ProjectID: in.ProjectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NewAPIKey{}, repository.CreateAPIKeyRow{}, fmt.Errorf(
				"%w: you are not a member of this project", ErrScopeExceedsRole)
		}

		return NewAPIKey{}, repository.CreateAPIKeyRow{}, fmt.Errorf("load project membership: %w", err)
	}

	if !ScopeWithinRole(in.Scope, member.Role) {
		return NewAPIKey{}, repository.CreateAPIKeyRow{}, fmt.Errorf(
			"%w: role %q cannot grant scope %q", ErrScopeExceedsRole, member.Role, in.Scope)
	}

	key, err := GenerateAPIKey()
	if err != nil {
		return NewAPIKey{}, repository.CreateAPIKeyRow{}, err
	}

	expires := pgtype.Timestamptz{Time: time.Time{}, InfinityModifier: pgtype.Finite, Valid: false}
	if in.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *in.ExpiresAt, InfinityModifier: pgtype.Finite, Valid: true}
	}

	row, err := s.store.CreateAPIKey(ctx, repository.CreateAPIKeyParams{
		UserID:      p.UserID(),
		ProjectID:   in.ProjectID,
		Name:        in.Name,
		TokenSha256: key.Hash,
		TokenPrefix: key.Prefix,
		Scope:       in.Scope,
		ExpiresAt:   expires,
	})
	if err != nil {
		return NewAPIKey{}, repository.CreateAPIKeyRow{}, fmt.Errorf("create api key: %w", err)
	}

	return key, row, nil
}

// observe records an auth attempt. The labels are the METHOD and the OUTCOME --
// never the key, never the subject, never the project. A metric label carrying a
// per-key or per-object value mints one time series per value and kills Prometheus
// inside a single build.
func (s *Service) observe(method Method, result string) {
	if s.metrics == nil {
		return
	}

	s.metrics.AuthAttempts.WithLabelValues(string(method), result).Inc()
}

func (s *Service) observeErr(method Method, err error) {
	switch {
	case err == nil:
		s.observe(method, "ok")
	case errors.Is(err, ErrLoginNotAllowed):
		s.observe(method, "denied")
	default:
		s.observe(method, "error")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	// Headers, then WriteHeader, then body. Setting a header after WriteHeader is
	// a silent no-op, and encoding before it flushes an implicit 200.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Error codes for /api/v1/auth/* failures.
//
// internal/api owns the closed error-code vocabulary the SPA and CLI branch on
// (api.CodeBadRequest, api.CodeUnauthorized, ...), but this package CANNOT import
// it: internal/api imports internal/auth, so the dependency only runs one way. The
// OIDC handlers live here rather than in the api layer, so the codes they emit are
// restated here and MUST stay equal to their api.CodeX counterparts. A refactor
// that reworded one and not the other would silently break a client's `switch
// error.code`; TestAuthErrorEnvelope pins the strings.
const (
	codeBadRequest   = "bad_request"    // api.CodeBadRequest
	codeUnauthorized = "unauthorized"   // api.CodeUnauthorized
	codeForbidden    = "forbidden"      // api.CodeForbidden
	codeInternal     = "internal_error" // api.CodeInternal
)

// authErrorBody mirrors api.ErrorBody on the wire, byte for byte, so every non-2xx
// under /api/v1 -- including the OIDC handlers, which the api package delegates to
// with its `raw` wrapper and never wraps in its own envelope -- is the one shape a
// client can `res.json()` and read `.error.code` from. A text/plain http.Error body
// here would make a SPA that does `const {error} = await res.json()` throw a parse
// error instead of branching on the code.
type authErrorBody struct {
	Error authErrorDetail `json:"error"`
}

type authErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeAuthError renders the shared error envelope. It is the http.Error of this
// package: same call shape (writer, status, message) plus the machine-readable code
// the envelope requires.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	// A 401 must advertise how to authenticate, exactly as api.writeError does.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bakery"`)
	}

	writeJSON(w, status, authErrorBody{Error: authErrorDetail{Code: code, Message: message}})
}
