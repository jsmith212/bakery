package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

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
		return errSlug("slug", err)
	}

	if req.Name == "" {
		req.Name = req.Slug
	}

	project, err := a.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: s.OrgID, Slug: req.Slug, Name: req.Name,
	})
	if err != nil {
		return fmt.Errorf("create project %q: %w", req.Slug, err)
	}

	writeJSON(w, http.StatusCreated, newProject(project, s.OrgSlug, nil, p))

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
