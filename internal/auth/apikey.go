package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// API key format. `bkry_` + 32 bytes of CSPRNG output, base64url (unpadded), so
// the whole token is 5 + 43 = 48 characters of URL- and header-safe text.
const (
	// TokenPrefix is the human-visible marker. It exists so a leaked key is
	// greppable: secret scanners key on the prefix, and a developer who finds one
	// in a log knows immediately what it is and that it must be revoked.
	TokenPrefix = "bkry_"

	// tokenBytes is 256 bits. That is the entire reason the stored form is a plain
	// SHA-256 and not bcrypt or argon2: there is no low-entropy secret to stretch,
	// brute force is not on the table, and a KDF on a path that runs once per
	// sstate HEAD -- thousands of times in parallel at the start of every build --
	// would be a self-inflicted denial of service.
	tokenBytes = 32

	// displayLen is how much of the random part the console may keep, so it can
	// tell keys apart after the one-time reveal. The schema constrains
	// token_prefix to `^bkry_[A-Za-z0-9_-]{6,12}$`, so this must stay in 6..12.
	displayLen = 8
)

var (
	// ErrKeyInvalid means the presented key is not a live grant: malformed,
	// unknown, revoked, or expired. Coarse on purpose -- distinguishing "unknown"
	// from "revoked" to an unauthenticated caller is an oracle.
	ErrKeyInvalid = errors.New("auth: the API key is not valid")

	// ErrScopeExceedsRole means the requested key scope is beyond the authority of
	// the user's role in the project.
	ErrScopeExceedsRole = errors.New("auth: the key scope exceeds the member's role")
)

// NewAPIKey is a freshly minted key. Token is the plaintext, and this is the ONLY
// moment it exists: there is no plaintext column in the schema, so "shown exactly
// once" is enforced by the database's inability to represent the secret rather
// than by application discipline. Hand it to the user and drop it.
type NewAPIKey struct {
	Token  string
	Prefix string
	Hash   []byte
}

// GenerateAPIKey mints a key. crypto/rand, never math/rand: a math/rand key is
// predictable from a handful of samples, and this is the credential that guards
// every cache write.
func GenerateAPIKey() (NewAPIKey, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return NewAPIKey{}, fmt.Errorf("read random bytes: %w", err)
	}

	body := base64.RawURLEncoding.EncodeToString(buf)
	token := TokenPrefix + body

	return NewAPIKey{
		Token:  token,
		Prefix: TokenPrefix + body[:displayLen],
		Hash:   HashToken(token),
	}, nil
}

// HashToken is the stored form: SHA-256 of the FULL presented token, as 32 raw
// bytes. Not 64 hex characters -- half the index entry, a memcmp comparison, and
// no case-or-encoding bug that surfaces only as "my key stopped working".
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))

	return sum[:]
}

// TokenMatchesHash reports whether token hashes to want, in constant time.
//
// crypto/subtle, not bytes.Equal: bytes.Equal short-circuits on the first
// differing byte, and a comparison whose duration depends on how many leading
// bytes of a secret an attacker guessed correctly is a byte-at-a-time oracle for
// recovering it.
func TokenMatchesHash(token string, want []byte) bool {
	return subtle.ConstantTimeCompare(HashToken(token), want) == 1
}

// looksLikeAPIKey is a cheap shape check so a session cookie or a JWT that lands
// in the wrong header never reaches the database.
func looksLikeAPIKey(token string) bool {
	return strings.HasPrefix(token, TokenPrefix) && len(token) > len(TokenPrefix)+displayLen
}

// keyGrantRow is what validation reads. It is the whole grant: no joins, and
// nothing here needs a second query.
type keyGrantRow struct {
	id        pgtype.UUID
	userID    pgtype.UUID
	projectID pgtype.UUID
	scope     Scope
	hash      []byte
}

// keyStore is the consumer-side interface for key validation, so the hot path can
// be tested against a fake as well as against a real Postgres.
type keyStore interface {
	validateKey(ctx context.Context, hash []byte) (keyGrantRow, error)
	touchKeys(ctx context.Context, ids []pgtype.UUID) error
}

// pgKeyStore is the real one.
type pgKeyStore struct{ pool *pgxpool.Pool }

// validateKeySQL is THE hot query. It runs on every cache request, including
// every HEAD of a BB_NUMBER_THREADS-parallel sstate storm at the start of a
// build.
//
// ZERO JOINS, by construction. It rides api_keys_token_sha256_key -- a UNIQUE
// index on token_sha256 that INCLUDEs every other column named here -- so it is
// an Index Only Scan with Heap Fetches: 0, on a hit and on a miss alike. It must
// stay that way: joining project_memberships to "check the user is still a
// member" would double the probe count on the storm, and it is unnecessary,
// because the composite FK from api_keys to project_memberships means the row
// cannot outlive the membership. A user dropped from an OIDC group has their org
// membership deleted at reconciliation, which cascades org_memberships ->
// project_memberships -> api_keys. Revocation happens at RECONCILIATION time, by
// a cascade -- not at VALIDATION time, by a join.
//
// token_sha256 is selected back even though it is the predicate: it is the index
// key, so an index-only scan already has it in hand and returning it is free. It
// lets the final equality be re-affirmed in Go in constant time (see validateKey),
// which is belt-and-braces today and the thing that keeps this safe if the lookup
// is ever reshaped into a prefix probe.
const validateKeySQL = `
SELECT id, user_id, project_id, scope, token_sha256
  FROM api_keys
 WHERE token_sha256 = $1
   AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at > now())`

func (s pgKeyStore) validateKey(ctx context.Context, hash []byte) (keyGrantRow, error) {
	var row keyGrantRow

	err := s.pool.QueryRow(ctx, validateKeySQL, hash).
		Scan(&row.id, &row.userID, &row.projectID, &row.scope, &row.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return keyGrantRow{}, ErrKeyInvalid
	}

	if err != nil {
		return keyGrantRow{}, fmt.Errorf("validate api key: %w", err)
	}

	return row, nil
}

// touchKeysSQL is the COALESCED last_used_at write.
//
// It is never run on the request path. One CI machine drives a whole build with
// ONE key, so an inline `UPDATE api_keys SET last_used_at = now()` funnels
// thousands of parallel HEADs into a row-lock convoy on the single hottest row in
// the database -- one WAL record each -- to maintain a value nobody reads in real
// time. The flusher below batches it, at most once per key per interval.
const touchKeysSQL = `
UPDATE api_keys SET last_used_at = now()
 WHERE id = ANY($1::uuid[])
   AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`

func (s pgKeyStore) touchKeys(ctx context.Context, ids []pgtype.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	if _, err := s.pool.Exec(ctx, touchKeysSQL, ids); err != nil {
		return fmt.Errorf("touch api keys: %w", err)
	}

	return nil
}

// authenticateKey turns a presented token into a Principal, in one index probe.
//
// The Principal it builds is deliberately thin: it carries the key's grant and
// the owning user's id, and NOTHING else -- no email, no site role, no org
// memberships. Filling those in would mean a second query, on the hot path, to
// produce facts an API key must never be able to use anyway (see
// principal.IsSiteAdmin). One probe in, one authorization decision out.
func (s *Service) authenticateKey(ctx context.Context, token string) (Principal, error) {
	if !looksLikeAPIKey(token) {
		return nil, ErrKeyInvalid
	}

	hash := HashToken(token)

	row, err := s.keys.validateKey(ctx, hash)
	if err != nil {
		return nil, err
	}

	// The database already matched on equality; this re-affirms it in constant
	// time in our own address space, so the guarantee does not depend on a query
	// shape someone may change later.
	if subtle.ConstantTimeCompare(row.hash, hash) != 1 {
		return nil, ErrKeyInvalid
	}

	s.toucher.mark(row.id)

	return &principal{
		userID:      row.userID,
		issuer:      "",
		subject:     "",
		email:       "",
		displayName: "",
		method:      MethodAPIKey,
		// Explicitly the ordinary site role, whatever the owning user actually is.
		// A key is a delegation capped to one project and one scope; it must not
		// carry site admin with it.
		siteRole: SiteRoleUser,
		orgs:     nil,
		projects: nil,
		key: &KeyGrant{
			KeyID:     row.id,
			ProjectID: row.projectID,
			Scope:     row.scope,
		},
	}, nil
}

// keyToucher coalesces last_used_at updates off the request path.
//
// mark() is what the hot path calls: one mutex-guarded map insert, no I/O. The
// flusher drains the set on a timer and issues ONE statement for the whole batch.
type keyToucher struct {
	mu      sync.Mutex
	pending map[pgtype.UUID]struct{}
	store   keyStore
}

func newKeyToucher(store keyStore) *keyToucher {
	return &keyToucher{pending: make(map[pgtype.UUID]struct{}), store: store}
}

func (t *keyToucher) mark(id pgtype.UUID) {
	t.mu.Lock()
	t.pending[id] = struct{}{}
	t.mu.Unlock()
}

func (t *keyToucher) drain() []pgtype.UUID {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.pending) == 0 {
		return nil
	}

	ids := make([]pgtype.UUID, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}

	t.pending = make(map[pgtype.UUID]struct{})

	return ids
}

// flush writes one batched UPDATE for every key seen since the last flush.
func (t *keyToucher) flush(ctx context.Context) error {
	return t.store.touchKeys(ctx, t.drain())
}

// StartKeyToucher runs the coalescing last_used_at flusher until ctx is
// cancelled. Wire it to the server's shutdown context.
func (s *Service) StartKeyToucher(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.toucher.flush(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.ErrorContext(ctx, "api key last_used_at flush failed", slog.Any("error", err))
			}
		}
	}
}
