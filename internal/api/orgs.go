package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/slug"
)

// CreateOrgRequest creates an organization.
type CreateOrgRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// UpdateOrgRequest renames an organization. The slug is NOT updatable: it is the
// first path segment of every cache URL, and a rename would silently break every
// configured BitBake, Bazel and Docker client pointed at it. Renaming is a
// delete-and-recreate, which is honest about the cost.
type UpdateOrgRequest struct {
	Name string `json:"name"`
}

// handleListOrgs lists the orgs the CALLER can see.
//
// The scoping is not a filter bolted on afterwards -- it is the whole endpoint. A
// site admin sees every org; anyone else sees exactly the orgs their OIDC groups
// put them in. There is no "list all and let the console hide some".
func (a *API) handleListOrgs(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	all, err := a.store.ListOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("list organizations: %w", err)
	}

	out := make([]Org, 0, len(all))

	for _, o := range all {
		if !p.CanViewOrg(o.ID) {
			continue
		}

		out = append(out, newOrg(o, p))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handleCreateOrg creates an organization. Site admin only (AccessSiteAdmin).
func (a *API) handleCreateOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	var req CreateOrgRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Slug = strings.TrimSpace(req.Slug)
	req.Name = strings.TrimSpace(req.Name)

	// The RESERVED-SLUG rule. `cache`, `api`, `v2`, `ac`, `cas`, `blobs`, ... are
	// the routing grammar itself: an org called `cache` would make
	// /cache/cache/{project}/... ambiguous, and an org called `v2` would collide
	// with BuildKit's registry prefix. slug.Check is a MIRROR of the database's
	// bakery_slug_ok CHECK -- it buys a readable 422 instead of a raw 23514, and it
	// is deliberately not the only guard: the CHECK still fires if this is ever
	// skipped, on any writer, including a psql session.
	if err := slug.Check(req.Slug); err != nil {
		return errSlug("slug", req.Slug, err)
	}

	if req.Name == "" {
		req.Name = req.Slug
	}

	org, err := a.store.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: req.Slug, Name: req.Name,
	})
	if err != nil {
		return fmt.Errorf("create organization %q: %w", req.Slug, err)
	}

	writeJSON(w, http.StatusCreated, newOrg(org, p))

	return nil
}

// handleGetOrg reads one org. The guard already resolved the slug and confirmed
// the caller may view it; a caller who may not gets a 404, not a 403, so the
// endpoint is not an organization-name oracle.
func (a *API) handleGetOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	// Note the id came from the guard's resolution, not from the path.
	org, err := a.store.GetOrganizationBySlug(ctx, s.OrgSlug)
	if err != nil {
		return fmt.Errorf("load organization: %w", err)
	}

	writeJSON(w, http.StatusOK, newOrg(org, p))

	return nil
}

// handleUpdateOrg renames an org. Org admin or site admin.
func (a *API) handleUpdateOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var req UpdateOrgRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	org, err := a.store.UpdateOrganization(ctx, repository.UpdateOrganizationParams{
		ID: s.OrgID, Name: req.Name,
	})
	if err != nil {
		return fmt.Errorf("update organization: %w", err)
	}

	writeJSON(w, http.StatusOK, newOrg(org, p))

	return nil
}

// handleDeleteOrg deletes an org. Owner or site admin (AccessOrgOwner).
//
// This cascades: projects, memberships, API keys, backend configs, and every
// cache object in the org. That is why it is owner-only and not admin.
func (a *API) handleDeleteOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	n, err := a.store.DeleteOrganization(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("delete organization: %w", err)
	}

	if n == 0 {
		return errNotFound("organization not found")
	}

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}
