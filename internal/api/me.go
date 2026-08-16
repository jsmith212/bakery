package api

import (
	"fmt"
	"net/http"
)

// handleMe returns the current principal: identity, site role, org roles, project
// roles, and (if the request authenticated with a key) that key's grant.
//
// This is the console's boot call. Note it reports authorization, it does not
// perform it: hiding a button here changes nothing about what the server will
// accept, and every endpoint re-checks.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	// A ROBOT IS ANSWERED FIRST, before any user read.
	//
	// It has no row in `users` -- that is the whole point of robots being separate
	// tables -- so p.UserID() is the zero UUID and the GetUser below would 404 a
	// perfectly valid credential. There is nothing to look up and nothing to
	// report but the grant itself.
	if grant, isRobot := p.Robot(); isRobot {
		writeJSON(w, http.StatusOK, Me{
			UserID: "", Email: "", DisplayName: "", AvatarURL: "",
			Method:      string(p.Method()),
			SiteRole:    string(p.SiteRole()),
			IsSiteAdmin: false,
			Orgs:        []MeOrg{}, Projects: []MeProject{},
			APIKey: nil,
			Robot: &MeRobotGrant{
				RobotID: uuidString(grant.RobotID),
				OrgID:   uuidString(grant.OrgID),
				Scope:   string(grant.Scope),
			},
		})

		return nil
	}

	// AvatarURL is read from the user row directly, not from the Principal
	// interface: `auth.Principal` is sealed and its whole vocabulary is
	// authorization (see forgery/) -- a rendering-only field has no business
	// joining it. handleMe already owns a store and already loads the user
	// implicitly via p.UserID(), so one more read here keeps the authz type
	// unchanged.
	user, err := a.store.GetUser(ctx, p.UserID())
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}

	me := Me{
		UserID:      uuidString(p.UserID()),
		Email:       p.Email(),
		DisplayName: p.DisplayName(),
		AvatarURL:   user.AvatarURL.String,
		Method:      string(p.Method()),
		SiteRole:    string(p.SiteRole()),
		IsSiteAdmin: p.IsSiteAdmin(),
		Orgs:        []MeOrg{},
		Projects:    []MeProject{},
		APIKey:      nil,
		Robot:       nil,
	}

	if grant, isKey := p.APIKey(); isKey {
		me.APIKey = &MeKeyGrant{
			KeyID:     uuidString(grant.KeyID),
			ProjectID: uuidString(grant.ProjectID),
			Scope:     string(grant.Scope),
		}

		// A key principal has no org or project memberships to report -- it has one
		// grant, and that is the whole of its authority. Listing the OWNER's orgs
		// here would be a lie about what this credential can do.
		writeJSON(w, http.StatusOK, me)

		return nil
	}

	orgs, err := a.store.ListOrgMembershipsForUser(ctx, p.UserID())
	if err != nil {
		return fmt.Errorf("list org memberships: %w", err)
	}

	for _, o := range orgs {
		me.Orgs = append(me.Orgs, MeOrg{
			ID: uuidString(o.OrgID), Slug: o.Slug, Name: o.Name, Role: string(o.Role),
		})
	}

	projects, err := a.store.ListProjectMembershipsForUser(ctx, p.UserID())
	if err != nil {
		return fmt.Errorf("list project memberships: %w", err)
	}

	for _, pr := range projects {
		me.Projects = append(me.Projects, MeProject{
			ID: uuidString(pr.ProjectID), Slug: pr.ProjectSlug,
			OrgSlug: pr.OrgSlug, Role: string(pr.Role),
		})
	}

	writeJSON(w, http.StatusOK, me)

	return nil
}
