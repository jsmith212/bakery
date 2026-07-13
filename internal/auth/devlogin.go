package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// The synthetic developer identity.
//
// The issuer is "dev", which is why the schema keys a user on (issuer, subject)
// rather than on email: the dev user coexists with every real IdP's users with no
// special-case column and no chance of colliding with a real `sub`.
const (
	DevIssuer      = "dev"
	DevSubject     = "dev"
	DevEmail       = "dev@bakery.local"
	DevDisplayName = "Dev"
	DevOrgSlug     = "dev-org"
	DevProjectSlug = "playground"

	// DevLoginGroup is what the dev membership records in oidc_group. The dev login
	// is a synthetic CLAIM, not a local grant: it is written to the OIDC half so it
	// reconciles away like any other, and the audit trail says where it came from.
	DevLoginGroup = "dev-login"
)

// DevLoginEnabled reports the flag. It is READ here and set NOWHERE: the only
// writer is Kong, from DEV_LOGIN_ENABLED or --dev-login-enabled, at boot.
//
// There is deliberately no SetDevLogin, no API route that flips it and no database
// column that persists it. It mints a session for a site admin with NO credential,
// so any runtime path that could enable it would be a total authentication bypass
// -- which is why it must remain a boot-time-only decision, and why the endpoint
// below is absent rather than merely forbidden when it is off.
func (s *Service) DevLoginEnabled() bool { return s.devLogin }

// SeedDevLogin creates dev@bakery.local (site admin) plus the dev-org/playground
// org and project. It is a no-op when the flag is off.
//
// Call it at boot, after migrations.
func (s *Service) SeedDevLogin(ctx context.Context) error {
	if !s.devLogin {
		return nil
	}

	if _, err := s.seedDevUser(ctx); err != nil {
		return err
	}

	s.log.WarnContext(ctx, "DEV_LOGIN_ENABLED is on: an unauthenticated endpoint will mint a site-admin session",
		slog.String("email", DevEmail))

	return nil
}

// seedDevUser is idempotent: every statement is an upsert or an ensure, so it is
// safe on every boot.
func (s *Service) seedDevUser(ctx context.Context) (pgtype.UUID, error) {
	orgID, err := s.ensureOrg(ctx, DevOrgSlug)
	if err != nil {
		return pgtype.UUID{}, err
	}

	projectID, err := s.ensureDevProject(ctx, orgID)
	if err != nil {
		return pgtype.UUID{}, err
	}

	var userID pgtype.UUID

	err = s.store.Tx(ctx, func(q *repository.Queries) error {
		// The dev user's site-admin role is a synthetic CLAIM (site_role_oidc), not a
		// local grant, and it names DevLoginGroup as its source -- so it reconciles away
		// like any other claim, and the site-admin listing reports it honestly as
		// claim-derived rather than as a local grant nobody can account for.
		user, err := q.UpsertUser(ctx, repository.UpsertUserParams{
			Issuer:      DevIssuer,
			Subject:     DevSubject,
			Email:       DevEmail,
			DisplayName: DevDisplayName,
			SiteRole:    SiteRoleAdmin,
			SiteGroup:   pgtype.Text{String: DevLoginGroup, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("upsert dev user: %w", err)
		}

		userID = user.ID

		// Org membership must exist before the project membership: the composite FK
		// from project_memberships to org_memberships makes "a project member is an
		// org member" a fact the database enforces, not one we remember.
		// oidc_role: the dev login is a synthetic CLAIM, not a local grant. It must
		// reconcile away like any other claim-derived membership.
		if err := q.ReconcileOrgMembershipUpsert(ctx, repository.ReconcileOrgMembershipUpsertParams{
			UserID:    user.ID,
			OrgID:     orgID,
			OidcRole:  repository.NullOrgRole{OrgRole: OrgRoleOwner, Valid: true},
			OidcGroup: pgtype.Text{String: DevLoginGroup, Valid: true},
		}); err != nil {
			return fmt.Errorf("upsert dev org membership: %w", err)
		}

		if _, err := q.UpsertProjectMembership(ctx, repository.UpsertProjectMembershipParams{
			UserID: user.ID,
			ID:     projectID,
			Role:   ProjectRoleAdmin,
		}); err != nil {
			return fmt.Errorf("upsert dev project membership: %w", err)
		}

		return nil
	})
	if err != nil {
		return pgtype.UUID{}, err
	}

	return userID, nil
}

func (s *Service) ensureDevProject(ctx context.Context, orgID pgtype.UUID) (pgtype.UUID, error) {
	route, err := s.store.ResolveRoute(ctx, repository.ResolveRouteParams{
		Slug:   DevOrgSlug,
		Slug_2: DevProjectSlug,
	})
	if err == nil {
		return route.ProjectID, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("resolve dev project: %w", err)
	}

	project, err := s.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: orgID,
		Slug:  DevProjectSlug,
		Name:  DevProjectSlug,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("create dev project: %w", err)
	}

	return project.ID, nil
}

// HandleDevLogin mints a session for dev@bakery.local without any credential.
//
// When the flag is off it is a 404, not a 403. That is the point: a 403 CONFIRMS
// the endpoint exists and is merely disabled, which tells a scanner exactly what
// to come back for and what to try to flip. A 404 is indistinguishable from a
// server that was never built with the route at all. The API layer should also
// simply not register it -- this check is the second lock, for the case where
// someone registers it unconditionally.
func (s *Service) HandleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.devLogin {
		http.NotFound(w, r)

		return
	}

	ctx := r.Context()

	userID, err := s.seedDevUser(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "dev login seed failed", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "dev login failed")

		return
	}

	if err := s.establish(ctx, userID); err != nil {
		s.log.ErrorContext(ctx, "dev login session failed", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, codeInternal, "dev login failed")

		return
	}

	s.observe(MethodDev, "ok")

	writeJSON(w, http.StatusOK, map[string]string{
		"email":   DevEmail,
		"org":     DevOrgSlug,
		"project": DevProjectSlug,
	})
}
