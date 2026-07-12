package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// PutProjectMemberRequest grants or changes a project role.
type PutProjectMemberRequest struct {
	Role string `json:"role"` // reader|writer|admin
}

// ---------------------------------------------------------------------------
// Org memberships: READ-ONLY over this API.
// ---------------------------------------------------------------------------

// handleListOrgMembers lists an org's members and their claim-derived org roles.
func (a *API) handleListOrgMembers(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	rows, err := a.store.ListOrgMembers(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list org members: %w", err)
	}

	out := make([]Member, 0, len(rows))

	for _, m := range rows {
		out = append(out, Member{
			UserID: uuidString(m.UserID), Email: m.Email, DisplayName: m.DisplayName,
			OrgRole: string(m.Role), ProjectRole: "", Source: OrgRoleSourceOIDC,
		})
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handleOrgMemberImmutable refuses every write to an org membership.
//
// # Why this endpoint exists only to say no
//
// Org roles (and the site role) are 100% derived from OIDC group claims and are
// RECONCILED ON EVERY LOGIN: auth.Service.Reconcile computes the user's whole org
// set from the group map and then calls ReconcileOrgMembershipsRemove with that
// set as the keep-list, deleting everything else. The reconciler owns the table.
//
// So an API that let an admin hand-edit an org role would produce one of exactly
// two outcomes, and both are bad:
//
//   - The IdP does not agree. The edit survives until that user's next login and
//     is then silently reverted. The admin saw a 200, the console showed the new
//     role, and the change evaporated hours later with no event and no log line
//     anyone will read. Worse in the granting direction than the revoking one: for
//     a window measured in "however long until they log in again", the user holds
//     authority their IdP groups never conferred, and the audit trail for it lives
//     in Bakery rather than in the identity provider where the org's access review
//     will look.
//
//   - The IdP does agree, in which case the edit was unnecessary -- fix the group
//     in the IdP and the next login carries it.
//
// Nothing is gained and a whole class of "why did my permissions change back"
// incidents is created. The right place to change an org role is the group map and
// the IdP, and the right thing for the API to do is say so.
//
// It is a 409 rather than a 404 or a 405 on purpose. A 404 would send an operator
// hunting for an endpoint they are certain must exist; a 405 would imply the
// method is wrong rather than the whole idea. A 409 with a code the console can
// branch on (`claim_derived_role`) says: the state you are trying to write is
// owned by something else.
//
// Note the route is still AccessOrgAdmin. Refusing AFTER the authorization check
// means an unauthorized caller learns nothing about the org from probing it -- they
// get a 404/403 exactly as they would anywhere else, not a helpful 409.
func (a *API) handleOrgMemberImmutable(_ http.ResponseWriter, r *http.Request) error {
	_ = scopeFrom(r.Context()) // assert the guard resolved and authorized a scope

	return errConflict(CodeClaimDerived,
		"org roles are derived from OIDC group claims and are reconciled on every login; "+
			"they cannot be edited here. Change the user's groups in the identity provider, "+
			"or change the group-to-org mapping file, and the next login will carry it.")
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
		out = append(out, Member{
			UserID: uuidString(m.UserID), Email: m.Email, DisplayName: m.DisplayName,
			OrgRole: string(m.Role), ProjectRole: string(roles[m.UserID]),
			Source: OrgRoleSourceOIDC,
		})
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
//
// (internal/db ships RevokeAPIKeysForMembership for precisely this, and it does not
// currently work -- see the comment at the revocation loop below.)
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
		// # Why this is not RevokeAPIKeysForMembership
		//
		// internal/db ships exactly that query for exactly this job, and it does not
		// work. Its parameter is `[]repository.ApiKeyScope` bound to
		// `sqlc.arg(scopes)::api_key_scope[]`, and pgx cannot build an encode plan for
		// a slice of a custom enum type unless that type has been registered on the
		// connection (conn.LoadType + TypeMap().RegisterType, in a pgxpool
		// AfterConnect). internal/db does not register it, so the call fails at
		// encode time:
		//
		//	unable to encode []repository.ApiKeyScope{"write"} into text format
		//	for unknown type (OID 16427): cannot find encode plan
		//
		// It had never had a caller, so nothing had ever proven it. Registering the
		// enum on the pool is the real fix and it belongs in internal/db; until then,
		// this does the same work with parameters that DO encode (uuids), inside the
		// same transaction, so the atomicity the invariant actually depends on is
		// preserved. Do not "simplify" this back to the one-liner without first
		// registering the type -- there is a test that will catch you, and it is the
		// one that proves a demoted writer loses their write key.
		keys, err := q.ListAPIKeysForProject(ctx, s.ProjectID)
		if err != nil {
			return fmt.Errorf("list keys for role downgrade: %w", err)
		}

		var revoked int

		for _, key := range keys {
			if key.UserID != userID || key.RevokedAt.Valid {
				continue
			}

			// Within the new role's authority => keep. For a writer or an admin,
			// MaxScopeForRole is write, so nothing is revoked and a promotion is
			// correctly a no-op.
			if auth.ScopeWithinRole(key.Scope, role) {
				continue
			}

			if _, err := q.RevokeAPIKey(ctx, key.ID); err != nil {
				return fmt.Errorf("revoke key exceeding role: %w", err)
			}

			revoked++
		}

		if revoked > 0 {
			a.log.InfoContext(ctx, "revoked API keys exceeding the new project role",
				slog.String("project", s.ProjectSlug),
				slog.String("role", string(role)),
				slog.Int("keys_revoked", revoked),
			)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set project role: %w", err)
	}

	writeJSON(w, http.StatusOK, Member{
		UserID: uuidString(out.UserID), Email: "", DisplayName: "",
		OrgRole: "", ProjectRole: string(out.Role), Source: OrgRoleSourceOIDC,
	})

	return nil
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
		"no such member of this organization. Users are provisioned at their first login, " +
			"and org membership comes from OIDC group claims")
}
