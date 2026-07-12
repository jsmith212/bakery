package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// sessionUserKey is the only thing we put in the session: the user's id.
//
// Deliberately not the roles. Every authorization fact -- site role, org
// memberships, project roles -- is read live from the tables on each console
// request, so a demotion or an org removal takes effect on the user's very next
// request with no session-invalidation machinery at all. Caching roles in the
// session would buy a few milliseconds on a cold path and cost us a revocation
// story. (API keys make the opposite trade, and must: they are on the HEAD hot
// path, so their grant is self-contained and revocation happens by cascade at
// reconciliation time instead.)
//
// It is a string because scs's default codec is encoding/gob: anything beyond a
// primitive needs gob.Register, and a registration we forget is a runtime panic
// in a session decode.
const sessionUserKey = "user_id"

// SessionStore is an scs.Store over pgxpool.
//
// scs ships a postgresstore, and it is database/sql. So is the pgx/v5/stdlib
// bridge its README suggests. Both are forbidden here, so this is hand-written
// against the pgxpool the rest of the process already owns.
//
// It implements BOTH method sets, and both are required:
//
//   - the plain three (Find/Commit/Delete) because SessionManager.Store is typed
//     scs.Store, so without them a *SessionStore is not assignable to it and the
//     code does not compile;
//   - the *Ctx three because those are what scs actually CALLS -- it dispatches
//     on anonymous single-method interfaces (`s.Store.(interface{ FindCtx(...) })`),
//     not on scs.CtxStore. Omit them and it silently falls back to the plain
//     forms, and every session query loses cancellation and deadline propagation.
type SessionStore struct {
	q   *repository.Queries
	log *slog.Logger
}

// The compiler is the thing that keeps all four contracts honest.
var (
	_ scs.Store            = (*SessionStore)(nil)
	_ scs.CtxStore         = (*SessionStore)(nil)
	_ scs.IterableStore    = (*SessionStore)(nil)
	_ scs.IterableCtxStore = (*SessionStore)(nil)
)

// NewSessionStore builds the store over an existing pool.
func NewSessionStore(pool *pgxpool.Pool, log *slog.Logger) *SessionStore {
	return &SessionStore{q: repository.New(pool), log: log}
}

// FindCtx returns the session data for token.
//
// A missing OR expired token is a clean MISS -- (nil, false, nil) -- and NOT an
// error. Returning an error here makes scs's ErrorFunc fire a 500 on every
// request from a logged-out user, which is to say on every request to the login
// page.
func (s *SessionStore) FindCtx(ctx context.Context, token string) ([]byte, bool, error) {
	data, err := s.q.SessionFind(ctx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("session find: %w", err)
	}

	return data, true, nil
}

// CommitCtx upserts the session.
func (s *SessionStore) CommitCtx(ctx context.Context, token string, b []byte, expiry time.Time) error {
	err := s.q.SessionCommit(ctx, repository.SessionCommitParams{
		Token:  token,
		Data:   b,
		Expiry: pgtype.Timestamptz{Time: expiry, InfinityModifier: pgtype.Finite, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("session commit: %w", err)
	}

	return nil
}

// DeleteCtx removes the session. Deleting a token that is not there is a
// success: logout must be idempotent.
func (s *SessionStore) DeleteCtx(ctx context.Context, token string) error {
	if err := s.q.SessionDelete(ctx, token); err != nil {
		return fmt.Errorf("session delete: %w", err)
	}

	return nil
}

// AllCtx returns every live session. Only SessionManager.Iterate uses it.
func (s *SessionStore) AllCtx(ctx context.Context) (map[string][]byte, error) {
	rows, err := s.q.SessionAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("session all: %w", err)
	}

	out := make(map[string][]byte, len(rows))
	for _, row := range rows {
		out[row.Token] = row.Data
	}

	return out, nil
}

// The context-free forms exist for assignability to scs.Store. scs prefers the
// *Ctx forms above and will not call these in the normal request path.
func (s *SessionStore) Find(token string) ([]byte, bool, error) {
	return s.FindCtx(context.Background(), token)
}

func (s *SessionStore) Commit(token string, b []byte, expiry time.Time) error {
	return s.CommitCtx(context.Background(), token, b, expiry)
}

func (s *SessionStore) Delete(token string) error {
	return s.DeleteCtx(context.Background(), token)
}

func (s *SessionStore) All() (map[string][]byte, error) {
	return s.AllCtx(context.Background())
}

// StartCleanup reaps expired sessions until ctx is cancelled.
//
// Expired rows are already invisible to FindCtx (the query filters on expiry), so
// this is hygiene, not correctness: without it the table grows without bound.
// Run it in a goroutine tied to the server's shutdown context.
func (s *SessionStore) StartCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.DeleteExpired(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.ErrorContext(ctx, "session cleanup failed", slog.Any("error", err))
			}
		}
	}
}

// DeleteExpired removes every expired session and reports how many.
func (s *SessionStore) DeleteExpired(ctx context.Context) error {
	if _, err := s.q.SessionDeleteExpired(ctx); err != nil {
		return fmt.Errorf("session delete expired: %w", err)
	}

	return nil
}

// NewSessionManager builds the scs manager over the store.
//
// secure controls the Secure cookie attribute. It must be true in production and
// cannot be true in local HTTP development, so it is a parameter rather than a
// constant.
func NewSessionManager(store scs.Store, secure bool) *scs.SessionManager {
	sm := scs.New()
	sm.Store = store
	sm.Lifetime = 12 * time.Hour
	sm.IdleTimeout = 2 * time.Hour

	sm.Cookie.Name = "bakery_session"
	sm.Cookie.Path = "/"
	sm.Cookie.HttpOnly = true // the credential must be unreachable from JS
	sm.Cookie.Secure = secure
	// Lax, not None: it blocks the cross-site POST, which is the CSRF shape that
	// matters for a state-changing API. The API layer additionally requires a JSON
	// content type on mutations, which a cross-site HTML form cannot set without
	// tripping a CORS preflight.
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Persist = true

	// The cookie's token is hashed before it is stored, so a leaked database dump
	// does not hand over live session cookies.
	sm.HashTokenInStore = true

	return sm
}
