package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Robots: /api/v1/orgs/{org}/robots.
//
// EVERY ROUTE HERE IS AccessOrgAdmin, and the door (methodMayReach) admits only
// an interactive human to that level. A robot token managing robots would be a
// self-renewing, org-wide, write-everywhere credential with no human in the loop;
// a personal access token doing it would be an org-admin capability the PAT
// design explicitly refuses to grant.
//
// Nothing here takes an org id from the request. The guard resolved {org} to a
// database id and authorized it, and every query below is scoped by that id -- so
// a robot id from another tenant is not in the result set and 404s, with no
// cross-tenant check for a handler to forget.

// CreateRobotRequest creates a machine identity. It mints nothing: a robot with
// no tokens authorizes nothing at all, which is the right thing for the object
// that merely names one.
type CreateRobotRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateOrgTokenRequest mints a token for a robot.
//
// ExpiresAt is REQUIRED, unlike every other credential in the API.
// org_tokens.expires_at is NOT NULL because a robot deliberately outlives its
// creator: the mint-time cap and the membership cascade that revoke an API key do
// not exist here, so expiry is the countervailing control.
type CreateOrgTokenRequest struct {
	Name      string     `json:"name"`
	Scope     string     `json:"scope"` // read|write
	ExpiresAt *time.Time `json:"expires_at"`
}

// handleListRobots lists the org's robots, each with its tokens (metadata only).
//
// One query per TABLE, not per robot: the token list is fetched for the whole org
// and grouped in memory. A per-robot query would be N+1 on a page whose whole
// purpose is to be read.
func (a *API) handleListRobots(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	robots, err := a.store.ListRobotsForOrg(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list robots: %w", err)
	}

	tokens, err := a.store.ListOrgTokensForOrg(ctx, s.OrgID)
	if err != nil {
		return fmt.Errorf("list org tokens: %w", err)
	}

	byRobot := make(map[string][]OrgToken, len(robots))
	for _, row := range tokens {
		id := uuidString(row.RobotID)
		byRobot[id] = append(byRobot[id], newOrgToken(row))
	}

	out := make([]Robot, 0, len(robots))

	for _, robot := range robots {
		id := uuidString(robot.ID)

		list := byRobot[id]
		if list == nil {
			list = []OrgToken{}
		}

		out = append(out, Robot{
			ID: id, OrgID: uuidString(robot.OrgID), Name: robot.Name,
			Description:    robot.Description,
			CreatedBy:      uuidString(robot.CreatedBy),
			CreatedByEmail: robot.CreatedByEmail,
			CreatedAt:      robot.CreatedAt.Time,
			Tokens:         list,
		})
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

func (a *API) handleCreateRobot(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var req CreateRobotRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	robot, err := a.store.CreateRobot(ctx, repository.CreateRobotParams{
		OrgID: s.OrgID, Name: req.Name, Description: strings.TrimSpace(req.Description),
		// created_by is the FK (ON DELETE SET NULL); created_by_email is the snapshot
		// that survives the human, because the robot does.
		CreatedBy: p.UserID(), CreatedByEmail: p.Email(),
	})
	if err != nil {
		if isPGCode(err, pgUniqueViolation) {
			return errConflict(CodeConflict, "a robot with that name already exists in this organization")
		}

		return fmt.Errorf("create robot: %w", err)
	}

	a.log.InfoContext(ctx, "created a robot", "org", s.OrgSlug, "name", robot.Name)

	writeJSON(w, http.StatusCreated, Robot{
		ID: uuidString(robot.ID), OrgID: uuidString(robot.OrgID), Name: robot.Name,
		Description:    robot.Description,
		CreatedBy:      uuidString(robot.CreatedBy),
		CreatedByEmail: robot.CreatedByEmail,
		CreatedAt:      robot.CreatedAt.Time,
		Tokens:         []OrgToken{},
	})

	return nil
}

// handleDeleteRobot deletes a robot, cascading every token it holds.
//
// This is one of the two STRUCTURAL revocation legs a robot has (the other is
// deleting the org), so it must not be reachable for a robot in another tenant --
// which is why the delete is scoped by org_id in the statement rather than by a
// check here.
func (a *API) handleDeleteRobot(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	robotID, err := a.resolveRobot(r)
	if err != nil {
		return err
	}

	n, err := a.store.DeleteRobotForOrg(ctx, repository.DeleteRobotForOrgParams{
		ID: robotID, OrgID: s.OrgID,
	})
	if err != nil {
		return fmt.Errorf("delete robot: %w", err)
	}

	if n == 0 {
		return errNotFound("robot not found")
	}

	a.log.InfoContext(ctx, "deleted a robot", "org", s.OrgSlug, "robot_id", uuidString(robotID))

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}

// handleCreateOrgToken mints a robot token and returns the plaintext EXACTLY
// ONCE.
func (a *API) handleCreateOrgToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	robotID, err := a.resolveRobot(r)
	if err != nil {
		return err
	}

	var req CreateOrgTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	scope, err := scopeOf(strings.TrimSpace(req.Scope))
	if err != nil {
		return err
	}

	// REQUIRED, and capped. "Never" is not representable in the schema and must not
	// be smuggled in as an absent field that the API quietly turns into a century.
	if req.ExpiresAt == nil {
		return errValidation("expires_at", "a robot token must have an expiry")
	}

	if !req.ExpiresAt.After(time.Now()) {
		return errValidation("expires_at", "expires_at must be in the future")
	}

	if req.ExpiresAt.After(time.Now().Add(auth.MaxOrgTokenLifetime)) {
		return errValidation("expires_at", "a robot token may not last more than 365 days")
	}

	token, row, err := a.keys.CreateOrgToken(ctx, p, auth.CreateOrgTokenInput{
		RobotID: robotID, OrgID: s.OrgID, Name: req.Name,
		Scope: scope, ExpiresAt: *req.ExpiresAt,
	})
	if err != nil {
		if isPGCode(err, pgUniqueViolation) {
			return errConflict(CodeConflict, "a live token with that name already exists on this robot")
		}

		return fmt.Errorf("create org token: %w", err)
	}

	a.log.InfoContext(ctx, "minted a robot token",
		"org", s.OrgSlug, "robot_id", uuidString(robotID),
		"name", row.Name, "prefix", row.TokenPrefix, "scope", string(row.Scope))

	writeJSON(w, http.StatusCreated, CreatedOrgToken{
		OrgToken: OrgToken{
			ID: uuidString(row.ID), RobotID: uuidString(row.RobotID), OrgID: uuidString(row.OrgID),
			Name: row.Name, TokenPrefix: row.TokenPrefix, Scope: string(row.Scope),
			ExpiresAt:      row.ExpiresAt.Time,
			CreatedBy:      uuidString(row.CreatedBy),
			CreatedByEmail: row.CreatedByEmail,
			CreatedAt:      row.CreatedAt.Time,
			LastUsedAt:     nil, RevokedAt: nil,
		},
		Token: token.Token,
	})

	return nil
}

// handleRevokeOrgToken revokes one token without deleting the robot.
func (a *API) handleRevokeOrgToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := scopeFrom(ctx)

	robotID, err := a.resolveRobot(r)
	if err != nil {
		return err
	}

	tokenID, err := parseUUID(r.PathValue("token"))
	if err != nil {
		return err
	}

	n, err := a.store.RevokeOrgToken(ctx, repository.RevokeOrgTokenParams{
		ID: tokenID, RobotID: robotID, OrgID: s.OrgID,
	})
	if err != nil {
		return fmt.Errorf("revoke org token: %w", err)
	}

	if n == 0 {
		// Already revoked, not this robot's, or not this org's. Idempotent and
		// indistinguishable, for the same reasons as handleRevokeUserToken.
		writeJSON(w, http.StatusNoContent, nil)

		return nil
	}

	a.log.InfoContext(ctx, "revoked a robot token",
		"org", s.OrgSlug, "robot_id", uuidString(robotID), "token_id", uuidString(tokenID))

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}

// resolveRobot turns the {robot} path segment into an id THAT BELONGS TO THE
// AUTHORIZED ORG.
//
// The guard resolves {org} and {project}, and nothing else -- a robot id is a
// route-specific object, so this is where the equivalent check lives. It reads
// the row scoped by the guard's org id, so a well-formed id from another tenant
// is a 404 and never an oracle.
func (a *API) resolveRobot(r *http.Request) (pgtype.UUID, error) {
	ctx := r.Context()
	s := scopeFrom(ctx)

	robotID, err := parseUUID(r.PathValue("robot"))
	if err != nil {
		return pgtype.UUID{}, err
	}

	if _, err := a.store.GetRobotForOrg(ctx, repository.GetRobotForOrgParams{
		ID: robotID, OrgID: s.OrgID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, errNotFound("robot not found")
		}

		return pgtype.UUID{}, fmt.Errorf("load robot: %w", err)
	}

	return robotID, nil
}

func newOrgToken(row repository.ListOrgTokensForOrgRow) OrgToken {
	return OrgToken{
		ID:             uuidString(row.ID),
		RobotID:        uuidString(row.RobotID),
		OrgID:          uuidString(row.OrgID),
		Name:           row.Name,
		TokenPrefix:    row.TokenPrefix,
		Scope:          string(row.Scope),
		ExpiresAt:      row.ExpiresAt.Time,
		CreatedBy:      uuidString(row.CreatedBy),
		CreatedByEmail: row.CreatedByEmail,
		CreatedAt:      row.CreatedAt.Time,
		LastUsedAt:     timePtr(row.LastUsedAt),
		RevokedAt:      timePtr(row.RevokedAt),
	}
}
