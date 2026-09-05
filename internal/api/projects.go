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
		// A backend under teardown (000018) is not a CONFIGURED kind: its mount 404s,
		// its snippet is refused, and it is about to stop existing. This field is what
		// the projects screen renders per row as "what this project can serve", so a
		// torn-down kind listed here is an offer the router will not honour.
		if b.DeletingAt.Valid {
			continue
		}

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
		// Users first: the grant below writes a project_memberships row, and every
		// membership writer takes the users row before the membership row (see
		// lockUserFirst). A site admin whose grant is a no-op still takes it -- the
		// ordering rule is about the transaction, not about whether it writes.
		if err := lockUserFirst(ctx, q, p.UserID()); err != nil {
			return err
		}

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
		return a.slugConflict(ctx, s.OrgID, req.Slug, err)
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

// slugConflict re-reads a 23505 to tell two conflicts apart.
//
// The generic mapping is "that slug is already taken", which is true and, for a project
// being TORN DOWN, actively unhelpful: a deleting project is hidden from every listing
// for everyone, site admins included, so the admin who just hit this looks at the org's
// projects, sees nothing with that slug, and has no way to learn why the name is
// refused or whether waiting would help. It would. The slug frees up on its own the
// moment the GC finishes emptying the project's backends, and that is the only sentence
// that tells them what to do.
//
// The probe runs only on the conflict, never on the happy path, and a failure to answer
// it falls back to the generic message rather than turning a 409 into a 500.
func (a *API) slugConflict(
	ctx context.Context, orgID pgtype.UUID, slug string, err error,
) error {
	if !isPGCode(err, pgUniqueViolation) {
		return err
	}

	deleting, probeErr := a.store.ProjectSlugIsDeleting(ctx, repository.ProjectSlugIsDeletingParams{
		OrgID: orgID, Slug: slug,
	})
	if probeErr != nil || !deleting {
		return err
	}

	return errConflict(CodeConflict,
		"a project with this slug is being torn down; the slug frees up when that finishes")
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

// handleDeleteProject tears a project down. ORG admin, not project admin
// (AccessOrgAdmin): this destroys every cache object, key and backend config in
// the project, and a project admin is a role the org's admins hand out. Letting
// the recipient of a delegated role destroy the thing it was delegated over is
// the wrong default.
//
// TWO OUTCOMES, THE SAME SHAPE AS A BACKEND DELETE. Everything happens in ONE
// transaction, in ownership order:
//
//  1. lock the project's members (the cascade below deletes a project_memberships
//     row per member and fires the epoch trigger once each, so lockUserFirst's
//     ordering rule applies at the width of the roster);
//  2. mark every backend for teardown, so none of them is reachable a moment later
//     than the project is;
//  3. delete the ones that hold nothing, which is the whole set in the common case
//     -- a project configured by mistake;
//  4. if nothing is left, delete the project outright: 204, exactly as before;
//  5. otherwise mark the project and answer 202.
//
// One transaction and not five statements, because the intermediate states are all
// wrong: marked backends under a live project (the project keeps serving nothing),
// or a marked project whose backends are still live (routes that resolve to a
// project the console has already forgotten). A crash between any two of them would
// leave exactly that.
//
// BEFORE 000018 THIS ENDPOINT COULD NOT DELETE A USED PROJECT AT ALL. projects ->
// cache_backends is ON DELETE RESTRICT and so is cache_backends -> cache_objects,
// and the 23503 that came back was not even mapped -- a project with cached data
// failed with a generic 500-shaped error and no way forward.
//
// Once marked, the project is GONE from the API: ResolveRoute filters
// deleting_at IS NULL, and that one statement is both the cache route resolver's
// project probe and this API's own {project} guard, so every route carrying
// {project} 404s from here on. That is deliberate and it is why the 202 carries the
// project body -- it is the last response about this project anyone will get.
func (a *API) handleDeleteProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var (
		gone   bool
		marked repository.Project
	)

	err := a.store.Tx(ctx, func(q *repository.Queries) error {
		if err := q.LockProjectMemberUsersForAuthzUpdate(ctx, s.ProjectID); err != nil {
			return fmt.Errorf("lock the project's members before deleting it: %w", err)
		}

		// THEN THE PROJECT ROW ITSELF, FOR UPDATE, before anything is marked. Steps 2-5
		// below are all READ COMMITTED and none of them conflicts with a concurrent
		// POST .../backends: creating a backend takes only KEY SHARE on this row (its
		// foreign key's own lock) and MarkProjectDeleting takes NO KEY UPDATE, which KEY
		// SHARE does not block. So a backend inserted between step 2's mark and step 3's
		// count lands UNMARKED under a project that is then marked -- a row in no
		// worklist, whose project's teardown anti-join never sees zero backends. The
		// project sits in `deleting` forever: hidden from every listing, holding its slug,
		// with no API that can un-mark it. FOR UPDATE is the one row mode that conflicts
		// with KEY SHARE, so the create either finishes before this transaction reads the
		// set or waits and is marked by it.
		//
		// Users first, then this: the ordering rule is about the transaction as a whole,
		// and a project taken before its roster deadlocks against every other membership
		// writer. The GC's markStragglers heals a row that got past this anyway.
		if err := q.LockProjectForTeardown(ctx, s.ProjectID); err != nil {
			return fmt.Errorf("lock the project before deleting it: %w", err)
		}

		if _, err := q.MarkProjectBackendsDeleting(ctx, s.ProjectID); err != nil {
			return fmt.Errorf("mark the project's backends for teardown: %w", err)
		}

		if _, err := q.DeleteEmptyBackendsForProject(ctx, s.ProjectID); err != nil {
			return fmt.Errorf("delete the project's empty backends: %w", err)
		}

		remaining, err := q.ListBackendsForProject(ctx, s.ProjectID)
		if err != nil {
			return fmt.Errorf("list the project's remaining backends: %w", err)
		}

		if len(remaining) == 0 {
			deleted, err := q.DeleteProject(ctx, s.ProjectID)
			if err != nil {
				return fmt.Errorf("delete project: %w", err)
			}

			gone = deleted > 0

			return nil
		}

		marked, err = q.MarkProjectDeleting(ctx, s.ProjectID)
		if err != nil {
			return fmt.Errorf("mark project for teardown: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	if gone {
		writeJSON(w, http.StatusNoContent, nil)

		return nil
	}

	// A project that was neither deleted nor marked did not exist -- the guard
	// resolved it, so this is a concurrent delete, and "not found" is the truth.
	if !marked.DeletingAt.Valid {
		return errNotFound("project not found")
	}

	// No kinds: every backend is marked, so backendKinds would render an empty list
	// anyway, and the project is unresolvable from here on so nothing will re-read
	// it. Passing nil says that plainly instead of spending a query to prove it.
	writeJSON(w, http.StatusAccepted, newProject(marked, s.OrgSlug, nil, p))

	return nil
}
