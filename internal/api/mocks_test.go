package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Shared fakes. Per-endpoint fakes live in the test file that uses them (kbi's
// convention); this file holds only what more than one test file needs.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()

	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}

	return u
}

// The fixture graph: org `acme` holds project `firmware`; org `other` is a
// different tenant entirely, and is what proves a caller cannot reach across.
const (
	orgAcmeID      = "11111111-1111-1111-1111-111111111111"
	orgOtherID     = "22222222-2222-2222-2222-222222222222"
	projFirmwareID = "33333333-3333-3333-3333-333333333333"
	projOtherID    = "44444444-4444-4444-4444-444444444444"

	userAnnaID  = "55555555-5555-5555-5555-555555555555"
	userMarkoID = "66666666-6666-6666-6666-666666666666"

	keyAnnaID  = "77777777-7777-7777-7777-777777777777"
	keyMarkoID = "88888888-8888-8888-8888-888888888888"
)

// ---------------------------------------------------------------------------
// fakePrincipal
// ---------------------------------------------------------------------------

// fakePrincipal implements api.Principal -- the CONSUMER-SIDE interface -- and
// emphatically NOT auth.Principal, which is sealed and cannot be implemented
// outside internal/auth. It exists so the authorization matrix can be driven for
// every role without a live OIDC provider.
//
// It duplicates the real capability logic from internal/auth rather than calling
// it, which is the honest trade: these tests assert that THE API asks the right
// question of the principal, at the right route, in the right order. That the
// principal answers correctly is internal/auth's own test's job, and it has one.
type fakePrincipal struct {
	userID      pgtype.UUID
	email       string
	displayName string
	method      auth.Method

	siteRole auth.SiteRole
	orgs     map[pgtype.UUID]auth.OrgRole
	projects map[pgtype.UUID]auth.ProjectRole

	key *auth.KeyGrant
}

var _ Principal = (*fakePrincipal)(nil)

func (p *fakePrincipal) UserID() pgtype.UUID     { return p.userID }
func (p *fakePrincipal) Email() string           { return p.email }
func (p *fakePrincipal) DisplayName() string     { return p.displayName }
func (p *fakePrincipal) Method() auth.Method     { return p.method }
func (p *fakePrincipal) SiteRole() auth.SiteRole { return p.siteRole }

func (p *fakePrincipal) IsSiteAdmin() bool {
	return p.method != auth.MethodAPIKey && p.siteRole == auth.SiteRoleAdmin
}

func (p *fakePrincipal) OrgRole(orgID pgtype.UUID) (auth.OrgRole, bool) {
	r, ok := p.orgs[orgID]

	return r, ok
}

func (p *fakePrincipal) ProjectRole(projectID pgtype.UUID) (auth.ProjectRole, bool) {
	r, ok := p.projects[projectID]

	return r, ok
}

func (p *fakePrincipal) APIKey() (auth.KeyGrant, bool) {
	if p.key == nil {
		return auth.KeyGrant{}, false
	}

	return *p.key, true
}

func (p *fakePrincipal) CanViewOrg(orgID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	_, ok := p.orgs[orgID]

	return ok
}

func (p *fakePrincipal) CanAdminOrg(orgID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	r, ok := p.orgs[orgID]

	return ok && (r == auth.OrgRoleAdmin || r == auth.OrgRoleOwner)
}

func (p *fakePrincipal) CanOwnOrg(orgID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	r, ok := p.orgs[orgID]

	return ok && r == auth.OrgRoleOwner
}

func (p *fakePrincipal) CanReadProject(orgID, projectID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return p.key != nil && p.key.ProjectID == projectID
	}

	if p.IsSiteAdmin() {
		return true
	}

	if _, ok := p.orgs[orgID]; ok {
		return true
	}

	_, ok := p.projects[projectID]

	return ok
}

func (p *fakePrincipal) CanWriteProject(orgID, projectID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return p.key != nil && p.key.ProjectID == projectID && p.key.Scope == auth.ScopeWrite
	}

	if p.CanAdminOrg(orgID) {
		return true
	}

	r, ok := p.projects[projectID]

	return ok && (r == auth.ProjectRoleWriter || r == auth.ProjectRoleAdmin)
}

func (p *fakePrincipal) CanAdminProject(orgID, projectID pgtype.UUID) bool {
	if p.method == auth.MethodAPIKey {
		return false
	}

	if p.CanAdminOrg(orgID) {
		return true
	}

	r, ok := p.projects[projectID]

	return ok && r == auth.ProjectRoleAdmin
}

// ---------------------------------------------------------------------------
// fakeStore
// ---------------------------------------------------------------------------

// fakeStore is a hand-written in-memory Store. No gomock, no testify: the
// interface is small enough that a struct with slices and a `desiredErr` reads
// better than any generated double, and it lets a test assert on what was WRITTEN
// (see calls) rather than only on what was returned.
type fakeStore struct {
	orgs     []repository.Organization
	projects []repository.Project
	backends []repository.CacheBackend
	users    []repository.User
	keys     []repository.ListAPIKeysForProjectRow

	orgMembers     map[pgtype.UUID][]repository.ListOrgMembersRow
	projectMembers map[pgtype.UUID][]repository.ListProjectMembersRow

	// calls records mutating calls, so a test can assert a denied request wrote
	// NOTHING -- a 403 that still performed the write is the failure mode a
	// status-code-only assertion cannot see.
	calls []string

	// revokedForMembership records RevokeAPIKeysForMembership calls, which is how
	// the downgrade-revokes-write-keys transaction is asserted.
	revokedForMembership []repository.RevokeAPIKeysForMembershipParams
	revokedKeys          []pgtype.UUID

	// orgMemberReads records every ListOrgMembers(orgID). Key-owner decoration must
	// resolve names through THIS (an org-scoped, bounded read), never through a
	// whole-users-table scan -- so a test can assert the lookup stayed org-scoped.
	orgMemberReads []pgtype.UUID

	desiredErr error
}

var _ Store = (*fakeStore)(nil)

func (s *fakeStore) note(name string) { s.calls = append(s.calls, name) }

func (s *fakeStore) CreateOrganization(
	_ context.Context, arg repository.CreateOrganizationParams,
) (repository.Organization, error) {
	s.note("CreateOrganization:" + arg.Slug)

	if s.desiredErr != nil {
		return repository.Organization{}, s.desiredErr
	}

	org := repository.Organization{
		ID: uuidOf(arg.Slug), Slug: arg.Slug, Name: arg.Name,
		CreatedAt: pgtype.Timestamptz{}, UpdatedAt: pgtype.Timestamptz{},
	}
	s.orgs = append(s.orgs, org)

	return org, nil
}

func (s *fakeStore) GetOrganizationBySlug(_ context.Context, slug string) (repository.Organization, error) {
	for _, o := range s.orgs {
		if o.Slug == slug {
			return o, nil
		}
	}

	return repository.Organization{}, pgx.ErrNoRows
}

func (s *fakeStore) ListOrganizations(_ context.Context) ([]repository.Organization, error) {
	if s.desiredErr != nil {
		return nil, s.desiredErr
	}

	return s.orgs, nil
}

func (s *fakeStore) UpdateOrganization(
	_ context.Context, arg repository.UpdateOrganizationParams,
) (repository.Organization, error) {
	s.note("UpdateOrganization")

	for i := range s.orgs {
		if s.orgs[i].ID == arg.ID {
			s.orgs[i].Name = arg.Name

			return s.orgs[i], nil
		}
	}

	return repository.Organization{}, pgx.ErrNoRows
}

func (s *fakeStore) DeleteOrganization(_ context.Context, id pgtype.UUID) (int64, error) {
	s.note("DeleteOrganization")

	for i := range s.orgs {
		if s.orgs[i].ID == id {
			s.orgs = append(s.orgs[:i], s.orgs[i+1:]...)

			return 1, nil
		}
	}

	return 0, nil
}

func (s *fakeStore) CreateProject(
	_ context.Context, arg repository.CreateProjectParams,
) (repository.Project, error) {
	s.note("CreateProject:" + arg.Slug)

	if s.desiredErr != nil {
		return repository.Project{}, s.desiredErr
	}

	pr := repository.Project{
		ID: uuidOf(arg.Slug), OrgID: arg.OrgID, Slug: arg.Slug, Name: arg.Name,
		CreatedAt: pgtype.Timestamptz{}, UpdatedAt: pgtype.Timestamptz{},
	}
	s.projects = append(s.projects, pr)

	return pr, nil
}

func (s *fakeStore) GetProject(_ context.Context, id pgtype.UUID) (repository.Project, error) {
	for _, p := range s.projects {
		if p.ID == id {
			return p, nil
		}
	}

	return repository.Project{}, pgx.ErrNoRows
}

func (s *fakeStore) ListProjectsForOrg(_ context.Context, orgID pgtype.UUID) ([]repository.Project, error) {
	var out []repository.Project

	for _, p := range s.projects {
		if p.OrgID == orgID {
			out = append(out, p)
		}
	}

	return out, nil
}

func (s *fakeStore) UpdateProject(
	_ context.Context, arg repository.UpdateProjectParams,
) (repository.Project, error) {
	s.note("UpdateProject")

	for i := range s.projects {
		if s.projects[i].ID == arg.ID {
			s.projects[i].Name = arg.Name

			return s.projects[i], nil
		}
	}

	return repository.Project{}, pgx.ErrNoRows
}

func (s *fakeStore) DeleteProject(_ context.Context, id pgtype.UUID) (int64, error) {
	s.note("DeleteProject")

	for i := range s.projects {
		if s.projects[i].ID == id {
			s.projects = append(s.projects[:i], s.projects[i+1:]...)

			return 1, nil
		}
	}

	return 0, nil
}

func (s *fakeStore) ResolveRoute(
	_ context.Context, arg repository.ResolveRouteParams,
) (repository.ResolveRouteRow, error) {
	for _, o := range s.orgs {
		if o.Slug != arg.Slug {
			continue
		}

		for _, p := range s.projects {
			if p.OrgID == o.ID && p.Slug == arg.Slug_2 {
				return repository.ResolveRouteRow{ProjectID: p.ID, OrgID: o.ID}, nil
			}
		}
	}

	return repository.ResolveRouteRow{}, pgx.ErrNoRows
}

func (s *fakeStore) ListOrgMembers(
	_ context.Context, orgID pgtype.UUID,
) ([]repository.ListOrgMembersRow, error) {
	s.orgMemberReads = append(s.orgMemberReads, orgID)

	return s.orgMembers[orgID], nil
}

func (s *fakeStore) ListOrgMembershipsForUser(
	_ context.Context, userID pgtype.UUID,
) ([]repository.ListOrgMembershipsForUserRow, error) {
	var out []repository.ListOrgMembershipsForUserRow

	for orgID, members := range s.orgMembers {
		for _, m := range members {
			if m.UserID != userID {
				continue
			}

			for _, o := range s.orgs {
				if o.ID == orgID {
					out = append(out, repository.ListOrgMembershipsForUserRow{
						OrgID: orgID, Role: m.Role, Slug: o.Slug, Name: o.Name,
					})
				}
			}
		}
	}

	return out, nil
}

func (s *fakeStore) ListProjectMembers(
	_ context.Context, projectID pgtype.UUID,
) ([]repository.ListProjectMembersRow, error) {
	return s.projectMembers[projectID], nil
}

func (s *fakeStore) ListProjectMembershipsForUser(
	_ context.Context, userID pgtype.UUID,
) ([]repository.ListProjectMembershipsForUserRow, error) {
	var out []repository.ListProjectMembershipsForUserRow

	for projectID, members := range s.projectMembers {
		for _, m := range members {
			if m.UserID != userID {
				continue
			}

			for _, p := range s.projects {
				if p.ID != projectID {
					continue
				}

				for _, o := range s.orgs {
					if o.ID == p.OrgID {
						out = append(out, repository.ListProjectMembershipsForUserRow{
							ProjectID: projectID, Role: m.Role,
							ProjectSlug: p.Slug, OrgSlug: o.Slug,
						})
					}
				}
			}
		}
	}

	return out, nil
}

func (s *fakeStore) GetProjectMembership(
	_ context.Context, arg repository.GetProjectMembershipParams,
) (repository.ProjectMembership, error) {
	for _, m := range s.projectMembers[arg.ProjectID] {
		if m.UserID == arg.UserID {
			return repository.ProjectMembership{
				UserID: m.UserID, ProjectID: arg.ProjectID, Role: m.Role,
			}, nil
		}
	}

	return repository.ProjectMembership{}, pgx.ErrNoRows
}

func (s *fakeStore) DeleteProjectMembership(
	_ context.Context, arg repository.DeleteProjectMembershipParams,
) (int64, error) {
	s.note("DeleteProjectMembership")

	members := s.projectMembers[arg.ProjectID]
	for i := range members {
		if members[i].UserID == arg.UserID {
			s.projectMembers[arg.ProjectID] = append(members[:i], members[i+1:]...)

			return 1, nil
		}
	}

	return 0, nil
}

func (s *fakeStore) ListAPIKeysForProject(
	_ context.Context, projectID pgtype.UUID,
) ([]repository.ListAPIKeysForProjectRow, error) {
	var out []repository.ListAPIKeysForProjectRow

	for _, k := range s.keys {
		if k.ProjectID == projectID {
			out = append(out, k)
		}
	}

	return out, nil
}

func (s *fakeStore) RevokeAPIKey(_ context.Context, id pgtype.UUID) (int64, error) {
	s.note("RevokeAPIKey")
	s.revokedKeys = append(s.revokedKeys, id)

	return 1, nil
}

func (s *fakeStore) ListBackendsForProject(
	_ context.Context, projectID pgtype.UUID,
) ([]repository.CacheBackend, error) {
	var out []repository.CacheBackend

	for _, b := range s.backends {
		if b.ProjectID == projectID {
			out = append(out, b)
		}
	}

	return out, nil
}

func (s *fakeStore) CreateBackend(
	_ context.Context, arg repository.CreateBackendParams,
) (repository.CacheBackend, error) {
	s.note("CreateBackend:" + string(arg.Kind))

	if s.desiredErr != nil {
		return repository.CacheBackend{}, s.desiredErr
	}

	b := repository.CacheBackend{
		ID: int64(len(s.backends) + 1), ProjectID: arg.ProjectID, Kind: arg.Kind,
		Enabled: arg.Enabled, ReadAuthRequired: arg.ReadAuthRequired, Config: arg.Config,
	}
	s.backends = append(s.backends, b)

	return b, nil
}

func (s *fakeStore) UpdateBackend(
	_ context.Context, arg repository.UpdateBackendParams,
) (repository.CacheBackend, error) {
	s.note("UpdateBackend")

	for i := range s.backends {
		if s.backends[i].ID == arg.ID {
			s.backends[i].Enabled = arg.Enabled
			s.backends[i].ReadAuthRequired = arg.ReadAuthRequired
			s.backends[i].Config = arg.Config

			return s.backends[i], nil
		}
	}

	return repository.CacheBackend{}, pgx.ErrNoRows
}

func (s *fakeStore) DeleteBackend(_ context.Context, id int64) (int64, error) {
	s.note("DeleteBackend")

	if s.desiredErr != nil {
		return 0, s.desiredErr
	}

	for i := range s.backends {
		if s.backends[i].ID == id {
			s.backends = append(s.backends[:i], s.backends[i+1:]...)

			return 1, nil
		}
	}

	return 0, nil
}

// Tx cannot rebind a *repository.Queries onto a fake -- Queries is a concrete
// generated struct over a real DBTX. So the fake refuses, and every test that
// exercises the transactional path (the project-role downgrade) is DB-BACKED
// instead, against a real Postgres. That is the right way round: the whole point
// of that transaction is atomicity, which a fake cannot demonstrate.
var errFakeTx = errors.New("fakeStore: Tx is not implemented; use a DB-backed test")

func (s *fakeStore) Tx(_ context.Context, _ func(*repository.Queries) error) error {
	s.note("Tx")

	return errFakeTx
}

// uuidOf derives a stable UUID from a slug, so a fake create can hand back an id.
func uuidOf(seed string) pgtype.UUID {
	var u pgtype.UUID
	u.Valid = true

	for i := range len(seed) {
		u.Bytes[i%16] ^= seed[i]
	}

	return u
}

// ---------------------------------------------------------------------------
// fakeMinter
// ---------------------------------------------------------------------------

// fakeMinter stands in for auth.Service's key minting, which cannot be driven from
// here: CreateAPIKey takes an auth.Principal, and a test cannot construct one --
// that is the sealed-Principal invariant working. The real adapter
// (serviceKeyMinter) is exercised by the DB-backed test, which gets a genuine
// Principal by driving dev-login end to end.
type fakeMinter struct {
	token string
	err   error

	got auth.CreateKeyInput
}

var _ keyMinter = (*fakeMinter)(nil)

func (m *fakeMinter) CreateAPIKey(
	_ context.Context, p Principal, in auth.CreateKeyInput,
) (auth.NewAPIKey, repository.CreateAPIKeyRow, error) {
	m.got = in

	if m.err != nil {
		return auth.NewAPIKey{}, repository.CreateAPIKeyRow{}, m.err
	}

	return auth.NewAPIKey{Token: m.token, Prefix: "bkry_abcd1234", Hash: nil},
		repository.CreateAPIKeyRow{
			ID: uuidOf(in.Name), UserID: p.UserID(), ProjectID: in.ProjectID,
			Name: in.Name, TokenPrefix: "bkry_abcd1234", Scope: in.Scope,
		}, nil
}
