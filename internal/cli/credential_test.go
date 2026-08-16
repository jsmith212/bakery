package cli

import "testing"

// ---------------------------------------------------------------------------
// resolveServer: --server > BAKERY_SERVER (already folded into flagServer by
// Kong before this runs) > credentials.json's default_server > localhost.
// ---------------------------------------------------------------------------

func TestResolveServer(t *testing.T) {
	tests := []struct {
		name          string
		flagServer    string
		defaultServer string // "" means never call SetDefaultServer
		want          string
	}{
		{
			name: "nothing set anywhere falls back to localhost",
			want: "http://localhost:8080",
		},
		{
			name:       "an explicit --server (or BAKERY_SERVER, already folded by Kong) wins outright",
			flagServer: "https://flag.example.com",
			// A default_server is ALSO set below by the harness, and must lose.
			defaultServer: "https://default.example.com",
			want:          "https://flag.example.com",
		},
		{
			name:          "with no flag, the stored default_server wins over localhost",
			defaultServer: "https://default.example.com",
			want:          "https://default.example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := tempStore(t)

			if tc.defaultServer != "" {
				if err := store.SetDefaultServer(tc.defaultServer); err != nil {
					t.Fatalf("SetDefaultServer: %v", err)
				}
			}

			if got := resolveServer(tc.flagServer, store); got != tc.want {
				t.Errorf("resolveServer(%q) = %q, want %q", tc.flagServer, got, tc.want)
			}
		})
	}
}

// TestResolveServerWithNoStore covers the (defensive) nil-store case: it must
// not panic, and must still fall through to the localhost default.
func TestResolveServerWithNoStore(t *testing.T) {
	if got := resolveServer("", nil); got != "http://localhost:8080" {
		t.Errorf("resolveServer with a nil store = %q, want the localhost default", got)
	}

	if got := resolveServer("https://explicit.example.com", nil); got != "https://explicit.example.com" {
		t.Errorf("resolveServer with a nil store did not honour an explicit flag: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// resolveCacheCredential: --key/BAKERY_API_KEY (already folded into flagKey)
// > stored "org/project" key > stored "org" key/robot token > stored user
// token > empty (falls through to Client.authorize's bearer path).
// ---------------------------------------------------------------------------

func TestResolveCacheCredentialPrecedence(t *testing.T) {
	const server = "https://bakery.example.com"

	newFullyStockedStore := func(t *testing.T) *TokenStore {
		t.Helper()

		store, _ := tempStore(t)

		if err := store.PutUserToken(server, "bkru_user_token"); err != nil {
			t.Fatalf("PutUserToken: %v", err)
		}

		if err := store.PutKey(server, "acme", "bkro_org_token"); err != nil {
			t.Fatalf("PutKey(org): %v", err)
		}

		if err := store.PutKey(server, "acme/firmware", "bkry_project_key"); err != nil {
			t.Fatalf("PutKey(org/project): %v", err)
		}

		return store
	}

	t.Run("an explicit flag key wins over everything cached", func(t *testing.T) {
		store := newFullyStockedStore(t)

		got := resolveCacheCredential(store, server, "acme", "firmware", "bkry_explicit")
		if got.Key != "bkry_explicit" {
			t.Errorf("Key = %q, want the explicit flag value", got.Key)
		}
	})

	t.Run("with no flag, a stored org/project key wins over the org token and the user token", func(t *testing.T) {
		store := newFullyStockedStore(t)

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "bkry_project_key" {
			t.Errorf("Key = %q, want the stored org/project key", got.Key)
		}
	})

	t.Run("with no project key, a stored org/robot token wins over the user token", func(t *testing.T) {
		store, _ := tempStore(t)

		if err := store.PutUserToken(server, "bkru_user_token"); err != nil {
			t.Fatalf("PutUserToken: %v", err)
		}

		if err := store.PutKey(server, "acme", "bkro_org_token"); err != nil {
			t.Fatalf("PutKey(org): %v", err)
		}

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "bkro_org_token" {
			t.Errorf("Key = %q, want the stored org token", got.Key)
		}
	})

	t.Run("with nothing project- or org-scoped, the stored user token is used", func(t *testing.T) {
		store, _ := tempStore(t)

		if err := store.PutUserToken(server, "bkru_user_token"); err != nil {
			t.Fatalf("PutUserToken: %v", err)
		}

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "bkru_user_token" {
			t.Errorf("Key = %q, want the stored user token", got.Key)
		}
	})

	t.Run("a key stored for a DIFFERENT org/project is never used", func(t *testing.T) {
		store, _ := tempStore(t)

		if err := store.PutKey(server, "other/project", "bkry_other"); err != nil {
			t.Fatalf("PutKey: %v", err)
		}

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "" {
			t.Errorf("Key = %q, want empty -- a differently-scoped key must not leak in", got.Key)
		}
	})

	t.Run("a key stored for a DIFFERENT server is never used", func(t *testing.T) {
		store, _ := tempStore(t)

		if err := store.PutKey("https://other.example.com", "acme/firmware", "bkry_other_server"); err != nil {
			t.Fatalf("PutKey: %v", err)
		}

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "" {
			t.Errorf("Key = %q, want empty -- credentials.json is keyed per server", got.Key)
		}
	})

	t.Run("nothing cached and no flag: empty Key falls through to the bearer path", func(t *testing.T) {
		store, _ := tempStore(t)

		got := resolveCacheCredential(store, server, "acme", "firmware", "")
		if got.Key != "" {
			t.Errorf("Key = %q, want empty (Client.authorize falls back to the OIDC bearer)", got.Key)
		}
	})

	t.Run("a nil store never panics and behaves like an empty one", func(t *testing.T) {
		got := resolveCacheCredential(nil, server, "acme", "firmware", "")
		if got.Key != "" {
			t.Errorf("Key = %q, want empty", got.Key)
		}

		got = resolveCacheCredential(nil, server, "acme", "firmware", "bkry_explicit")
		if got.Key != "bkry_explicit" {
			t.Errorf("Key = %q, want the explicit flag value even with a nil store", got.Key)
		}
	})
}
