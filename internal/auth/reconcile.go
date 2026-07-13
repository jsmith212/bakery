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
// their groups claim was UNREADABLE, or the login gate is configured and they are
// in none of its groups. It FAILS CLOSED.
//
// What it no longer means -- and this is the M1.5 change -- is "they resolved to
// zero orgs". A user whose orgs are all granted in-app legitimately resolves to
// zero CLAIM-derived orgs, and refusing them was the bug. The fail-closed rule did
// not go away; it moved to the sharper question:
//
//	"the IdP says this user is in zero groups"  -> an ANSWER. Admit.
//	"we could not read this user's groups"      -> NO answer. Refuse, write nothing.
//
// Azure AD replaces a >200-group claim with a `_claim_names` overage pointing at
// Graph, so the second happens to real, correctly-configured users. Reading it as
// the first NULLs every oidc_role, deletes every membership no local grant
// justifies, and cascades away the user's project roles and API keys. Re-adding the
// membership does not bring the keys back.
var ErrLoginNotAllowed = errors.New("auth: this account is not authorized to use Bakery")

// Reconcile JIT-provisions the user and reconciles the OIDC HALF of their site and
// org roles from the ID token's groups claim. It runs on EVERY login, and returns
// the user id.
//
// It owns exactly one of the two sources. It writes site_role_oidc, oidc_role and
// oidc_group; it NEVER writes site_role_local, local_role, granted_by, granted_at,
// or project_memberships. A local grant therefore survives a login structurally --
// because this function does not name the column that holds it -- and the effective
// role stays greatest(oidc, local), computed by the database, recomputed by nobody.
//
// The order below is the whole safety argument:
//
//  1. an unreadable claim is refused BEFORE the first write. Not rolled back after
//     one -- never written.
//  2. memberships nothing justifies are DELETED (cascading project roles and API
//     keys, which is the revocation half of the design).
//  3. memberships a local grant still justifies have only their OIDC half cleared.
//
// There is deliberately NO "is a group map configured?" check. s.groups is never
// nil -- auth.New normalizes an absent mapping file to an EMPTY GroupMap -- and an
// empty map resolves to the documented thing: empty login gate (admit any
// successful OIDC auth), no site admins, zero claim-derived orgs. Refusing here
// instead would break the one deployment the hybrid model exists for, the one that
// runs entirely on local grants, and it would be unrecoverable: no login means no
// user row means nothing for the CLI break-glass to promote.
//
// The fail-closed rule lives in the GroupsPresent check below, not in the presence
// of a file. An unreadable claim is refused whether or not a map is configured.
func (s *Service) Reconcile(ctx context.Context, id Identity) (pgtype.UUID, error) {
	// STEP 1, AND IT IS FIRST FOR A REASON. An unreadable groups claim is refused
	// here, before ensureOrgs, before the transaction, before a single row is
	// touched. "Reconcile and roll back on error" is NOT equivalent and is NOT
	// acceptable: the requirement is that a login we cannot authorize writes nothing
	// at all, and the only way to be sure of that is to not start.
	//
	// Resolve checks the same thing and would return ErrGroupsUnreadable. That is the
	// BACKSTOP, not the gate -- it is one refactor away from being called after a
	// write, and this check is not.
	if !id.GroupsPresent {
		return pgtype.UUID{}, fmt.Errorf(
			"%w: the groups claim for %s is unreadable (absent, or an overage reference), so we "+
				"do not know their groups and will not reconcile against a guess", ErrLoginNotAllowed, id.Subject)
	}

	// STEP 2, the login gate. Resolve refuses an unreadable claim (again -- see
	// above) and a user outside every login group. It no longer refuses a zero-org
	// resolution: that is a local-only user, and it is an ordinary state.
	//
	// GroupsPresent is what carries the distinction across this call, and it is the
	// only thing that does: `id.Groups` is empty in both the admissible case (the
	// IdP asserts zero groups) and the catastrophic one (we never read the claim),
	// so passing the slice alone would hand Resolve a lie it cannot detect.
	res, err := s.groups.Resolve(config.GroupsClaim{Groups: id.Groups, Present: id.GroupsPresent})
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
		// site_role_OIDC and site_oidc_group, never site_role_local: the local half of
		// a site role is granted in-app (or by the CLI break-glass) and MUST survive a
		// login, so this reconciler does not name the column that holds it. The group
		// is audit -- it is what lets the site-admin listing say `ldap: platform-admins`
		// beside `local: granted by jsmith`, which is the whole mitigation for a hybrid
		// site admin.
		user, err := q.UpsertUser(ctx, repository.UpsertUserParams{
			Issuer:      id.Issuer,
			Subject:     id.Subject,
			Email:       id.Email,
			DisplayName: id.DisplayName,
			SiteRole:    siteRole(res.SiteRole),
			SiteGroup:   pgtype.Text{String: res.SiteGroup, Valid: res.SiteGroup != ""},
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
				UserID:    user.ID,
				OrgID:     orgID,
				OidcRole:  repository.NullOrgRole{OrgRole: orgRole(role), Valid: true},
				OidcGroup: pgtype.Text{String: res.OrgGroups[slug], Valid: res.OrgGroups[slug] != ""},
			})
			if err != nil {
				return fmt.Errorf("upsert org membership: %w", err)
			}

			keep = append(keep, orgID)
		}

		// AN EMPTY KEEP-SET IS NOW LEGITIMATE, and the guard that used to refuse it is
		// gone on purpose. It was the fail-closed backstop from when zero orgs meant
		// "we could not authorize this login"; under the hybrid model zero CLAIM-derived
		// orgs is what a local-only user has, and refusing them is the bug. Its job
		// moved to the GroupsPresent check at the top -- which fires on "we do not know
		// your groups" rather than on "you have none", and which is the correct place
		// because it fires before any write.
		//
		// What makes the empty keep-set safe is not a guard, it is the two statements
		// below: neither can touch a local grant.

		// Deletes only what NOTHING justifies: dropped by the claims AND holding no
		// local grant. It cascades org_memberships -> project_memberships -> api_keys,
		// so a user dropped from an OIDC group loses every key they hold anywhere in
		// that org, in one statement. That cascade is what makes a join-free API key
		// grant safe to trust on the sstate HEAD storm.
		//
		// It runs BEFORE the clear, and it must: `role` is GENERATED NOT NULL AS
		// greatest(oidc_role, local_role), so clearing oidc_role on a row with no local
		// grant is a 23502. These are exactly the rows this statement has already
		// removed.
		if _, err := q.ReconcileOrgMembershipsDelete(ctx, repository.ReconcileOrgMembershipsDeleteParams{
			UserID: user.ID,
			Keep:   keep,
		}); err != nil {
			return fmt.Errorf("delete unjustified org memberships: %w", err)
		}

		// And for the rest -- dropped by the claims but held up by a LOCAL grant -- we
		// clear our half and leave theirs. The row survives, the effective role falls
		// back to the local grant, and the user's project roles and API keys survive
		// with it. They are still a member; an admin said so.
		if _, err := q.ReconcileOrgMembershipsClearOIDC(ctx, repository.ReconcileOrgMembershipsClearOIDCParams{
			UserID: user.ID,
			Keep:   keep,
		}); err != nil {
			return fmt.Errorf("clear the oidc half of locally-granted org memberships: %w", err)
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
//
// With no mapping file (or one that names no orgs) it ensures nothing, which is
// correct: those orgs are created in-app by a site admin.
func (s *Service) EnsureOrgs(ctx context.Context) error {
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
