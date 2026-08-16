package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// Personal access tokens: /api/v1/user/tokens.
//
// EVERY ROUTE HERE IS AccessUser -- a verified HUMAN, never a machine. That is
// not belt-and-braces on top of auth.requireMintAuthority; it is the coarser half
// of the same rule, applied at the door. A token that could LIST its owner's
// other tokens would hand a leaked credential a map of every other credential
// that human holds, and a token that could REVOKE them would hand it a denial of
// service. Neither needs a capability to be dangerous, so neither gets through
// the door.
//
// There is no {user} segment anywhere below. A token is always the CALLER's:
// "manage another user's tokens" is not a route that exists, because the query
// layer scopes every mutation by user_id in the statement and there is nothing to
// plumb another id into.

// maxUserTokenLifetime caps how far ahead a personal access token may expire.
//
// There is no cap in the schema -- expires_at is nullable, and "never" is a
// legitimate deployment for a build host that must survive an unattended year.
// This is the API's ceiling on an EXPLICIT expiry, so that a typo'd year cannot
// mint a century-long credential while looking like a normal request.
const maxUserTokenLifetime = 10 * 365 * 24 * time.Hour

// CreateUserTokenRequest mints a personal access token.
//
// `scope` is the ceiling, not a grant: see UserToken.MaxScope. `expires_at` null
// or absent means never; the console sends 90 days by default.
type CreateUserTokenRequest struct {
	Name      string     `json:"name"`
	Scope     string     `json:"scope"` // read|write
	ExpiresAt *time.Time `json:"expires_at"`
}

// handleListUserTokens lists the caller's own tokens, metadata only.
func (a *API) handleListUserTokens(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	rows, err := a.store.ListUserTokensForUser(ctx, p.UserID())
	if err != nil {
		return fmt.Errorf("list user tokens: %w", err)
	}

	out := make([]UserToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, newUserToken(row))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// handleCreateUserToken mints a token and returns the plaintext EXACTLY ONCE.
//
// There is no scope cap against a role here and there deliberately cannot be: a
// personal access token names no project, so its ceiling is the caller's own live
// authority, re-evaluated on every request. A reader who mints a `write` token
// gets a token that writes exactly nothing -- see auth.CreateUserToken.
func (a *API) handleCreateUserToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	var req CreateUserTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	maxScope, err := scopeOf(strings.TrimSpace(req.Scope))
	if err != nil {
		return err
	}

	if req.ExpiresAt != nil {
		if !req.ExpiresAt.After(time.Now()) {
			return errValidation("expires_at", "expires_at must be in the future")
		}

		if req.ExpiresAt.After(time.Now().Add(maxUserTokenLifetime)) {
			return errValidation("expires_at", "expires_at is too far in the future")
		}
	}

	token, row, err := a.keys.CreateUserToken(ctx, p, auth.CreateUserTokenInput{
		Name: req.Name, MaxScope: maxScope, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("create user token: %w", err)
	}

	// The token's NAME, prefix and ceiling. Never the plaintext: a secret that
	// reaches the log has been disclosed to everyone who can read the log.
	a.log.InfoContext(ctx, "minted a personal access token",
		"name", row.Name, "prefix", row.TokenPrefix, "max_scope", string(row.MaxScope))

	writeJSON(w, http.StatusCreated, CreatedUserToken{
		UserToken: UserToken{
			ID: uuidString(row.ID), Name: row.Name, TokenPrefix: row.TokenPrefix,
			MaxScope: string(row.MaxScope), CreatedAt: row.CreatedAt.Time,
			ExpiresAt: timePtr(row.ExpiresAt), LastUsedAt: nil, RevokedAt: nil,
		},
		Token: token.Token,
	})

	return nil
}

// handleRevokeUserToken revokes one of the caller's own tokens.
//
// The IDOR is closed IN THE STATEMENT: RevokeUserToken's predicate carries
// user_id, so an id belonging to another human matches nothing and comes back as
// a 404 -- identical to an id that does not exist, which is the answer that tells
// a prober the least.
func (a *API) handleRevokeUserToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	tokenID, err := parseUUID(r.PathValue("token"))
	if err != nil {
		return err
	}

	n, err := a.store.RevokeUserToken(ctx, repository.RevokeUserTokenParams{
		ID: tokenID, UserID: p.UserID(),
	})
	if err != nil {
		return fmt.Errorf("revoke user token: %w", err)
	}

	if n == 0 {
		// Either it is not yours, it does not exist, or it is already revoked. All
		// three answer the same way: revocation is the safe direction, so a
		// double-revoke must never look like a failure to revoke, and "not yours" must
		// never be distinguishable from "no such token".
		writeJSON(w, http.StatusNoContent, nil)

		return nil
	}

	a.log.InfoContext(ctx, "revoked a personal access token", "token_id", uuidString(tokenID))

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}

func newUserToken(row repository.ListUserTokensForUserRow) UserToken {
	return UserToken{
		ID:          uuidString(row.ID),
		Name:        row.Name,
		TokenPrefix: row.TokenPrefix,
		MaxScope:    string(row.MaxScope),
		CreatedAt:   row.CreatedAt.Time,
		ExpiresAt:   timePtr(row.ExpiresAt),
		LastUsedAt:  timePtr(row.LastUsedAt),
		RevokedAt:   timePtr(row.RevokedAt),
	}
}
