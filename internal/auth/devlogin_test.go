package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestDevLoginIsAbsentWhenTheFlagIsOff is the test the brief asks for by name, and
// the assertion is specifically 404 -- not 403, and emphatically not "it works".
//
// A 403 would CONFIRM the endpoint exists and is merely switched off, which tells
// a scanner exactly what to come back for and what to try to flip. A 404 is
// indistinguishable from a binary that was never built with the route.
//
// The endpoint mints a session for a site admin with NO credential. If it can be
// reached when the operator did not ask for it, that is a complete authentication
// bypass, so this test guards the whole system, not a feature flag.
func TestDevLoginIsAbsentWhenTheFlagIsOff(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false) // DevLogin: false
	ctx := t.Context()

	if ts.DevLoginEnabled() {
		t.Fatal("DevLoginEnabled() = true, but the flag was not set")
	}

	rec := httptest.NewRecorder()
	ts.HandleDevLogin(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev-login", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /auth/dev-login with the flag off = %d, want %d.\n"+
			"A %d that is not 404 tells an attacker the endpoint is there and merely disabled.",
			rec.Code, http.StatusNotFound, rec.Code)
	}

	// And it must not have mutated anything on the way to refusing: no session, and
	// no dev user seeded into the database.
	if body := rec.Body.String(); len(body) > 0 && rec.Code != http.StatusNotFound {
		t.Errorf("the disabled endpoint wrote a body: %q", body)
	}

	_, err := ts.store.GetUserByIssuerSubject(ctx, repository.GetUserByIssuerSubjectParams{
		Issuer: DevIssuer, Subject: DevSubject,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the dev user exists with the flag off (err = %v); nothing may seed it", err)
	}

	// SeedDevLogin is a no-op too. Boot calls it unconditionally.
	if err := ts.SeedDevLogin(ctx); err != nil {
		t.Fatalf("SeedDevLogin() with the flag off = %v, want a silent no-op", err)
	}

	_, err = ts.store.GetUserByIssuerSubject(ctx, repository.GetUserByIssuerSubjectParams{
		Issuer: DevIssuer, Subject: DevSubject,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SeedDevLogin() seeded the dev user with the flag off (err = %v)", err)
	}
}

// TestDevLoginSeedsAndMintsASession covers the on path: dev@bakery.local as a site
// admin, plus the dev-org/playground org and project.
func TestDevLoginSeedsAndMintsASession(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, true) // DevLogin: true
	ctx := t.Context()

	if err := ts.SeedDevLogin(ctx); err != nil {
		t.Fatalf("SeedDevLogin() error = %v", err)
	}

	user, err := ts.store.GetUserByIssuerSubject(ctx, repository.GetUserByIssuerSubjectParams{
		Issuer: DevIssuer, Subject: DevSubject,
	})
	if err != nil {
		t.Fatalf("the dev user was not seeded: %v", err)
	}

	if user.Email != DevEmail {
		t.Errorf("Email = %q, want %q", user.Email, DevEmail)
	}

	if user.SiteRole != SiteRoleAdmin {
		t.Errorf("SiteRole = %q, want %q", user.SiteRole, SiteRoleAdmin)
	}

	route, err := ts.store.ResolveRoute(ctx, repository.ResolveRouteParams{
		Slug: DevOrgSlug, Slug_2: DevProjectSlug,
	})
	if err != nil {
		t.Fatalf("dev-org/playground was not seeded: %v", err)
	}

	// The seeded user can actually use the seeded project.
	p, err := ts.loadPrincipal(ctx, user.ID, MethodDev)
	if err != nil {
		t.Fatalf("loadPrincipal() error = %v", err)
	}

	if !p.IsSiteAdmin() {
		t.Error("the dev user is not a site admin")
	}

	if !p.CanAdminProject(route.OrgID, route.ProjectID) {
		t.Error("the dev user cannot administer dev-org/playground")
	}

	// Idempotent: boot runs the seed on every start, and so does the handler.
	if err := ts.SeedDevLogin(ctx); err != nil {
		t.Fatalf("SeedDevLogin() is not idempotent: %v", err)
	}

	// And the handler mints a session rather than 404ing.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev-login", nil)

	ts.LoadAndSave(http.HandlerFunc(ts.HandleDevLogin)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/dev-login with the flag on = %d, want 200. Body: %s", rec.Code, rec.Body)
	}

	if len(rec.Result().Cookies()) == 0 {
		t.Error("the dev login set no session cookie")
	}
}
