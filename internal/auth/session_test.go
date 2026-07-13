package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// mustParse is the cookie jar's key.
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}

	return u
}

func newStore(t *testing.T) *SessionStore {
	t.Helper()

	return NewSessionStore(dbtest.New(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestSessionStoreSatisfiesScs is a compile-time fact made visible. scs dispatches
// on ANONYMOUS single-method interfaces, not on scs.CtxStore, so a store that
// implements only the plain three still compiles into SessionManager and then
// silently drops every context -- no cancellation, no deadline, on every session
// query. Both method sets are mandatory.
func TestSessionStoreSatisfiesScs(t *testing.T) {
	t.Parallel()

	var store any = &SessionStore{}

	checks := map[string]bool{}

	_, checks["scs.Store"] = store.(scs.Store)
	_, checks["scs.CtxStore"] = store.(scs.CtxStore)
	_, checks["scs.IterableStore"] = store.(scs.IterableStore)
	_, checks["scs.IterableCtxStore"] = store.(scs.IterableCtxStore)

	// These are the anonymous interfaces scs ACTUALLY type-asserts against
	// (data.go). Satisfying scs.CtxStore is not what makes them fire; having the
	// methods is.
	_, checks["FindCtx"] = store.(interface {
		FindCtx(ctx context.Context, token string) ([]byte, bool, error)
	})
	_, checks["CommitCtx"] = store.(interface {
		CommitCtx(ctx context.Context, token string, b []byte, expiry time.Time) error
	})
	_, checks["DeleteCtx"] = store.(interface {
		DeleteCtx(ctx context.Context, token string) error
	})

	for name, ok := range checks {
		if !ok {
			t.Errorf("*SessionStore does not satisfy %s", name)
		}
	}
}

func TestSessionStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	ctx := t.Context()

	// A token that is not there is a clean MISS, not an error. Returning an error
	// here makes scs's ErrorFunc fire a 500 on every request from a logged-out
	// user -- which is to say, on the login page.
	data, found, err := store.FindCtx(ctx, "nobody")
	if err != nil || found || data != nil {
		t.Fatalf("FindCtx(missing) = (%v, %v, %v), want (nil, false, nil)", data, found, err)
	}

	if err := store.CommitCtx(ctx, "tok", []byte("payload"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CommitCtx() error = %v", err)
	}

	data, found, err = store.FindCtx(ctx, "tok")
	if err != nil || !found || string(data) != "payload" {
		t.Fatalf("FindCtx() = (%q, %v, %v), want (payload, true, nil)", data, found, err)
	}

	// ON CONFLICT overwrite.
	if err := store.CommitCtx(ctx, "tok", []byte("payload-2"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CommitCtx() overwrite error = %v", err)
	}

	data, _, _ = store.FindCtx(ctx, "tok")
	if string(data) != "payload-2" {
		t.Fatalf("FindCtx() after overwrite = %q, want payload-2", data)
	}

	// Delete, and deleting again, are both successes: logout must be idempotent.
	for range 2 {
		if err := store.DeleteCtx(ctx, "tok"); err != nil {
			t.Fatalf("DeleteCtx() error = %v", err)
		}
	}

	if _, found, _ := store.FindCtx(ctx, "tok"); found {
		t.Fatal("FindCtx() found a deleted session")
	}
}

// TestSessionStoreExcludesExpired: an expired session must be invisible, and it
// must be invisible WITHOUT anyone having run the reaper -- the query filters on
// expiry, so cleanup is hygiene, not correctness.
func TestSessionStoreExpiry(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	ctx := t.Context()

	if err := store.CommitCtx(ctx, "live", []byte("a"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CommitCtx() error = %v", err)
	}

	if err := store.CommitCtx(ctx, "dead", []byte("b"), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CommitCtx() error = %v", err)
	}

	if _, found, _ := store.FindCtx(ctx, "dead"); found {
		t.Error("FindCtx() returned an EXPIRED session")
	}

	all, err := store.AllCtx(ctx)
	if err != nil {
		t.Fatalf("AllCtx() error = %v", err)
	}

	if len(all) != 1 {
		t.Errorf("AllCtx() = %d sessions, want 1 (the expired one must be excluded)", len(all))
	}

	// The reaper removes the row so the table does not grow without bound.
	if err := store.DeleteExpired(ctx); err != nil {
		t.Fatalf("DeleteExpired() error = %v", err)
	}

	if _, found, _ := store.FindCtx(ctx, "live"); !found {
		t.Error("DeleteExpired() reaped a LIVE session")
	}
}

// TestSessionCleanupStopsOnCancel: the goroutine is tied to the server's shutdown
// context and must actually exit, or graceful shutdown hangs.
func TestSessionCleanupStopsOnCancel(t *testing.T) {
	t.Parallel()

	store := newStore(t)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		store.StartCleanup(ctx, 10*time.Millisecond)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCleanup() did not return when its context was cancelled")
	}
}

// TestSessionThroughARealManager drives a login/read/logout cycle through a real
// scs.SessionManager, a real ServeMux and a real cookie jar -- so the cookie
// plumbing, the gob codec and the store are all exercised together.
func TestSessionThroughARealManager(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	sm := NewSessionManager(store, false)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if err := sm.RenewToken(r.Context()); err != nil {
			t.Errorf("RenewToken: %v", err)
		}

		sm.Put(r.Context(), sessionUserKey, "the-user")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sm.GetString(r.Context(), sessionUserKey)))
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if err := sm.Destroy(r.Context()); err != nil {
			t.Errorf("Destroy: %v", err)
		}

		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(sm.LoadAndSave(mux))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	client := &http.Client{Jar: jar}

	do := func(method, path string) string {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		return string(body)
	}

	// Anonymous.
	if got := do("GET", "/me"); got != "" {
		t.Errorf("GET /me before login = %q, want empty", got)
	}

	do("POST", "/login")

	if got := do("GET", "/me"); got != "the-user" {
		t.Errorf("GET /me after login = %q, want the-user", got)
	}

	do("POST", "/logout")

	if got := do("GET", "/me"); got != "" {
		t.Errorf("GET /me after logout = %q, want empty", got)
	}

	// HashTokenInStore: the cookie's token is NOT the primary key in the table, so
	// a database dump does not hand over live session cookies.
	all, err := store.AllCtx(t.Context())
	if err != nil {
		t.Fatalf("AllCtx: %v", err)
	}

	for _, cookie := range jar.Cookies(mustParse(t, server.URL)) {
		if cookie.Name != "bakery_session" {
			continue
		}

		if _, ok := all[cookie.Value]; ok {
			t.Error("the session token in the cookie is stored verbatim; HashTokenInStore is not on")
		}
	}
}
