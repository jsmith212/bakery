package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jsmith212/bakery/internal/auth"
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

// handleCreateOrg creates an organization AND MAKES THE CREATOR ITS LOCAL OWNER,
// in one transaction. AccessUser -- any signed-in human, unless self-serve is off.
//
// # The dead-end this exists to abolish
//
// Creating an org used to grant no membership in it. There is no group mapping for
// an org that did not exist a second ago, and there could not be -- so the creator,
// site admin or not, was not a member of their own org, was therefore not a project
// member of anything in it, and the API-key scope cap (which is capped at the
// caller's PROJECT role at grant time, deliberately without a join, so the sstate
// HEAD storm stays a single index probe) refused them with `scope_exceeds_role`.
// They had created an org they could not use, and the only ways out were an LDAP
// round trip or a psql session.
//
// The grant is LOCAL -- local_role = owner, with granted_by and granted_at -- and
// never oidc_role: no group claim says this, and forging one would be a lie the
// next login would rightly reconcile away. The local half is exactly the half the
// reconciler cannot touch, so the grant survives every login the creator ever makes.
//
// # Why it is one transaction
//
// A crash between the INSERT and the grant leaves an org with no members. Nobody
// can ever join it: adding a member requires being an admin of the org, and there
// is no admin. It cannot be deleted either -- that requires being its owner. It is
// precisely the orphaned, unusable org this milestone exists to abolish, and it
// would be created by the very code meant to abolish it. So both statements go
// through store.Tx and the org is not created at all unless its owner is.
func (a *API) handleCreateOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	// The guard has already refused an API-key principal (AccessUser is not
	// AccessAuthenticated), which matters here more than anywhere: this endpoint
	// hands its caller ownership of a fresh tenant, and a key that could reach it
	// would be a delegation that had become a master key.
	if !a.allowSelfServeOrgs && !p.IsSiteAdmin() {
		return errForbidden("creating an organization is restricted to site administrators " +
			"on this installation")
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

	var org repository.Organization

	err := a.store.Tx(ctx, func(q *repository.Queries) error {
		created, err := q.CreateOrganization(ctx, repository.CreateOrganizationParams{
			Slug: req.Slug, Name: req.Name,
		})
		if err != nil {
			return fmt.Errorf("create organization %q: %w", req.Slug, err)
		}

		if _, err := q.GrantOrgMembershipLocal(ctx, repository.GrantOrgMembershipLocalParams{
			UserID:    p.UserID(),
			OrgID:     created.ID,
			LocalRole: repository.NullOrgRole{OrgRole: auth.OrgRoleOwner, Valid: true},
			GrantedBy: p.UserID(),
		}); err != nil {
			return fmt.Errorf("grant the creator a local owner role: %w", err)
		}

		org = created

		return nil
	})
	if err != nil {
		return err
	}

	a.log.InfoContext(ctx, "created an organization; the creator is its local owner",
		slog.String("org", org.Slug),
		slog.String("owner", p.Email()),
	)

	// newOrg reports the CALLER's role in the org, and it reads it from the
	// principal -- which was built at authentication time, before this org existed.
	// So it would say "" here, and the console would render a brand-new org the
	// creator apparently cannot administer. The grant above is what makes owner the
	// true answer; say so.
	created := newOrg(org, p)
	created.Role = string(auth.OrgRoleOwner)

	writeJSON(w, http.StatusCreated, created)

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
