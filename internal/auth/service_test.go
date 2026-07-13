package auth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// seedMember gives the reconciled user a project and a membership at role, and
// returns the org id, project id and a live Principal.
func seedMember(t *testing.T, ts *testService, role ProjectRole) (pgtype.UUID, pgtype.UUID, Principal) {
	t.Helper()

	ctx := t.Context()

	// acme-devs is a plain org MEMBER, so the project role is the only thing that
	// can grant write -- which is what makes the scope cap observable below. An org
	// admin would short-circuit it.
	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-devs"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	orgID := orgIDOf(t, ts, "acme")

	project, err := ts.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: orgID, Slug: "yocto", Name: "Yocto",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := ts.store.UpsertProjectMembership(ctx, repository.UpsertProjectMembershipParams{
		UserID: userID, ID: project.ID, Role: role,
	}); err != nil {
		t.Fatalf("UpsertProjectMembership: %v", err)
	}

	p, err := ts.loadPrincipal(ctx, userID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal: %v", err)
	}

	return orgID, project.ID, p
}

// TestCreateAPIKeyCapsScopeAtTheRole.
//
// The cap MUST be applied at creation, because validation deliberately never joins
// project_memberships -- a second probe on a BB_NUMBER_THREADS-parallel HEAD storm
// is exactly the thing the schema was shaped to avoid. So if a reader could mint a
// write key here, nothing downstream would ever notice: the key would validate,
// forever, as a write grant its owner never had.
func TestCreateAPIKeyCapsScopeAtTheRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		role    ProjectRole
		scope   Scope
		wantErr bool
	}{
		{name: "a reader may mint a read key", role: ProjectRoleReader, scope: ScopeRead, wantErr: false},
		{name: "a reader may NOT mint a write key", role: ProjectRoleReader, scope: ScopeWrite, wantErr: true},
		{name: "a writer may mint a write key", role: ProjectRoleWriter, scope: ScopeWrite, wantErr: false},
		{name: "an admin may mint a write key", role: ProjectRoleAdmin, scope: ScopeWrite, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := newTestService(t, testGroupMap, false)
			_, projectID, p := seedMember(t, ts, tt.role)

			key, row, err := ts.CreateAPIKey(t.Context(), p, CreateKeyInput{
				OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "ci",
				Scope: tt.scope, ExpiresAt: nil,
			})

			if tt.wantErr {
				if !errors.Is(err, ErrScopeExceedsRole) {
					t.Fatalf("CreateAPIKey() = %v, want ErrScopeExceedsRole", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("CreateAPIKey() error = %v", err)
			}

			if row.Scope != tt.scope {
				t.Errorf("stored scope = %q, want %q", row.Scope, tt.scope)
			}

			// The plaintext exists exactly once, here, and the key validates with it.
			if _, err := ts.authenticateKey(t.Context(), key.Token); err != nil {
				t.Fatalf("the freshly minted key does not validate: %v", err)
			}
		})
	}
}

// TestCreateAPIKeyRequiresMembership: the membership FK means a key for a
// non-member cannot EXIST. Surfacing that as a real error rather than a 23503 is
// the API's job.
func TestCreateAPIKeyRequiresMembership(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	userID, err := ts.Reconcile(ctx, identity("s1", "dev@acme.example", "acme-owners"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// An org OWNER -- but with no project membership row.
	project, err := ts.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: orgIDOf(t, ts, "acme"), Slug: "yocto", Name: "Yocto",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	p, err := ts.loadPrincipal(ctx, userID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal: %v", err)
	}

	_, _, err = ts.CreateAPIKey(ctx, p, CreateKeyInput{
		OrgID: pgtype.UUID{}, ProjectID: project.ID, Name: "ci", Scope: ScopeRead, ExpiresAt: nil,
	})
	if !errors.Is(err, ErrScopeExceedsRole) {
		t.Fatalf("CreateAPIKey() for a non-member = %v, want a refusal", err)
	}
}

// TestAnAPIKeyCannotMintAnotherKey: otherwise a read-scoped key for one project is
// a self-service credential factory.
func TestAnAPIKeyCannotMintAnotherKey(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	_, projectID, p := seedMember(t, ts, ProjectRoleAdmin)

	key, _, err := ts.CreateAPIKey(t.Context(), p, CreateKeyInput{
		OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "ci", Scope: ScopeWrite, ExpiresAt: nil,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	keyPrincipal, err := ts.authenticateKey(t.Context(), key.Token)
	if err != nil {
		t.Fatalf("authenticateKey: %v", err)
	}

	_, _, err = ts.CreateAPIKey(t.Context(), keyPrincipal, CreateKeyInput{
		OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "ci-2", Scope: ScopeWrite, ExpiresAt: nil,
	})
	if err == nil {
		t.Fatal("an API key minted another API key; a key must never be a credential factory")
	}
}

// TestAPIKeyExpiryAndRevocation: both are enforced in the validation query itself,
// so neither depends on the caller remembering to check.
func TestAPIKeyExpiryAndRevocation(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	_, projectID, p := seedMember(t, ts, ProjectRoleWriter)

	t.Run("an expired key does not validate", func(t *testing.T) {
		key, row, err := ts.CreateAPIKey(ctx, p, CreateKeyInput{
			OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "soon",
			Scope: ScopeRead, ExpiresAt: ptr(time.Now().Add(time.Hour)),
		})
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}

		if _, err := ts.authenticateKey(ctx, key.Token); err != nil {
			t.Fatalf("the live key does not validate: %v", err)
		}

		// The key has to be AGED, not merely handed a past expiry: the schema's
		// api_keys_expires_after_created CHECK refuses expires_at <= created_at
		// outright, so "expired before it was created" is not a row Postgres will
		// hold. (That constraint fired on the first draft of this test, which is the
		// constraint doing exactly its job.) Move both timestamps into the past.
		if _, err := ts.pool.Exec(ctx, `
			UPDATE api_keys
			   SET created_at = now() - interval '2 hours',
			       expires_at = now() - interval '1 hour'
			 WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("age the key: %v", err)
		}

		if _, err := ts.authenticateKey(ctx, key.Token); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("an EXPIRED key still validates: %v", err)
		}
	})

	t.Run("a revoked key does not validate", func(t *testing.T) {
		key, row, err := ts.CreateAPIKey(ctx, p, CreateKeyInput{
			OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "revoke-me",
			Scope: ScopeWrite, ExpiresAt: nil,
		})
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}

		if _, err := ts.authenticateKey(ctx, key.Token); err != nil {
			t.Fatalf("the live key does not validate: %v", err)
		}

		if _, err := ts.store.RevokeAPIKey(ctx, row.ID); err != nil {
			t.Fatalf("RevokeAPIKey: %v", err)
		}

		if _, err := ts.authenticateKey(ctx, key.Token); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("a REVOKED key still validates: %v", err)
		}
	})

	t.Run("a key with no expiry never expires", func(t *testing.T) {
		key, _, err := ts.CreateAPIKey(ctx, p, CreateKeyInput{
			OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "forever",
			Scope: ScopeRead, ExpiresAt: nil,
		})
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}

		if _, err := ts.authenticateKey(ctx, key.Token); err != nil {
			t.Fatalf("a key with a NULL expiry does not validate: %v", err)
		}
	})
}

// TestMiddlewareRequiresAVerifiedPrincipal: the failure mode for an
// unauthenticated request is a MISSING principal and a 401 -- never a zero-valued
// one handed to the handler.
func TestMiddlewareRequiresAVerifiedPrincipal(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	_, projectID, _ := seedMember(t, ts, ProjectRoleWriter)

	orgID := orgIDOf(t, ts, "acme")

	key, _, err := ts.CreateAPIKey(t.Context(), mustPrincipal(t, ts), CreateKeyInput{
		OrgID: pgtype.UUID{}, ProjectID: projectID, Name: "ci", Scope: ScopeWrite, ExpiresAt: nil,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	var seen Principal

	protected := ts.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, ok := FromRequest(r)
		if !ok {
			t.Error("the handler ran with NO principal in the context")
		}

		seen = p
	}))

	tests := []struct {
		name     string
		header   string
		wantCode int
	}{
		{name: "no credential", header: "", wantCode: http.StatusUnauthorized},
		{name: "a garbage key", header: "Bearer bkry_" + "z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0", wantCode: http.StatusUnauthorized},
		{name: "a garbage bearer", header: "Bearer not.a.jwt", wantCode: http.StatusUnauthorized},
		{name: "a live API key", header: "Bearer " + key.Token, wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil

			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, newRequest(t, http.MethodGet, "/api/v1/me", tt.header))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body)
			}

			if tt.wantCode != http.StatusOK {
				if seen != nil {
					t.Fatal("the handler ran for an unauthenticated request")
				}

				// A 401 must say how to authenticate.
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Error("the 401 carried no WWW-Authenticate header")
				}

				return
			}

			if seen == nil {
				t.Fatal("the handler ran without a principal")
			}

			if seen.Method() != MethodAPIKey {
				t.Errorf("Method() = %q, want %q", seen.Method(), MethodAPIKey)
			}

			if !seen.CanWriteProject(orgID, projectID) {
				t.Error("the authenticated key cannot write its own project")
			}
		})
	}
}

// TestAuthConfigReportsTheDevFlag: /auth/config is what the SPA and CLI configure
// themselves from. It REPORTS dev_login_enabled; nothing can SET it there.
func TestAuthConfig(t *testing.T) {
	t.Parallel()

	t.Run("no OIDC configured", func(t *testing.T) {
		t.Parallel()

		ts := newTestService(t, testGroupMap, true)

		cfg := ts.AuthConfig()
		if cfg.OIDCEnabled {
			t.Error("OIDCEnabled = true with no provider")
		}

		if !cfg.DevLoginEnabled {
			t.Error("DevLoginEnabled = false, but the flag is on")
		}

		rec := httptest.NewRecorder()
		ts.HandleAuthConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want JSON", ct)
		}
	})

	t.Run("OIDC configured", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)

		ts := newTestService(t, testGroupMap, false)
		ts.provider = idp.provider(t)

		cfg := ts.AuthConfig()

		if !cfg.OIDCEnabled {
			t.Error("OIDCEnabled = false with a provider")
		}

		if cfg.DevLoginEnabled {
			t.Error("DevLoginEnabled = true, but the flag is off")
		}

		if cfg.Issuer != idp.issuer() || cfg.ClientID != idp.clientID {
			t.Errorf("AuthConfig() = %+v, want the discovered issuer and client id", cfg)
		}

		// The CLI needs this to run the device grant without redoing discovery.
		if cfg.DeviceAuthorizationEndpoint == "" {
			t.Error("DeviceAuthorizationEndpoint is empty; the CLI cannot start a device flow")
		}
	})
}

// TestLoginAndCallbackRejectAForgedState: the state parameter is what binds the
// callback to the browser that started the flow. Without the check, any site can
// walk a victim through an attacker-initiated login.
func TestCallbackRejectsAForgedState(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)

	ts := newTestService(t, testGroupMap, false)
	ts.provider = idp.provider(t)

	// No login happened, so the session holds no state -- and an attacker-supplied
	// one must not be accepted.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?code=c&state=attacker-state", nil)

	ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback with a forged state = %d, want 400", rec.Code)
	}

	// The refusal is the shared JSON envelope, not a text/plain http.Error body:
	// the SPA drives this over fetch and decodes every non-2xx from /api/v1 the same
	// way. A bare-text body makes `await res.json()` throw instead of branching.
	assertAuthErrorEnvelope(t, rec.Result(), codeBadRequest)
}

// assertAuthErrorEnvelope pins the contract that every /api/v1/auth/* non-2xx is
// the same JSON error envelope internal/api emits: Content-Type application/json,
// a body that decodes cleanly to {"error":{"code","message"}}, and the expected
// machine-readable code. This is the assertion a text/plain http.Error body fails.
func assertAuthErrorEnvelope(t *testing.T, res *http.Response, wantCode string) {
	t.Helper()

	if ct := res.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the JSON envelope's %q", ct, "application/json; charset=utf-8")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	_ = res.Body.Close()

	// This is exactly what a client's `await res.json()` does. A text/plain body
	// ("logout failed\n") fails right here, which is the bug.
	var env authErrorBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not the JSON error envelope (a client's res.json() would throw): %v\nbody: %q", err, body)
	}

	if env.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q (clients branch on the code, never the message)", env.Error.Code, wantCode)
	}

	if env.Error.Message == "" {
		t.Error("error.message is empty; the envelope must carry a human-readable message")
	}
}

// TestLogoutRendersTheErrorEnvelope: when Destroy fails, HandleLogout must return
// the shared JSON envelope with a 500, not a bare text/plain http.Error body.
//
// This is the exact reproduction from the finding: a session store whose Delete is
// broken, driven through the real LoadAndSave -> HandleLogout chain. The happy path
// (204, empty) is asserted alongside it so the test also proves the envelope is
// reserved for the failure.
func TestLogoutRendersTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	store := newFakeSessionStore()

	svc := &Service{
		sessions: NewSessionManager(store, false),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics:  metrics.New(),
	}

	// Establish a real session so logout has a token to destroy. Without one, Destroy
	// is a no-op and never reaches the store.
	seedRec := httptest.NewRecorder()
	svc.LoadAndSave(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := svc.sessions.RenewToken(r.Context()); err != nil {
			t.Fatalf("seed RenewToken: %v", err)
		}

		svc.sessions.Put(r.Context(), sessionUserKey, uuid(0x01).String())
	})).ServeHTTP(seedRec, httptest.NewRequest(http.MethodGet, "/api/v1/seed", nil))

	var sessionCookie *http.Cookie

	for _, c := range seedRec.Result().Cookies() {
		if c.Name == "bakery_session" {
			sessionCookie = c
		}
	}

	if sessionCookie == nil {
		t.Fatal("seed did not set a session cookie; the test cannot exercise logout")
	}

	logout := func() *http.Response {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		req.AddCookie(sessionCookie)
		svc.LoadAndSave(http.HandlerFunc(svc.HandleLogout)).ServeHTTP(rec, req)

		return rec.Result()
	}

	// Arm the failure and log out.
	store.deleteErr = errFake

	res := logout()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout with a broken store = %d, want 500", res.StatusCode)
	}

	assertAuthErrorEnvelope(t, res, codeInternal)

	// And the happy path is a clean 204 with no body -- the envelope is for failures.
	store.deleteErr = nil

	ok := logout()
	if ok.StatusCode != http.StatusNoContent {
		t.Fatalf("a successful logout = %d, want 204", ok.StatusCode)
	}
}

func ptr[T any](v T) *T { return &v }

// mustPrincipal reloads the seeded member as a Principal.
func mustPrincipal(t *testing.T, ts *testService) Principal {
	t.Helper()

	user, err := ts.store.GetUserByIssuerSubject(t.Context(), repository.GetUserByIssuerSubjectParams{
		Issuer: "https://idp.example.com", Subject: "s1",
	})
	if err != nil {
		t.Fatalf("GetUserByIssuerSubject: %v", err)
	}

	p, err := ts.loadPrincipal(t.Context(), user.ID, MethodSession)
	if err != nil {
		t.Fatalf("loadPrincipal: %v", err)
	}

	return p
}
