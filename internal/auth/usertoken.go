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

// Personal access tokens (`bkru_`): the credential a human hands a headless
// machine so it can act AS THEM.
//
// The difference from an API key is not the prefix, it is WHEN authority is
// resolved. An api_keys row is a snapshot -- one project, one scope, capped at
// mint time and revoked by an FK cascade. A user token resolves the holder's LIVE
// role set on every request, so "Anna lost her acme membership" narrows her token
// on the very next HEAD rather than at her next login.
//
// Live has a cost, and the whole of this file is about paying it once instead of
// three times per request: ONE round trip (an Index Only Scan on
// user_tokens_token_sha256_key joined to users on the primary key) returns both
// the grant and the authorization EPOCH, and the epoch keys an in-process cache
// of the role set. See principalcache.go for why the epoch, and not an Evict()
// call, is what makes that sound.

// looksLikeUserToken is this validator's own shape gate.
//
// tokenKind already routed on the prefix; this re-applies the length floor, the
// same way authenticateKey does. Each validator owning its own shape rules is
// what lets the FAMILY gate (LooksLikeBakeryToken) stay deliberately loose --
// loose enough that a malformed Bakery token is refused rather than silently
// reclassified as a foreign credential.
func looksLikeUserToken(token string) bool {
	return strings.HasPrefix(token, UserTokenPrefix) && len(token) > len(UserTokenPrefix)+displayLen
}

// userTokenRow is what the probe reads. It is the whole grant plus the epoch: no
// second query is needed to decide anything.
type userTokenRow struct {
	id       pgtype.UUID
	userID   pgtype.UUID
	maxScope Scope
	hash     []byte
	epoch    int64
}

// userTokenStore is the consumer-side interface for the hot probe, so it can be
// driven against a fake as well as a real Postgres.
type userTokenStore interface {
	validateUserToken(ctx context.Context, hash []byte) (userTokenRow, error)
	touchUserTokens(ctx context.Context, ids []pgtype.UUID) error
}

type pgUserTokenStore struct{ pool *pgxpool.Pool }

// validateUserTokenSQL is ONE ROUND TRIP and must stay one.
//
// The JOIN looks like the thing the api_keys probe is famous for refusing, and it
// is not the same shape at all: that ban is on joining project_memberships to
// re-check authority, which would be a second index traversal answering a question
// the cascade already answers. This join is `users` on its PRIMARY KEY, reading
// one bigint that the cache key needs. An Index Only Scan on
// user_tokens_token_sha256_key (Heap Fetches: 0, hit and miss alike) plus one PK
// lookup, in one statement, on one connection.
//
// Splitting it into two statements would be strictly worse in the way that
// matters: the token row and the epoch would come from two different snapshots,
// so a role change landing between them could be missed entirely -- the cache
// would be filled under the OLD epoch with the NEW roles, or the reverse.
//
// token_sha256 is selected back for the same reason api_keys does it: the final
// equality is re-affirmed in constant time in our own address space, so the
// guarantee does not depend on a query shape someone may reshape later.
const validateUserTokenSQL = `
SELECT t.id, t.user_id, t.max_scope, t.token_sha256, u.authz_epoch
  FROM user_tokens t
  JOIN users u ON u.id = t.user_id
 WHERE t.token_sha256 = $1
   AND t.revoked_at IS NULL
   AND (t.expires_at IS NULL OR t.expires_at > now())`

func (s pgUserTokenStore) validateUserToken(ctx context.Context, hash []byte) (userTokenRow, error) {
	var row userTokenRow

	err := s.pool.QueryRow(ctx, validateUserTokenSQL, hash).
		Scan(&row.id, &row.userID, &row.maxScope, &row.hash, &row.epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return userTokenRow{}, ErrKeyInvalid
	}

	if err != nil {
		return userTokenRow{}, fmt.Errorf("validate user token: %w", err)
	}

	return row, nil
}

// touchUserTokensSQL is the COALESCED last_used_at write, never run inline. See
// apikey.go's touchKeysSQL for the row-lock convoy this avoids.
const touchUserTokensSQL = `
UPDATE user_tokens SET last_used_at = now()
 WHERE id = ANY($1::uuid[])
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`

func (s pgUserTokenStore) touchUserTokens(ctx context.Context, ids []pgtype.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	if _, err := s.pool.Exec(ctx, touchUserTokensSQL, ids); err != nil {
		return fmt.Errorf("touch user tokens: %w", err)
	}

	return nil
}

// authenticateUserToken turns a `bkru_` plaintext into a live-role Principal.
//
// Revocation is INSTANT and does not go through the cache: the probe is always a
// real database read, and `revoked_at IS NULL` is one of its predicates. Only the
// ROLE SET is cached, and only under a key the database invalidates.
func (s *Service) authenticateUserToken(ctx context.Context, token string) (Principal, error) {
	if !looksLikeUserToken(token) {
		return nil, ErrKeyInvalid
	}

	hash := HashToken(token)

	row, err := s.userTokens.validateUserToken(ctx, hash)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare(row.hash, hash) != 1 {
		return nil, ErrKeyInvalid
	}

	s.userTokenToucher.mark(row.id)

	authz, err := s.principals.load(ctx, row.userID, row.epoch, s.loadUserAuthz)
	if err != nil {
		return nil, err
	}

	// The owner was deleted between the probe's JOIN and this read. A credential
	// with no principal behind it is not a credential.
	if authz == nil {
		return nil, ErrKeyInvalid
	}

	// MethodUserToken is non-interactive, so IsSiteAdmin, CanAdminOrg, CanOwnOrg and
	// CanAdminProject all refuse it even though siteRole and the orgs map are fully
	// populated here. That is deliberate and it is where the "a delegation must not
	// become a master key" rule is enforced -- see principal.actsAsUser.
	return authz.principal(MethodUserToken, row.maxScope, nil), nil
}

// isNoRows is the one place "the row is gone" is recognised, so principalcache's
// negative-entry decision does not have to import pgx.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// CreateUserTokenInput is a request to mint a personal access token.
//
// Note what is NOT here, exactly as in CreateKeyInput: a user_id. A token is
// always minted for the CALLER. "Mint a token on behalf of another user" is a
// credential-forging primitive, and there is nothing to plumb it into.
type CreateUserTokenInput struct {
	Name     string
	MaxScope Scope
	// ExpiresAt is optional; nil means never. The console defaults to 90 days.
	ExpiresAt *time.Time
}

// CreateUserToken mints a `bkru_` token for the calling principal and returns the
// plaintext EXACTLY ONCE.
//
// There is no scope cap against a role here, and that is not an omission. An API
// key names one project, so its scope can be capped against the caller's role in
// that project. A user token names no project: its ceiling IS the caller's own
// live authority, re-evaluated on every request, and `max_scope` only narrows
// that further. A read-only human minting a `write` token gets a token that can
// write exactly nothing -- the ceiling is a floor-less cap, not a grant.
func (s *Service) CreateUserToken(
	ctx context.Context, p Principal, in CreateUserTokenInput,
) (NewAPIKey, repository.CreateUserTokenRow, error) {
	// NO TOKEN MINTS A TOKEN. See requireMintAuthority: an API key, a personal
	// access token and a robot are all refused, because minting is an interactive
	// act, always.
	if err := requireMintAuthority(p); err != nil {
		return NewAPIKey{}, repository.CreateUserTokenRow{}, err
	}

	token, err := GenerateToken(UserTokenPrefix)
	if err != nil {
		return NewAPIKey{}, repository.CreateUserTokenRow{}, err
	}

	expires := pgtype.Timestamptz{Time: time.Time{}, InfinityModifier: pgtype.Finite, Valid: false}
	if in.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *in.ExpiresAt, InfinityModifier: pgtype.Finite, Valid: true}
	}

	row, err := s.store.CreateUserToken(ctx, repository.CreateUserTokenParams{
		UserID:      p.UserID(),
		Name:        in.Name,
		TokenSha256: token.Hash,
		TokenPrefix: token.Prefix,
		MaxScope:    in.MaxScope,
		ExpiresAt:   expires,
	})
	if err != nil {
		return NewAPIKey{}, repository.CreateUserTokenRow{}, fmt.Errorf("create user token: %w", err)
	}

	return token, row, nil
}
