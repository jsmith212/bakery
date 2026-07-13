package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// ErrLoginNotAllowed means the identity verified, but Bakery will not admit them:
// the ID token carried no group claim, or none of their groups maps to an
// organization. It is the allowed-groups gate, and it FAILS CLOSED.
//
// This is not pedantry. The alternative -- "no groups, so reconcile them to zero
// orgs" -- calls ReconcileOrgMembershipsRemove with an empty keep-set, which
// deletes every org membership the user has, which CASCADES to every project role
// and every API key they hold in those orgs. That is irreversible: re-adding the
// org membership does not bring the keys back. And "no groups" is not a
// hypothetical -- Azure AD replaces a >200-group claim with a `_claim_names`
// overage, so it happens to real, correctly-configured users. A login we cannot
// authorize must be REFUSED, never silently reconciled to nothing.
var ErrLoginNotAllowed = errors.New("auth: this account is not authorized to use Bakery")

// Reconcile JIT-provisions the user and reconciles their site and org roles from
// the ID token's group claim. It runs on EVERY login, and returns the user id.
//
// Org roles and the site role are 100% claim-derived: the IdP is the source of
// truth and this function makes the database agree with it, adding what appeared
// and REMOVING what did not. Project roles are managed in-app and are deliberately
// untouched here -- the reconciler must never write project_memberships.
func (s *Service) Reconcile(ctx context.Context, id Identity) (pgtype.UUID, error) {
	if s.groups == nil {
		return pgtype.UUID{}, fmt.Errorf("%w: no group mapping is configured", ErrLoginNotAllowed)
	}

	// The gate. Resolve fails closed on an absent group claim and on a claim that
	// maps to zero orgs; both mean refuse.
	res, err := s.groups.Resolve(id.Groups)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %w", ErrLoginNotAllowed, err)
	}

	// Orgs named by the mapping file are created outside the transaction below, on
	// purpose. CreateOrganization has no ON CONFLICT clause, so two users of a new
	// org logging in at once would race, and in Postgres a failed statement aborts
	// the WHOLE transaction -- taking the membership reconciliation down with it.
	// Out here each ensure is its own autocommitted statement and the loser of the
	// race simply re-reads the winner's row.
	orgIDs, err := s.ensureOrgs(ctx, slugsOf(res.Orgs))
	if err != nil {
		return pgtype.UUID{}, err
	}

	var userID pgtype.UUID

	err = s.store.Tx(ctx, func(q *repository.Queries) error {
		user, err := q.UpsertUser(ctx, repository.UpsertUserParams{
			Issuer:      id.Issuer,
			Subject:     id.Subject,
			Email:       id.Email,
			DisplayName: id.DisplayName,
			SiteRole:    siteRole(res.SiteRole),
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}

		userID = user.ID

		keep := make([]pgtype.UUID, 0, len(res.Orgs))

		for slug, role := range res.Orgs {
			orgID, ok := orgIDs[slug]
			if !ok {
				continue
			}

			// The OIDC half, and only the OIDC half. `role` is generated from
			// greatest(oidc_role, local_role), so a local grant this reconciler
			// knows nothing about survives -- structurally, because the column that
			// holds it is never named here.
			err := q.ReconcileOrgMembershipUpsert(ctx, repository.ReconcileOrgMembershipUpsertParams{
				UserID:   user.ID,
				OrgID:    orgID,
				OidcRole: repository.NullOrgRole{OrgRole: orgRole(role), Valid: true},
			})
			if err != nil {
				return fmt.Errorf("upsert org membership: %w", err)
			}

			keep = append(keep, orgID)
		}

		// The second, redundant fail-closed guard. Resolve already refuses an empty
		// result, so reaching here with an empty keep-set would mean a bug -- and
		// the consequence of that bug is an irreversible cascade delete of the
		// user's project roles and API keys. It is worth one `if`.
		if len(keep) == 0 {
			return fmt.Errorf("%w: refusing to reconcile %s to zero organizations", ErrLoginNotAllowed, id.Subject)
		}

		// Removes exactly the memberships the IdP no longer asserts. This is the
		// revocation half of the design: it cascades org_memberships ->
		// project_memberships -> api_keys, so a user dropped from an OIDC group
		// loses every key they hold anywhere in that org, in one statement. That
		// cascade is what makes a join-free API key grant safe to trust.
		if _, err := q.ReconcileOrgMembershipsRemove(ctx, repository.ReconcileOrgMembershipsRemoveParams{
			UserID: user.ID,
			Keep:   keep,
		}); err != nil {
			return fmt.Errorf("reconcile org memberships: %w", err)
		}

		return nil
	})
	if err != nil {
		return pgtype.UUID{}, err
	}

	return userID, nil
}

// EnsureOrgs creates every organization the group mapping names. Call it at boot.
//
// Without it the system deadlocks on day one: the mapping file names an org, no
// org row exists, so Resolve maps the first user to zero orgs and the login is
// refused -- and the only way to create an org is through the API, which requires
// a logged-in site admin. The mapping file IS the declaration that these orgs
// exist; this makes the database agree.
func (s *Service) EnsureOrgs(ctx context.Context) error {
	if s.groups == nil {
		return nil
	}

	_, err := s.ensureOrgs(ctx, s.groups.OrgSlugs())

	return err
}

// ensureOrgs resolves each slug to an org id, creating the org if it is absent.
func (s *Service) ensureOrgs(ctx context.Context, slugs []string) (map[string]pgtype.UUID, error) {
	out := make(map[string]pgtype.UUID, len(slugs))

	for _, slug := range slugs {
		id, err := s.ensureOrg(ctx, slug)
		if err != nil {
			return nil, err
		}

		out[slug] = id
	}

	return out, nil
}

func (s *Service) ensureOrg(ctx context.Context, slug string) (pgtype.UUID, error) {
	org, err := s.store.GetOrganizationBySlug(ctx, slug)
	if err == nil {
		return org.ID, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("look up organization %q: %w", slug, err)
	}

	created, err := s.store.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: slug,
		Name: slug,
	})
	if err == nil {
		return created.ID, nil
	}

	// Another login created it between our SELECT and our INSERT. That is the
	// expected outcome of a concurrent first login, not an error.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
		org, err := s.store.GetOrganizationBySlug(ctx, slug)
		if err != nil {
			return pgtype.UUID{}, fmt.Errorf("re-read organization %q after a concurrent create: %w", slug, err)
		}

		return org.ID, nil
	}

	return pgtype.UUID{}, fmt.Errorf("create organization %q: %w", slug, err)
}

// loadPrincipal reads every authorization fact for a user, live, from the tables.
//
// Live, not cached in the session: a demotion or an org removal then takes effect
// on the user's very next console request, with no session-invalidation machinery
// at all. This is a cold path -- the console, not the cache -- and it can afford
// three queries. (API keys make the opposite trade and must: see apikey.go.)
func (s *Service) loadPrincipal(ctx context.Context, userID pgtype.UUID, method Method) (Principal, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}

	orgRows, err := s.store.ListOrgMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load org memberships: %w", err)
	}

	projectRows, err := s.store.ListProjectMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load project memberships: %w", err)
	}

	orgs := make(map[pgtype.UUID]OrgRole, len(orgRows))
	for _, row := range orgRows {
		orgs[row.OrgID] = row.Role
	}

	projects := make(map[pgtype.UUID]ProjectRole, len(projectRows))
	for _, row := range projectRows {
		projects[row.ProjectID] = row.Role
	}

	return &principal{
		userID:      user.ID,
		issuer:      user.Issuer,
		subject:     user.Subject,
		email:       user.Email,
		displayName: user.DisplayName,
		method:      method,
		siteRole:    user.SiteRole,
		orgs:        orgs,
		projects:    projects,
		key:         nil,
	}, nil
}

func slugsOf(orgs map[string]config.OrgRole) []string {
	out := make([]string, 0, len(orgs))
	for slug := range orgs {
		out = append(out, slug)
	}

	return out
}

// siteRole and orgRole bridge internal/config's role vocabulary (parsed from the
// mapping FILE) to the schema's enums. Both sets are validated at parse time, so
// an unknown value here is impossible -- but the default is the LEAST privileged
// value in each, so if that ever changes the failure is a denial, not an
// escalation.
func siteRole(r config.SiteRole) SiteRole {
	if r == config.SiteRoleAdmin {
		return SiteRoleAdmin
	}

	return SiteRoleUser
}

func orgRole(r config.OrgRole) OrgRole {
	switch r {
	case config.OrgRoleOwner:
		return OrgRoleOwner
	case config.OrgRoleAdmin:
		return OrgRoleAdmin
	case config.OrgRoleMember:
		return OrgRoleMember
	}

	return OrgRoleMember
}
