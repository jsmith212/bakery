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

	// B8/R8#2: a state mismatch is browser-reachable by an ordinary user too (an
	// old or reused callback link, a double-back-button), not only by an attacker,
	// so it redirects into the SPA's own /login (reason stale_request) rather than
	// writing raw JSON at the callback URL.
	assertDeniedRedirect(t, rec, deniedStaleRequest)
}

// assertDeniedRedirect is the shared assertion for every browser-facing denial
// arm of HandleLogin/HandleCallback (B8, R8#2): a 302 into the SPA's own /login
// carrying `reason`, and NOT the raw JSON error envelope this used to write at
// the callback URL. No browser or fetch client reads a redirect's body, but
// http.Redirect's own tiny HTML stub must not happen to decode as the shared
// {"error":{...}} shape either -- that would be the old bug wearing a 302.
func assertDeniedRedirect(t *testing.T, rec *httptest.ResponseRecorder, reason string) {
	t.Helper()

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (a redirect into the SPA, not a bare JSON page). body: %s",
			rec.Code, rec.Body.String())
	}

	want := "/login?denied=" + reason
	if loc := rec.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}

	var env authErrorBody
	if json.Unmarshal(rec.Body.Bytes(), &env) == nil && env.Error.Code != "" {
		t.Errorf("denial body still decodes as the JSON error envelope: %s", rec.Body.String())
	}
}

// TestCallbackDenialReasons is R8#2's table: every browser-reachable
// HandleCallback failure arm other than the login-gate one (which has its own
// two dedicated tests below, driving the real reconciler both ways) must redirect
// into the SPA's own /login with the reason login/+page.svelte switches its copy
// on, never write the raw JSON envelope at the callback URL.
func TestCallbackDenialReasons(t *testing.T) {
	t.Parallel()

	t.Run("the identity provider itself refused", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)
		ts := newTestService(t, testGroupMap, false)
		ts.provider = idp.provider(t)

		// The ?error= check runs before the state is even read, so no session needs
		// seeding: this is the shape the IdP's OWN redirect carries when a user
		// declines consent or the IdP itself refuses.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?error=access_denied", nil)

		ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

		assertDeniedRedirect(t, rec, deniedIDPRefused)
	})

	t.Run("the callback carried no code", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)
		ts := newTestService(t, testGroupMap, false)
		ts.provider = idp.provider(t)

		cookie := seedCallbackState(t, ts, "test-state")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?state=test-state", nil)
		req.AddCookie(cookie)

		ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

		assertDeniedRedirect(t, rec, deniedStaleRequest)
	})

	t.Run("code exchange failed", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)
		ts := newTestService(t, testGroupMap, false)
		ts.provider = idp.provider(t)

		cookie := seedCallbackState(t, ts, "test-state")

		// A code the fake IdP never issued: the exact shape of a reused, expired or
		// otherwise-rejected authorization code, which a real IdP's token endpoint
		// also answers with invalid_grant.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/auth/callback?code=never-issued&state=test-state", nil)
		req.AddCookie(cookie)

		ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

		assertDeniedRedirect(t, rec, deniedAuthFailed)
	})

	t.Run("reconciliation failed for a reason that is not the login gate", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)
		ts := newTestService(t, testGroupMap, false)
		ts.provider = idp.provider(t)

		// Seed a FIRST user who already owns this email, under an issuer/subject the
		// second login will not match.
		if _, err := ts.Reconcile(t.Context(), identity("s-first", "collide@acme.example", "acme-devs")); err != nil {
			t.Fatalf("seed the colliding user: %v", err)
		}

		// A second identity presents the SAME email -- case-insensitively, because
		// IdPs are inconsistent about case -- under a DIFFERENT subject. UpsertUser's
		// `ON CONFLICT (issuer, subject)` does not fire (the pair differs), so the
		// INSERT reaches `users_email_lower_key` instead: a genuine Postgres unique
		// violation, NOT ErrLoginNotAllowed. This is the real shape of "reconciliation
		// failed" that is not a login-gate refusal: two IdP identities that (by
		// misconfiguration, or a reassigned mailbox) claim the same email.
		c := defaultClaims(idp)
		c.sub = "s-second"
		c.email = "COLLIDE@acme.example"
		c.groups = []string{"acme-devs"}

		idp.issueCode("code-collide", idp.signIDToken(t, c))

		cookie := seedCallbackState(t, ts, "test-state")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/auth/callback?code=code-collide&state=test-state", nil)
		req.AddCookie(cookie)

		ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

		assertDeniedRedirect(t, rec, deniedAuthFailed)
	})

	t.Run("session establishment failed after a successful reconciliation", func(t *testing.T) {
		t.Parallel()

		idp := newFakeIDP(t)
		sessionStore := newFakeSessionStore()

		ts := newTestServiceWithSessionStore(t, testGroupMap, sessionStore)
		ts.provider = idp.provider(t)

		c := defaultClaims(idp)
		c.sub = "s-establish-fail"
		c.email = "establish-fail@acme.example"
		c.groups = []string{"acme-devs"}

		idp.issueCode("code-establish-fail", idp.signIDToken(t, c))

		cookie := seedCallbackState(t, ts, "test-state")

		// Arm the failure AFTER seeding: the seed step's own RenewToken must succeed,
		// so that reconciliation genuinely runs and succeeds, and only `establish`'s
		// RenewToken call -- the very last step of the happy path -- fails.
		sessionStore.deleteErr = errFake

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/auth/callback?code=code-establish-fail&state=test-state", nil)
		req.AddCookie(cookie)

		ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

		assertDeniedRedirect(t, rec, deniedAuthFailed)
	})
}

// TestLoginRenewTokenFailureRedirectsToLogin is HandleLogin's other 500 arm
// (R8#2): the session-fixation-defeating RenewToken call that runs after a
// successful AuthCodeURL build. scs's RenewToken only calls the store's Delete
// when the session ALREADY has a token (`if sd.token != "" {...}` --
// data.go:286), so this must drive HandleLogin with a PRE-EXISTING session
// cookie already attached -- which is exactly the case the doc comment above
// RenewToken's call site names: "a pre-existing (possibly attacker-planted)
// session token". A cookieless request cannot exercise this branch at all.
//
// AuthCodeURL's own failure mode is a crypto/rand read error, which is not
// practically triggerable from a table test without making randomToken() a seam
// solely to test a branch that, in real deployment, crypto/rand does not fail --
// so it is left uncovered here and the redirect behaviour for it is the same
// one-line change as this arm's, reviewed by eye.
func TestLoginRenewTokenFailureRedirectsToLogin(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	sessionStore := newFakeSessionStore()

	ts := newTestServiceWithSessionStore(t, testGroupMap, sessionStore)
	ts.provider = idp.provider(t)

	// Seed a pre-existing session (deleteErr still unarmed) so RenewToken has a
	// token to delete, then arm the failure for the real HandleLogin request.
	cookie := seedCallbackState(t, ts, "pre-existing-state")
	sessionStore.deleteErr = errFake

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	req.AddCookie(cookie)

	ts.LoadAndSave(http.HandlerFunc(ts.HandleLogin)).ServeHTTP(rec, req)

	assertDeniedRedirect(t, rec, deniedAuthFailed)
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

// TestCallbackDeniedRedirectsToLogin is B8 (SPA->API wiring wave, critique
// finding 2): a login-gate or unreadable-groups-claim denial from Reconcile
// must land the browser on the SPA's OWN /login, not a bare JSON 403 rendered
// at /api/v1/auth/callback. The old behaviour left a user who clicked
// "Continue with SSO" on a page with no console chrome and no way out.
//
// This drives the AZURE AD OVERAGE shape specifically (mocks_test.go's
// azureOverageClaims): NO `groups` claim at all, replaced by `_claim_names` /
// `_claim_sources` pointing at Graph -- the "unreadable claim" arm of
// ErrLoginNotAllowed (reconcile.go's GroupsPresent guard), which is the SAME
// sentinel the login-gate arm produces and therefore the SAME branch in
// HandleCallback. It is also the one the fake IdP can mint without needing a
// group map that names a gate at all.
func TestCallbackDeniedRedirectsToLogin(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)

	ts := newTestService(t, testGroupMap, false)
	ts.provider = idp.provider(t)

	c := azureOverageClaims(idp)
	c.sub = "s-gated"
	c.email = "ghost@acme.example"

	idToken := idp.signIDToken(t, c)
	idp.issueCode("code-gated", idToken)

	// Seed a session carrying the state HandleLogin would have stashed. The
	// nonce is left unset (verifyNonce skips the check entirely when the
	// stashed nonce is "", which is exactly the device-grant-shaped state this
	// mints), and this token carries no nonce claim either.
	seedRec := httptest.NewRecorder()
	ts.LoadAndSave(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := ts.sessions.RenewToken(r.Context()); err != nil {
			t.Fatalf("seed RenewToken: %v", err)
		}

		ts.sessions.Put(r.Context(), sessionStateKey, "test-state")
		ts.sessions.Put(r.Context(), sessionVerifierKey, "test-verifier")
	})).ServeHTTP(seedRec, httptest.NewRequest(http.MethodGet, "/api/v1/seed", nil))

	var sessionCookie *http.Cookie

	for _, c := range seedRec.Result().Cookies() {
		if c.Name == "bakery_session" {
			sessionCookie = c
		}
	}

	if sessionCookie == nil {
		t.Fatal("seed did not set a session cookie; the test cannot exercise the callback")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?code=code-gated&state=test-state", nil)
	req.AddCookie(sessionCookie)

	ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback denial = %d, want 302 (a redirect into the SPA, not a bare JSON page). "+
			"body: %s", rec.Code, rec.Body.String())
	}

	if loc := rec.Header().Get("Location"); loc != "/login?denied=login_gate" {
		t.Errorf("Location = %q, want %q", loc, "/login?denied=login_gate")
	}

	// And the JSON error envelope this used to write is GONE: http.Redirect's
	// own tiny HTML stub ("<a href=...>Found</a>") is fine -- no browser or
	// fetch client reads a 302's body -- but it must not decode as the shared
	// {"error":{...}} shape a client's `await res.json()` would otherwise try
	// against it.
	var env authErrorBody
	if json.Unmarshal(rec.Body.Bytes(), &env) == nil && env.Error.Code != "" {
		t.Errorf("callback denial body still decodes as the JSON error envelope: %s", rec.Body.String())
	}
}

// TestCallbackDeniedByLoginGateRedirectsToLogin is TestCallbackDeniedRedirectsToLogin's
// OTHER arm: a READABLE groups claim that simply names none of login_groups.
// Same ErrLoginNotAllowed sentinel, same HandleCallback branch, same redirect
// -- proven here against config.GroupMap's login-gate check specifically
// (Resolve, not the GroupsPresent guard) so a future refactor that special-cased
// one arm and not the other would be caught by one of these two tests.
func TestCallbackDeniedByLoginGateRedirectsToLogin(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)

	gatedGroupMap := `{
	  "login_groups": ["bakery-users"],
	  "orgs": [{"slug": "acme", "groups": {"acme-devs": "member"}}]
	}`

	ts := newTestService(t, gatedGroupMap, false)
	ts.provider = idp.provider(t)

	// A perfectly READABLE claim -- the IdP answered, and the answer is
	// "acme-devs" -- which simply is not "bakery-users". This is the ordinary
	// login-gate refusal, not the unreadable-claim trap.
	c := defaultClaims(idp)
	c.sub = "s-not-gated-in"
	c.email = "not-a-bakery-user@acme.example"
	c.groups = []string{"acme-devs"}

	idToken := idp.signIDToken(t, c)
	idp.issueCode("code-ungated-group", idToken)

	seedRec := httptest.NewRecorder()
	ts.LoadAndSave(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := ts.sessions.RenewToken(r.Context()); err != nil {
			t.Fatalf("seed RenewToken: %v", err)
		}

		ts.sessions.Put(r.Context(), sessionStateKey, "test-state")
	})).ServeHTTP(seedRec, httptest.NewRequest(http.MethodGet, "/api/v1/seed", nil))

	var sessionCookie *http.Cookie

	for _, c := range seedRec.Result().Cookies() {
		if c.Name == "bakery_session" {
			sessionCookie = c
		}
	}

	if sessionCookie == nil {
		t.Fatal("seed did not set a session cookie; the test cannot exercise the callback")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/callback?code=code-ungated-group&state=test-state", nil)
	req.AddCookie(sessionCookie)

	ts.LoadAndSave(http.HandlerFunc(ts.HandleCallback)).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("login-gate denial = %d, want 302. body: %s", rec.Code, rec.Body.String())
	}

	if loc := rec.Header().Get("Location"); loc != "/login?denied=login_gate" {
		t.Errorf("Location = %q, want %q", loc, "/login?denied=login_gate")
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
