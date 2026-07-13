package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthenticateCache is the Basic-scheme bridge BitBake needs. The whole point is
// that a key presented as HTTP Basic resolves to the SAME principal the Bearer arm
// would build, from EITHER Basic field, in constant time, and that the token never
// reaches a log line.
func TestAuthenticateCache(t *testing.T) {
	good, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}

	store := newFakeKeyStore()
	store.put(good.Hash, keyGrantRow{
		id: uuid(0xff), userID: uuid(0x01), projectID: projectA, scope: ScopeWrite, hash: nil,
	})

	newSvc := func() *Service {
		return &Service{keys: store, toucher: newKeyToucher(store)}
	}

	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	t.Run("token in the password field authenticates", func(t *testing.T) {
		svc := newSvc()
		r := httptest.NewRequest(http.MethodHead, "/cache/acme/widget/sstate/x", nil)
		r.Header.Set("Authorization", basic("bkry", good.Token))

		p, err := svc.AuthenticateCache(r.Context(), r)
		if err != nil {
			t.Fatalf("AuthenticateCache() error = %v", err)
		}

		if p.Method() != MethodAPIKey {
			t.Errorf("Method() = %q, want %q", p.Method(), MethodAPIKey)
		}

		if !p.CanWriteProject(orgA, projectA) {
			t.Error("a write-scoped key could not write its own project")
		}
	})

	t.Run("token in the username field authenticates", func(t *testing.T) {
		svc := newSvc()
		r := httptest.NewRequest(http.MethodHead, "/cache/acme/widget/sstate/x", nil)
		// http://<token>@host produces Basic base64(<token>:) -- token in the user field.
		r.Header.Set("Authorization", basic(good.Token, ""))

		p, err := svc.AuthenticateCache(r.Context(), r)
		if err != nil {
			t.Fatalf("AuthenticateCache() error = %v", err)
		}

		if p.Method() != MethodAPIKey {
			t.Errorf("Method() = %q, want %q", p.Method(), MethodAPIKey)
		}
	})

	t.Run("an unknown key is refused", func(t *testing.T) {
		other, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() error = %v", err)
		}

		svc := newSvc()
		r := httptest.NewRequest(http.MethodHead, "/cache/acme/widget/sstate/x", nil)
		r.Header.Set("Authorization", basic("bkry", other.Token))

		if _, err := svc.AuthenticateCache(r.Context(), r); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("AuthenticateCache() with an unknown key = %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("no credential is unauthenticated, not a panic", func(t *testing.T) {
		svc := newSvc()
		// No Authorization header and a context that never went through LoadAndSave:
		// the delegated arm must not touch scs.
		r := httptest.NewRequest(http.MethodHead, "/cache/acme/widget/sstate/x", nil)

		if _, err := svc.AuthenticateCache(r.Context(), r); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("AuthenticateCache() with no credential = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("the token never appears in a log line", func(t *testing.T) {
		var buf bytes.Buffer
		svc := newSvc()
		svc.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		r := httptest.NewRequest(http.MethodHead, "/cache/acme/widget/sstate/x", nil)
		r.Header.Set("Authorization", basic("bkry", good.Token))

		if _, err := svc.AuthenticateCache(r.Context(), r); err != nil {
			t.Fatalf("AuthenticateCache() error = %v", err)
		}

		if strings.Contains(buf.String(), good.Token) {
			t.Error("the API key token leaked into a log line")
		}
	})
}
