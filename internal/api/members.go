package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// PutProjectMemberRequest grants or changes a project role.
type PutProjectMemberRequest struct {
	Role string `json:"role"` // reader|writer|admin
}

// PutOrgMemberRequest grants or changes an org role LOCALLY.
type PutOrgMemberRequest struct {
	Role string `json:"role"` // member|admin|owner
}

// ---------------------------------------------------------------------------
// Org memberships: HYBRID. An OIDC half the reconciler owns, a local half this
// API owns, and a database-generated effective role = greatest(oidc, local).
//
// The two halves never touch. internal/auth's reconciler names oidc_role and
// oidc_group and nothing else; the handlers below name local_role, granted_by and
// granted_at and nothing else. Neither can clobber the other, and that is
// structural -- neither statement names the other's columns -- rather than a
// convention someone has to remember.
//
// (In M1 this endpoint was a 409: org roles were 100% claim-derived, so a
// hand-edit would have been silently reverted at the user's next login. The hybrid
// columns are precisely what make a local grant survive that login, which is what
// makes this endpoint honest to offer.)
// ---------------------------------------------------------------------------

// handleListOrgMembers lists an org's members, WITH the provenance of each role.
func (a *API) handleListOrgMembers(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.ListOrgMembers(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list org members: %w", err)
	}

	out := make([]Member, 0, len(rows))

	for _, m := range rows {
		out = append(out, newOrgMember(m))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handlePutOrgMember grants or changes someone's LOCAL org role. Org admin.
//
// This is the primary way a person joins an org: no group to create in the IdP, no
// group-map file to redeploy, no waiting for their next login. It writes local_role
// with its provenance (who granted it, when) and touches neither oidc_role nor
// oidc_group -- so the next login reconciles the claim half as usual and leaves
// this grant standing.
//
// # Granting OWNER requires being one, and so does writing OVER one
//
// The route is AccessOrgAdmin, but an org ADMIN may not mint an org OWNER: an owner
// can delete the org and everything cached in it, so admin -> owner would be a
// self-service escalation to a strictly greater authority than the granter holds.
// The check is on CanOwnOrg, so an owner and a site admin may do it and nobody else
// can. This narrows the route; it does not widen any.
//
// The mirror image is just as important and is the half that was missing: an org
// admin may not write over an EXISTING owner either. `PUT {"role":"member"}` against
// the owner is the same escalation run backwards -- it unmakes an authority strictly
// greater than the caller's, and (before the local role model, when every org role
// was claim-derived) it was not a reachable write at all. So the TARGET's effective
// role is checked, not only the requested one. See requireAuthorityOverTarget.
func (a *API) handlePutOrgMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	var req PutOrgMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	role, err := orgRoleOf(strings.TrimSpace(req.Role))
	if err != nil {
		return err
	}

	if role == auth.OrgRoleOwner && !p.CanOwnOrg(s.OrgID) {
		return errForbidden("only an organization owner may grant the owner role")
	}

	user, err := a.resolveUser(ctx, r.PathValue("user"))
	if err != nil {
		return err
	}

	if err := a.requireAuthorityOverTarget(ctx, p, s, user.ID, "change an owner's role"); err != nil {
		return err
	}

	membership, err := a.store.GrantOrgMembershipLocal(ctx, repository.GrantOrgMembershipLocalParams{
		UserID:    user.ID,
		OrgID:     s.OrgID,
		LocalRole: repository.NullOrgRole{OrgRole: role, Valid: true},
		GrantedBy: p.UserID(),
	})
	if err != nil {
		return fmt.Errorf("grant local org role: %w", err)
	}

	a.log.InfoContext(ctx, "granted a local org role",
		slog.String("org", s.OrgSlug),
		slog.String("user", user.Email),
		slog.String("local_role", string(role)),
		slog.String("granted_by", p.Email()),
	)

	writeJSON(w, http.StatusOK, newMembership(membership, user.Email, user.DisplayName, p.Email()))

	return nil
}

// handleDeleteOrgMember clears someone's LOCAL org role. Org admin.
//
// # The response has to be honest, and this is why
//
// This endpoint owns exactly one of the two sources of a membership. There are
// three outcomes and they are NOT interchangeable:
//
//  1. Local grant only. The row is deleted, the user leaves the org, and the
//     cascade takes their project roles and every API key they hold in the org with
//     it. 200, still_a_member = false.
//
//  2. Local grant AND an OIDC claim. The local half is cleared and THE ROW
//     SURVIVES: the user is still a member, at whatever role their groups confer,
//     with their project roles and their keys intact. 200, still_a_member = TRUE,
//     and the body names the group that is holding the membership up. An admin who
//     sees a bare 204 here and concludes the user is gone is wrong, and a bare 204
//     is what would have told them so.
//
//  3. No local grant at all -- the membership is purely claim-derived. There is
//     nothing here to remove. This is a 409, not a 200 and not a 204, because a
//     success on a request that changed nothing is the same lie as (2): the user is
//     still in the org, and the only way to remove them is in the IdP.
//
// # Removing an OWNER requires being one
//
// The route is AccessOrgAdmin, and removing an owner is an escalation past the
// caller's own authority in the most destructive direction there is: the DELETE
// cascades org_memberships -> project_memberships -> api_keys, so an admin could
// strip the owner of every project role and every API key they hold in the org, and
// re-adding the membership does not bring the keys back. Worse, it can leave the org
// with NO owner at all -- DELETE /orgs/{org} is AccessOrgOwner and granting owner
// needs CanOwnOrg, so nobody short of a site admin could own or delete it again. The
// same CanOwnOrg check that stops an admin MINTING an owner stops them unmaking one.
func (a *API) handleDeleteOrgMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	user, err := a.resolveUser(ctx, r.PathValue("user"))
	if err != nil {
		return err
	}

	current, err := a.store.GetOrgMembership(ctx, repository.GetOrgMembershipParams{
		UserID: user.ID, OrgID: s.OrgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("that user is not a member of this organization")
		}

		return fmt.Errorf("load org membership: %w", err)
	}

	// The target's authority, BEFORE the claim-derived branch: an admin who may not
	// touch this membership must not learn anything about it either, and must not
	// reach either write below.
	if current.Role == auth.OrgRoleOwner && !p.CanOwnOrg(s.OrgID) {
		return errForbidden("only an organization owner may remove an organization owner")
	}

	if !current.LocalRole.Valid {
		return errConflict(CodeClaimDerived, "this membership is derived from OIDC group claims"+
			claimSource(current)+", not from a grant made here, so it cannot be removed here. "+
			"Remove the user from the group in the identity provider, or change the group-to-org "+
			"mapping, and the membership disappears at their next login.")
	}

	// The claim half decides whether clearing the local half removes the user or
	// merely demotes them. Both statements are guarded on it in SQL, so this is a
	// branch, not a race: whichever one matches, matches.
	if current.OidcRole.Valid {
		survived, err := a.store.RevokeOrgMembershipLocal(ctx, repository.RevokeOrgMembershipLocalParams{
			UserID: user.ID, OrgID: s.OrgID,
		})
		if err != nil {
			return fmt.Errorf("revoke local org role: %w", err)
		}

		member := newMembership(survived, user.Email, user.DisplayName, "")

		a.log.InfoContext(ctx, "revoked a local org role; the membership survives by claim",
			slog.String("org", s.OrgSlug),
			slog.String("user", user.Email),
			slog.String("oidc_group", member.OIDCGroup),
		)

		writeJSON(w, http.StatusOK, OrgMemberRemoval{
			UserID: uuidString(user.ID), LocalRoleRevoked: true, StillAMember: true,
			Membership: &member,
			Message: "The local grant was removed, but this user IS STILL A MEMBER of the " +
				"organization: their OIDC group claims" + claimSource(current) + " justify the " +
				"membership on their own. Their project roles and API keys are unaffected. To " +
				"remove them entirely, remove them from the group in the identity provider.",
		})

		return nil
	}

	n, err := a.store.DeleteLocalOrgMembership(ctx, repository.DeleteLocalOrgMembershipParams{
		UserID: user.ID, OrgID: s.OrgID,
	})
	if err != nil {
		return fmt.Errorf("delete org membership: %w", err)
	}

	if n == 0 {
		// A concurrent login reconciled a claim onto the row between the read above
		// and this delete. The guard in the SQL held, which is the point of it: we
		// refuse rather than remove a membership the claims now justify.
		return errConflict(CodeConflict,
			"the membership changed while it was being removed; retry")
	}

	a.log.InfoContext(ctx, "removed an org membership",
		slog.String("org", s.OrgSlug),
		slog.String("user", user.Email),
	)

	writeJSON(w, http.StatusOK, OrgMemberRemoval{
		UserID: uuidString(user.ID), LocalRoleRevoked: true, StillAMember: false,
		Membership: nil,
		Message: "The user was removed from the organization. Their project roles and every " +
			"API key they held in it were revoked with the membership.",
	})

	return nil
}

// requireAuthorityOverTarget refuses a write against a member whose EFFECTIVE org
// role outranks the caller's authority: an org OWNER, written by someone who is not
// one.
//
// It reads the EFFECTIVE role (`role`, the generated greatest(oidc, local)), never
// local_role alone -- an owner by group claim is exactly as much an owner as an owner
// by local grant, and checking the local half only would leave the claim-derived case
// open. An owner and a site admin both pass, which is precisely CanOwnOrg's existing
// semantics: this narrows the route, it widens nothing.
//
// A target who holds no membership at all is not an owner, so there is nothing to
// protect and pgx.ErrNoRows is a pass -- that is the first grant, which is the whole
// point of the endpoint.
func (a *API) requireAuthorityOverTarget(
	ctx context.Context, p Principal, s scope, userID pgtype.UUID, action string,
) error {
	current, err := a.store.GetOrgMembership(ctx, repository.GetOrgMembershipParams{
		UserID: userID, OrgID: s.OrgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		return fmt.Errorf("load org membership: %w", err)
	}

	if current.Role == auth.OrgRoleOwner && !p.CanOwnOrg(s.OrgID) {
		return errForbidden("only an organization owner may " + action)
	}

	return nil
}

// claimSource names the group behind a claim-derived membership, for the prose in
// the two responses above. An admin's next question is always "held up by WHAT?".
func claimSource(m repository.OrgMembership) string {
	if !m.OidcGroup.Valid || m.OidcGroup.String == "" {
		return ""
	}

	return " (" + m.OidcGroup.String + ")"
}

// ---------------------------------------------------------------------------
// Project memberships: managed in-app, freely editable by an authorized caller.
// ---------------------------------------------------------------------------

// handleListProjectMembers lists the org's roster with each member's role IN THIS
// PROJECT.
//
// It returns every ORG member, not just the project's members, with ProjectRole
// empty for those who hold no role here. That is what the members screen renders
// ("org role: admin / role on firmware: none"), and it is what makes the "grant a
// role" affordance a picker rather than a free-text box that can only fail.
func (a *API) handleListProjectMembers(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	orgMembers, err := a.store.ListOrgMembers(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list org members: %w", err)
	}

	projectMembers, err := a.store.ListProjectMembers(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("list project members: %w", err)
	}

	roles := make(map[pgtype.UUID]repository.ProjectRole, len(projectMembers))
	for _, m := range projectMembers {
		roles[m.UserID] = m.Role
	}

	out := make([]Member, 0, len(orgMembers))

	for _, m := range orgMembers {
		member := newOrgMember(m)
		member.ProjectRole = string(roles[m.UserID])

		out = append(out, member)
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handlePutProjectMember grants or changes a project role. Project admin.
//
// {user} may be a user id or an email address. Either way it is resolved against
// THIS ORG's membership roster before anything is written -- so the path segment
// is never trusted, and a project admin in org A cannot name a user id from org B
// and quietly pull them in. (The composite foreign key would refuse that anyway,
// with a 23503; resolving first turns an opaque constraint violation into an
// honest 404.)
//
// # The downgrade transaction
//
// An API key's scope is capped at the member's project role AT GRANT TIME, and key
// validation deliberately never joins project_memberships -- a second index probe
// on a path that runs thousands of times in parallel at the start of every BitBake
// build is not affordable. The consequence is that a role downgrade does NOT
// automatically shrink the keys already issued under the old role: a writer demoted
// to reader would keep every write key they hold, indefinitely, and the cache would
// keep honouring them.
//
// So the downgrade and the revocation are ONE transaction. Running the revocation
// as a separate statement afterwards would leave a window -- short, but a window in
// which a demoted user still has write access to the cache, and one that a crash
// between the two statements widens to forever.
func (a *API) handlePutProjectMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	var req PutProjectMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	role, err := projectRoleOf(strings.TrimSpace(req.Role))
	if err != nil {
		return err
	}

	userID, err := a.resolveOrgMember(ctx, s.OrgID, r.PathValue("user"))
	if err != nil {
		return err
	}

	var out repository.ProjectMembership

	err = a.store.Tx(ctx, func(q *repository.Queries) error {
		membership, err := q.UpsertProjectMembership(ctx, repository.UpsertProjectMembershipParams{
			UserID: userID, ID: s.ProjectID, Role: role,
		})
		if err != nil {
			return fmt.Errorf("upsert project membership: %w", err)
		}

		out = membership

		// Revoke every key whose scope now exceeds the new role, IN THIS TRANSACTION.
		//
		// A promotion revokes nothing: for a writer or an admin MaxScopeForRole is
		// write, so no scope exceeds the role and there is no work to do. Skipping
		// the statement then is not an optimisation, it is what keeps the log line
		// below honest.
		exceeding := scopesExceedingRole(role)
		if len(exceeding) == 0 {
			return nil
		}

		revoked, err := q.RevokeAPIKeysForMembership(ctx, repository.RevokeAPIKeysForMembershipParams{
			UserID:    userID,
			ProjectID: s.ProjectID,
			Scopes:    exceeding,
		})
		if err != nil {
			return fmt.Errorf("revoke keys exceeding the new role: %w", err)
		}

		if revoked > 0 {
			a.log.InfoContext(ctx, "revoked API keys exceeding the new project role",
				slog.String("project", s.ProjectSlug),
				slog.String("role", string(role)),
				slog.Int64("keys_revoked", revoked),
			)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set project role: %w", err)
	}

	writeJSON(w, http.StatusOK, Member{
		UserID: uuidString(out.UserID), Email: "", DisplayName: "",
		OrgRole: "", OIDCRole: "", OIDCGroup: "", LocalRole: "",
		GrantedBy: "", GrantedByEmail: "", GrantedAt: nil,
		ProjectRole: string(out.Role), Source: "",
	})

	return nil
}

// scopesExceedingRole lists the key scopes that a member holding `role` may NOT
// hold -- the argument to RevokeAPIKeysForMembership on a downgrade.
//
// It is DERIVED from auth.ScopeWithinRole rather than restating the rule, so a
// future scope (or a change to what a reader may do) cannot leave this list
// quietly stale while every caller still believes it is exhaustive. The cost is
// a loop over two constants, once per role change.
func scopesExceedingRole(role auth.ProjectRole) []auth.Scope {
	var out []auth.Scope

	for _, scope := range []auth.Scope{auth.ScopeRead, auth.ScopeWrite} {
		if !auth.ScopeWithinRole(scope, role) {
			out = append(out, scope)
		}
	}

	return out
}

// handleDeleteProjectMember removes someone's project role. Project admin.
//
// The database cascades this into api_keys: removing a member deletes every key
// they hold for the project, in the same statement. The API does not have to
// remember to, and a future endpoint that forgets cannot leave a live credential
// behind.
func (a *API) handleDeleteProjectMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	userID, err := a.resolveOrgMember(ctx, s.OrgID, r.PathValue("user"))
	if err != nil {
		return err
	}

	n, err := a.store.DeleteProjectMembership(ctx, repository.DeleteProjectMembershipParams{
		UserID: userID, ProjectID: s.ProjectID,
	})
	if err != nil {
		return fmt.Errorf("delete project membership: %w", err)
	}

	if n == 0 {
		return errNotFound("that user holds no role in this project")
	}

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}

// resolveUser turns the {user} path segment -- a user id or an email -- into a
// USER, without requiring them to be a member of anything.
//
// This is the resolver the ORG-membership endpoints need, and it is deliberately
// weaker than resolveOrgMember below: granting someone their FIRST membership in an
// org means naming a user who is, by definition, not on the org's roster yet.
//
// The IDOR guard therefore cannot come from the roster here. It comes from the
// guard: the route is AccessOrgAdmin against the org in the path, and the only
// thing this id is ever used for is a write to (this user, THAT org). A caller who
// names a user id from another tenant can, at most, add that user to an org they
// already administer -- they cannot read anything of that user's, and they cannot
// reach any other org.
//
// It does tell an org admin whether an email address has a Bakery account, and that
// is an unavoidable consequence of being able to add one: the user must exist (they
// are provisioned at their first login) or there is nothing to grant. The message
// says exactly that, so nobody has to guess.
func (a *API) resolveUser(ctx context.Context, ref string) (repository.User, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return repository.User{}, errValidation("user", "a user id or email address is required")
	}

	notFound := errNotFound("no such user. Users are provisioned at their first login; " +
		"ask them to sign in once, then grant the role")

	var (
		user repository.User
		err  error
	)

	if strings.Contains(ref, "@") {
		user, err = a.store.GetUserByEmail(ctx, ref)
	} else {
		var id pgtype.UUID

		id, err = parseUUID(ref)
		if err != nil {
			return repository.User{}, err
		}

		user, err = a.store.GetUser(ctx, id)
	}

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repository.User{}, notFound
		}

		return repository.User{}, fmt.Errorf("resolve user: %w", err)
	}

	return user, nil
}

// resolveOrgMember turns the {user} path segment -- a user id or an email -- into
// a user id, VERIFIED to be a member of this org.
//
// This is the IDOR guard for the membership endpoints. The path segment is a
// caller-supplied identifier, so it is never used as one: it is looked up in the
// org's own roster, and anything not on that roster is a 404. A project admin
// cannot reach a user outside the org they were authorized against, whatever they
// put in the URL.
func (a *API) resolveOrgMember(ctx context.Context, orgID pgtype.UUID, ref string) (pgtype.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return pgtype.UUID{}, errValidation("user", "a user id or email address is required")
	}

	members, err := a.store.ListOrgMembers(ctx, orgID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("list org members: %w", err)
	}

	byEmail := strings.Contains(ref, "@")

	for _, m := range members {
		if byEmail && strings.EqualFold(m.Email, ref) {
			return m.UserID, nil
		}

		if !byEmail && uuidString(m.UserID) == strings.ToLower(ref) {
			return m.UserID, nil
		}
	}

	// Deliberately the same 404 whether the user does not exist at all or merely is
	// not in this org: telling an org admin which email addresses have Bakery
	// accounts elsewhere in the installation is a directory-enumeration oracle.
	return pgtype.UUID{}, errNotFound(
		"no such member of this organization. Users are provisioned at their first login; " +
			"add them to the organization with PUT /orgs/{org}/members/{user}, or map an OIDC " +
			"group to it")
}
