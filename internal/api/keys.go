package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// CreateKeyRequest mints a project-scoped API key.
//
// Note what is NOT here: a user_id. A key is ALWAYS minted for the caller. "Mint a
// key on behalf of another user" is a credential-forging primitive -- it makes the
// per-user key model, and with it every audit trail that says "this cache write
// came from this human", a fiction. auth.CreateKeyInput has no such field either,
// so there is nothing to plumb it into.
type CreateKeyRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"` // read|write

	// ExpiresAt is optional; null or absent means the key never expires.
	ExpiresAt *time.Time `json:"expires_at"`
}

// handleListKeys lists a project's API keys as METADATA ONLY.
//
// There is no token here and no hash here, and that is enforced by the types
// rather than by care: the Store interface exposes no query that returns
// token_sha256, and the APIKey wire type has no field to hold a secret. The
// schema, in turn, has no plaintext column at all. Three layers, and the innermost
// one is a database that literally cannot represent the thing we are promising not
// to leak.
//
// Visibility: a project admin sees every key in the project (they have to -- they
// are who revokes the leaked one belonging to someone who left). Everyone else
// sees only their own. A reader being able to enumerate their colleagues' key
// names, owners and last-used times is a reconnaissance gift for no operational
// benefit.
func (a *API) handleListKeys(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	rows, err := a.store.ListAPIKeysForProject(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("list api keys: %w", err)
	}

	owners, err := a.owners(ctx)
	if err != nil {
		return err
	}

	seeAll := p.CanAdminProject(s.OrgID, s.ProjectID)
	self := p.UserID()

	out := make([]APIKey, 0, len(rows))

	for _, row := range rows {
		if !seeAll && row.UserID != self {
			continue
		}

		owner := owners[uuidString(row.UserID)]
		out = append(out, newAPIKey(row, owner[0], owner[1]))
	}

	writeJSON(w, http.StatusOK, list(out))

	return nil
}

// owners maps user id -> {email, display name}, for decorating key rows.
//
// ListAPIKeysForProject does not join users -- the api_keys queries are written to
// stay join-free because the validation query on that table is the sstate HEAD hot
// path and nobody wants a second index to maintain there. So the join happens here,
// on a cold console page, over a user table measured in tens.
func (a *API) owners(ctx context.Context) (map[string][2]string, error) {
	users, err := a.store.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	out := make(map[string][2]string, len(users))
	for _, u := range users {
		out[uuidString(u.ID)] = [2]string{u.Email, u.DisplayName}
	}

	return out, nil
}

// handleCreateKey mints a key and returns the plaintext EXACTLY ONCE.
//
// This is the only response body in the entire API that carries a secret, and the
// only moment the secret exists anywhere: auth.GenerateAPIKey produces 256 bits of
// crypto/rand, hands back the plaintext and the SHA-256, and only the SHA-256 is
// written. There is no plaintext column, so there is no query, no admin endpoint,
// no support workflow and no database dump that can ever recover it. If the user
// closes the modal without copying it, the key is gone and they mint another.
//
// The scope is capped at the caller's project role INSIDE auth.CreateAPIKey (a
// reader cannot mint a write key), and an API-key principal cannot mint keys at
// all -- both are enforced there, not here, which is why this handler can be this
// short. The route is AccessProjectRead because a reader minting a read key for
// themselves is the normal case; the cap does the rest.
func (a *API) handleCreateKey(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	var req CreateKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errValidation("name", "name must not be empty")
	}

	keyScope, err := scopeOf(strings.TrimSpace(req.Scope))
	if err != nil {
		return err
	}

	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return errValidation("expires_at", "expires_at must be in the future")
	}

	key, row, err := a.keys.CreateAPIKey(ctx, p, auth.CreateKeyInput{
		OrgID: s.OrgID, ProjectID: s.ProjectID,
		Name: req.Name, Scope: keyScope, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}

	a.log.InfoContext(ctx, "minted an API key",
		// The key's NAME, prefix and scope. Never the token: a secret that reaches
		// the log has been disclosed to everyone who can read the log, which is a
		// much larger set than everyone who should have the key.
		"project", s.ProjectSlug, "name", row.Name,
		"prefix", row.TokenPrefix, "scope", string(row.Scope),
	)

	writeJSON(w, http.StatusCreated, CreatedAPIKey{
		APIKey: APIKey{
			ID: uuidString(row.ID), Name: row.Name, ProjectID: uuidString(row.ProjectID),
			TokenPrefix: row.TokenPrefix, Scope: string(row.Scope),
			OwnerID: uuidString(row.UserID), OwnerEmail: p.Email(), OwnerName: p.DisplayName(),
			CreatedAt: row.CreatedAt.Time, ExpiresAt: timePtr(row.ExpiresAt),
			LastUsedAt: nil, RevokedAt: nil,
		},
		Token: key.Token,
	})

	return nil
}

// handleRevokeKey revokes a key.
//
// # The IDOR
//
// {key} is a UUID the caller supplies, and repository.RevokeAPIKey takes an id and
// revokes it -- with no project check, because at the query layer there is nothing
// to check against. A handler that passed the path value straight through would
// let any project reader anywhere revoke ANY key in the installation, given only
// its id. That is the textbook insecure-direct-object-reference, and it is three
// lines of extremely reasonable-looking code.
//
// So the id is not trusted. It is looked up in THIS project's key list -- the list
// the guard already authorized the caller against -- and if it is not there, it is
// a 404, whether it does not exist or belongs to someone else's project. Then
// ownership: you may revoke your own key; a project admin may revoke anyone's.
//
// The route's Access is ProjectRead rather than ProjectAdmin precisely because a
// reader must be able to revoke their own leaked key without waiting for an admin.
// That makes the per-object check below load-bearing rather than decorative, and it
// is why it is here and not delegated to the route table.
func (a *API) handleRevokeKey(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	keyID, err := parseUUID(r.PathValue("key"))
	if err != nil {
		return err
	}

	rows, err := a.store.ListAPIKeysForProject(ctx, s.ProjectID)
	if err != nil {
		return fmt.Errorf("list api keys: %w", err)
	}

	var target *repository.ListAPIKeysForProjectRow

	for i := range rows {
		if rows[i].ID == keyID {
			target = &rows[i]

			break
		}
	}

	// Not in THIS project's list => 404, regardless of whether it exists elsewhere.
	if target == nil {
		return errNotFound("api key not found")
	}

	if target.UserID != p.UserID() && !p.CanAdminProject(s.OrgID, s.ProjectID) {
		return errForbidden("you may only revoke your own keys")
	}

	n, err := a.store.RevokeAPIKey(ctx, keyID)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}

	if n == 0 {
		// Already revoked. Idempotent: revocation is the safe direction, and a
		// double-revoke must never look like a failure to revoke.
		writeJSON(w, http.StatusNoContent, nil)

		return nil
	}

	a.log.InfoContext(ctx, "revoked an API key",
		"project", s.ProjectSlug, "name", target.Name, "prefix", target.TokenPrefix,
	)

	writeJSON(w, http.StatusNoContent, nil)

	return nil
}
