package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// Robot tokens (`bkro_`): org-owned machine identities.
//
// This is the CHEAPEST credential in Bakery, and the shape of that is the design
// rather than an optimization. A robot has no human behind it, so there is no
// role set to resolve, no principal to load and no cache to key: one Index Only
// Scan on org_tokens_token_sha256_key returns {robot_id, org_id, scope} and the
// authorization decision is a uuid compare. Cheaper than a user token, identical
// to an api_keys probe.
//
// What it authorizes is deliberately blunt: every project in one org, present and
// future, at one scope. "Current and future" is not a feature that had to be
// built -- the decision never names a project, so a project created after the
// token was minted is covered with no provisioning at all.
//
// What it CANNOT do is enforced by having no fields to do it with. The principal
// carries no orgs map, no projects map and no site role, so CanViewOrg,
// CanAdminOrg, CanOwnOrg, CanAdminProject and IsSiteAdmin have nothing to read
// even before principal.actsAsUser refuses it.

// looksLikeOrgToken is this validator's own shape gate. See looksLikeUserToken.
func looksLikeOrgToken(token string) bool {
	return strings.HasPrefix(token, OrgTokenPrefix) && len(token) > len(OrgTokenPrefix)+displayLen
}

type orgTokenRow struct {
	id      pgtype.UUID
	robotID pgtype.UUID
	orgID   pgtype.UUID
	scope   Scope
	hash    []byte
}

type orgTokenStore interface {
	validateOrgToken(ctx context.Context, hash []byte) (orgTokenRow, error)
	touchOrgTokens(ctx context.Context, ids []pgtype.UUID) error
}

type pgOrgTokenStore struct{ pool *pgxpool.Pool }

// validateOrgTokenSQL: ZERO JOINS, one covering-index probe.
//
// org_id is denormalized onto org_tokens precisely so this statement never has to
// look through `robots` -- the authorization decision is made from the index
// tuple alone. Deleting the robot cascades these rows away, and deleting the org
// cascades them by two independent paths, so the denormalized copy cannot outlive
// the thing it names.
//
// `expires_at > now()` has no NULL arm, unlike every other validator in this
// package: org_tokens.expires_at is NOT NULL. A robot outlives its creator on
// purpose, so expiry is the countervailing control and the schema refuses to
// represent a robot token that never expires.
const validateOrgTokenSQL = `
SELECT id, robot_id, org_id, scope, token_sha256
  FROM org_tokens
 WHERE token_sha256 = $1
   AND revoked_at IS NULL
   AND expires_at > now()`

func (s pgOrgTokenStore) validateOrgToken(ctx context.Context, hash []byte) (orgTokenRow, error) {
	var row orgTokenRow

	err := s.pool.QueryRow(ctx, validateOrgTokenSQL, hash).
		Scan(&row.id, &row.robotID, &row.orgID, &row.scope, &row.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return orgTokenRow{}, ErrKeyInvalid
	}

	if err != nil {
		return orgTokenRow{}, fmt.Errorf("validate org token: %w", err)
	}

	return row, nil
}

const touchOrgTokensSQL = `
UPDATE org_tokens SET last_used_at = now()
 WHERE id = ANY($1::uuid[])
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`

func (s pgOrgTokenStore) touchOrgTokens(ctx context.Context, ids []pgtype.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	if _, err := s.pool.Exec(ctx, touchOrgTokensSQL, ids); err != nil {
		return fmt.Errorf("touch org tokens: %w", err)
	}

	return nil
}

// authenticateOrgToken turns a `bkro_` plaintext into a robot Principal.
func (s *Service) authenticateOrgToken(ctx context.Context, token string) (Principal, error) {
	if !looksLikeOrgToken(token) {
		return nil, ErrKeyInvalid
	}

	hash := HashToken(token)

	row, err := s.orgTokens.validateOrgToken(ctx, hash)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare(row.hash, hash) != 1 {
		return nil, ErrKeyInvalid
	}

	s.orgTokenToucher.mark(row.id)

	return &principal{
		// A robot is not a user and has no row in `users`. Every identity field below
		// is empty ON PURPOSE, not for want of a lookup: there is nothing to look up.
		// UserID is the zero UUID, which is why handleMe must answer a robot from its
		// grant and never from a user read.
		userID:      pgtype.UUID{},
		issuer:      "",
		subject:     "",
		email:       "",
		displayName: "",
		method:      MethodOrgToken,
		siteRole:    SiteRoleUser,
		orgs:        nil,
		projects:    nil,
		key:         nil,
		robot: &RobotGrant{
			RobotID: row.robotID,
			OrgID:   row.orgID,
			Scope:   row.scope,
		},
		maxScope: row.scope,
	}, nil
}

// MaxOrgTokenLifetime caps how far ahead an org token may expire.
//
// A robot deliberately survives its creator: it is an org-owned identity, not a
// delegation, so that a CI pipeline does not break the day the engineer who set
// it up leaves. The two revocation legs an API key gets for free -- the mint-time
// cap and the org_memberships cascade -- do not exist for it, and deleting the
// robot is a deliberate act nobody will remember to perform. Expiry is the
// countervailing control, and it is cheap here in a way it is not for a human:
// rotation is a scripted call against a secret store.
const MaxOrgTokenLifetime = 365 * 24 * time.Hour

// CreateOrgTokenInput is a request to mint a robot token.
type CreateOrgTokenInput struct {
	RobotID pgtype.UUID
	OrgID   pgtype.UUID
	Name    string
	Scope   Scope
	// ExpiresAt is REQUIRED -- org_tokens.expires_at is NOT NULL. See
	// MaxOrgTokenLifetime.
	ExpiresAt time.Time
}

// CreateOrgToken mints a `bkro_` token for a robot and returns the plaintext
// EXACTLY ONCE.
//
// The caller must already have been authorized as an org admin by the route
// guard; this adds the one check the guard cannot make, which is that a MACHINE
// did not ask.
func (s *Service) CreateOrgToken(
	ctx context.Context, p Principal, in CreateOrgTokenInput,
) (NewAPIKey, repository.CreateOrgTokenRow, error) {
	// NO TOKEN MINTS A TOKEN -- and here the rule earns its keep twice over, because
	// a robot token that could mint robot tokens would be a self-renewing,
	// org-wide, write-everywhere credential with no human in the loop at all.
	if err := requireMintAuthority(p); err != nil {
		return NewAPIKey{}, repository.CreateOrgTokenRow{}, err
	}

	if in.ExpiresAt.IsZero() {
		return NewAPIKey{}, repository.CreateOrgTokenRow{},
			errors.New("auth: an org token must have an expiry")
	}

	token, err := GenerateToken(OrgTokenPrefix)
	if err != nil {
		return NewAPIKey{}, repository.CreateOrgTokenRow{}, err
	}

	// created_by is the FK (ON DELETE SET NULL); created_by_email is the snapshot
	// that survives the human. The card that lists robots is the audit surface for a
	// write-everywhere credential, and an audit row that goes blank when its subject
	// leaves is the one row an auditor most wants attributed.
	row, err := s.store.CreateOrgToken(ctx, repository.CreateOrgTokenParams{
		RobotID:        in.RobotID,
		OrgID:          in.OrgID,
		Name:           in.Name,
		TokenSha256:    token.Hash,
		TokenPrefix:    token.Prefix,
		Scope:          in.Scope,
		ExpiresAt:      pgtype.Timestamptz{Time: in.ExpiresAt, InfinityModifier: pgtype.Finite, Valid: true},
		CreatedBy:      p.UserID(),
		CreatedByEmail: p.Email(),
	})
	if err != nil {
		return NewAPIKey{}, repository.CreateOrgTokenRow{}, fmt.Errorf("create org token: %w", err)
	}

	return token, row, nil
}
