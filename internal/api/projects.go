package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/slug"
)

// CreateProjectRequest creates a project inside an org.
type CreateProjectRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// UpdateProjectRequest renames a project. As with orgs, the slug is immutable --
// it is the second path segment of every cache URL.
type UpdateProjectRequest struct {
	Name string `json:"name"`
}

// handleListProjects lists an org's projects.
//
// Every project member is necessarily an org member (the composite foreign key
// from project_memberships into org_memberships makes that a fact the database
// enforces), and any org membership implies read on every project in the org. So
// once the guard has established CanViewOrg, there is no project in this list the
// caller may not see -- the filter below is belt-and-braces against a future
// policy change, not dead weight.
func (a *API) handleListProjects(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	projects, err := a.store.ListProjectsForOrg(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	out := make([]Project, 0, len(projects))

	for _, pr := range projects {
		if !p.CanReadProject(s.OrgID, pr.ID) {
			continue
		}

		kinds, err := a.backendKinds(ctx, pr.ID)
		if err != nil {
			return err
		}

		out = append(out, newProject(pr, s.OrgSlug, kinds, p))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// backendKinds lists the configured backend kinds for a project.
//
// This is a query per project on the list endpoint. That is a deliberate N+1 and
// it is fine: this is a cold, human-speed console page, projects per org are a
// handful, and the alternative -- a bespoke join query -- would mean adding a
// query file, which is not this package's to own. If an org ever grows to hundreds
// of projects, replace it with one `GROUP BY project_id` query. It has never been
// on a cache path.
func (a *API) backendKinds(ctx context.Context, projectID pgtype.UUID) ([]string, error) {
	backends, err := a.store.ListBackendsForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list backends: %w", err)
	}

	kinds := make([]string, 0, len(backends))
	for _, b := range backends {
		kinds = append(kinds, string(b.Kind))
	}

	return kinds, nil
}

// handleCreateProject creates a project. Org admin (AccessOrgAdmin).
func (a *API) handleCreateProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var req CreateProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Slug = strings.TrimSpace(req.Slug)
	req.Name = strings.TrimSpace(req.Name)

	// The same reserved-slug rule as orgs, and for the same reason: a project named
	// `cas` would make /cache/{org}/cas/... ambiguous with the Bazel CAS namespace,
	// and `sstate`, `ac` and `blobs` are all live segments of the cache grammar.
	if err := slug.Check(req.Slug); err != nil {
		return errSlug("slug", req.Slug, err)
	}

	if req.Name == "" {
		req.Name = req.Slug
	}

	// The project and the creator's ADMIN role on it are ONE transaction, for the
	// same reason org creation and its owner grant are.
	//
	// The creator can already administer the project by virtue of their org role --
	// but they cannot mint a KEY for it, because api_keys carries
	// `FOREIGN KEY (user_id, project_id) REFERENCES project_memberships`: a key for a
	// non-member cannot exist, and CreateAPIKey refuses with `scope_exceeds_role`
	// before the database gets the chance to. So "create org -> create project ->
	// mint a key" dead-ended one step further along than the org grant alone fixes,
	// and this is the other half of it.
	//
	// The grant is guarded in SQL on the creator actually being an org member, so a
	// SITE ADMIN creating a project in an org they do not belong to gets a no-op
	// rather than a foreign-key 500. Nobody else can hit that path: creating an org
	// now makes you a member of it.
	var project repository.Project

	err := a.store.Tx(ctx, func(q *repository.Queries) error {
		created, err := q.CreateProject(ctx, repository.CreateProjectParams{
			OrgID: s.OrgID, Slug: req.Slug, Name: req.Name,
		})
		if err != nil {
			return fmt.Errorf("create project %q: %w", req.Slug, err)
		}

		if _, err := q.GrantProjectMembershipToCreator(ctx,
			repository.GrantProjectMembershipToCreatorParams{
				UserID: p.UserID(), ProjectID: created.ID,
			}); err != nil {
			return fmt.Errorf("grant the creator the project admin role: %w", err)
		}

		project = created

		return nil
	})
	if err != nil {
		return err
	}

	out := newProject(project, s.OrgSlug, nil, p)

	// As with org creation: the principal was built before this project existed, so
	// it cannot know the caller's role in it. The grant above is what makes `admin`
	// true -- unless the caller is a site admin who is not an org member, in which
	// case the grant was a no-op and reporting a role they do not hold would be a lie
	// the very next request contradicts.
	if _, isMember := p.OrgRole(s.OrgID); isMember {
		out.Role = string(auth.ProjectRoleAdmin)
	}

	writeJSON(w, http.StatusCreated, out)

	return nil
}

// handleGetProject reads one project.
func (a *API) handleGetProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	// s.ProjectID came from the guard's ResolveRoute of (org slug, project slug) --
	// so it is, by construction, a project inside the org the caller was authorized
	// against. A handler that took an id from the path could be handed any project
	// in the installation.
	project, err := a.store.GetProject(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}

	kinds, err := a.backendKinds(ctx, s.ProjectID)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, newProject(project, s.OrgSlug, kinds, p))

	return nil
}

// handleUpdateProject renames a project. Project admin or org admin.
func (a *API) handleUpdateProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var req UpdateProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	project, err := a.store.UpdateProject(ctx, repository.UpdateProjectParams{
		ID: s.ProjectID, Name: req.Name,
	})
	if err != nil {
		return fmt.Errorf("update project: %w", err)
	}

	kinds, err := a.backendKinds(ctx, s.ProjectID)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, newProject(project, s.OrgSlug, kinds, p))

	return nil
}

// handleDeleteProject deletes a project. ORG admin, not project admin
// (AccessOrgAdmin): this destroys every cache object, key and backend config in
// the project, and a project admin is a role the org's admins hand out. Letting
// the recipient of a delegated role destroy the thing it was delegated over is
// the wrong default.
func (a *API) handleDeleteProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	n, err := a.store.DeleteProject(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}

	if n == 0 {
		return errNotFound("project not found")
	}

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}
