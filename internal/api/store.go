package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Store is the control-plane repository, declared CONSUMER-SIDE: it lists exactly
// the queries this package uses and nothing else. *db.Store satisfies it.
//
// This is kbi's pattern and it earns its keep twice. It keeps mocks_test.go a
// hand-written fake with no framework, and it makes the blast radius of this
// package legible -- there is no way for a control-plane handler to reach a blob
// or an object query, because those methods are not on this interface.
type Store interface {
	// Organizations.
	CreateOrganization(ctx context.Context, arg repository.CreateOrganizationParams) (repository.Organization, error)
	GetOrganizationBySlug(ctx context.Context, slug string) (repository.Organization, error)
	ListOrganizations(ctx context.Context) ([]repository.Organization, error)
	UpdateOrganization(ctx context.Context, arg repository.UpdateOrganizationParams) (repository.Organization, error)
	DeleteOrganization(ctx context.Context, id pgtype.UUID) (int64, error)

	// Projects.
	CreateProject(ctx context.Context, arg repository.CreateProjectParams) (repository.Project, error)
	GetProject(ctx context.Context, id pgtype.UUID) (repository.Project, error)
	ListProjectsForOrg(ctx context.Context, orgID pgtype.UUID) ([]repository.Project, error)
	UpdateProject(ctx context.Context, arg repository.UpdateProjectParams) (repository.Project, error)
	DeleteProject(ctx context.Context, id pgtype.UUID) (int64, error)
	ResolveRoute(ctx context.Context, arg repository.ResolveRouteParams) (repository.ResolveRouteRow, error)

	// Memberships.
	ListOrgMembers(ctx context.Context, orgID pgtype.UUID) ([]repository.ListOrgMembersRow, error)
	ListOrgMembershipsForUser(
		ctx context.Context, userID pgtype.UUID,
	) ([]repository.ListOrgMembershipsForUserRow, error)
	ListProjectMembers(ctx context.Context, projectID pgtype.UUID) ([]repository.ListProjectMembersRow, error)
	ListProjectMembershipsForUser(
		ctx context.Context, userID pgtype.UUID,
	) ([]repository.ListProjectMembershipsForUserRow, error)
	GetProjectMembership(
		ctx context.Context, arg repository.GetProjectMembershipParams,
	) (repository.ProjectMembership, error)
	DeleteProjectMembership(ctx context.Context, arg repository.DeleteProjectMembershipParams) (int64, error)

	// Users. ListUsers is how an email in an invite becomes a user id; users are
	// JIT-provisioned at first login, so a member must already have signed in once.
	ListUsers(ctx context.Context) ([]repository.User, error)

	// API keys. Note what is ABSENT: there is no query that returns a token or a
	// hash, because the schema has no plaintext column and this interface does not
	// expose token_sha256. "Shown exactly once" is not a discipline here, it is a
	// shape.
	ListAPIKeysForProject(
		ctx context.Context, projectID pgtype.UUID,
	) ([]repository.ListAPIKeysForProjectRow, error)
	RevokeAPIKey(ctx context.Context, id pgtype.UUID) (int64, error)

	// Cache backends.
	GetBackend(ctx context.Context, arg repository.GetBackendParams) (repository.GetBackendRow, error)
	ListBackendsForProject(ctx context.Context, projectID pgtype.UUID) ([]repository.CacheBackend, error)
	CreateBackend(ctx context.Context, arg repository.CreateBackendParams) (repository.CacheBackend, error)
	UpdateBackend(ctx context.Context, arg repository.UpdateBackendParams) (repository.CacheBackend, error)
	DeleteBackend(ctx context.Context, id int64) (int64, error)

	// Tx is required, not optional. A project-role DOWNGRADE must revoke the keys
	// that now exceed the role IN THE SAME TRANSACTION as the downgrade -- key
	// validation deliberately does not join project_memberships (that would put a
	// second probe on the sstate HEAD storm), so a key's scope is only ever capped
	// at grant time and re-capped here. Two statements outside a transaction leave
	// a window in which a demoted user still holds a write key.
	Tx(ctx context.Context, fn func(*repository.Queries) error) error
}

// *db.Store must satisfy Store. If a query signature changes under us, this fails
// to compile rather than at the first request.
var _ Store = (*db.Store)(nil)

// keyMinter mints an API key. It is an interface so the show-once semantics can be
// tested against a fake, and so this package states its one privileged dependency
// explicitly.
type keyMinter interface {
	CreateAPIKey(
		ctx context.Context, p Principal, in auth.CreateKeyInput,
	) (auth.NewAPIKey, repository.CreateAPIKeyRow, error)
}

// errNotVerified is returned when something that is not a real, auth-issued
// Principal reaches the minter.
var errNotVerified = errors.New("api: an API key can only be minted for a verified principal")

// serviceKeyMinter adapts *auth.Service, whose CreateAPIKey takes an
// auth.Principal, onto this package's api.Principal.
//
// The type assertion is the interesting line. api.Principal is a strictly wider
// interface -- a test fake can implement it -- so the adapter demands the real
// thing back, and refuses anything else. A forged Principal therefore cannot mint
// a credential: it can, at most, be denied. That is the fail-closed direction, and
// it is the reason the wider interface is safe to have at all.
type serviceKeyMinter struct{ svc *auth.Service }

func (m serviceKeyMinter) CreateAPIKey(
	ctx context.Context, p Principal, in auth.CreateKeyInput,
) (auth.NewAPIKey, repository.CreateAPIKeyRow, error) {
	verified, ok := p.(auth.Principal)
	if !ok {
		return auth.NewAPIKey{}, repository.CreateAPIKeyRow{}, errNotVerified
	}

	key, row, err := m.svc.CreateAPIKey(ctx, verified, in)
	if err != nil {
		return auth.NewAPIKey{}, repository.CreateAPIKeyRow{}, err
	}

	return key, row, nil
}
