package cli

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeJWT builds a JWT with the given `exp`. It is NOT signed and does not need
// to be: idTokenExpiry deliberately does not verify anything -- the server does
// that, and it is the only party whose opinion counts. This exercises the exact
// parse the CLI performs.
func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()

	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(raw)
	}

	header := b64(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := b64(map[string]any{"exp": exp.Unix(), "sub": "user-1"})

	return header + "." + payload + ".c2ln"
}

// tempStore points the token cache at a throwaway XDG_CONFIG_HOME. It also
// proves, by construction, that XDG_CONFIG_HOME is honoured -- every test below
// would write to the developer's real ~/.config if it were not.
func tempStore(t *testing.T) (*TokenStore, string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	store, err := NewTokenStore()
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	want := filepath.Join(home, "bakery", "credentials.json")
	if store.Path() != want {
		t.Fatalf("token cache path = %q, want %q (XDG_CONFIG_HOME is not being honoured)", store.Path(), want)
	}

	return store, home
}

// TestTokenCacheIsNotWorldReadable is the security test in this file.
//
// The cache holds a live ID token (a bearer credential for the whole control
// plane) and a refresh token (a credential for minting more of them). If it lands
// 0644 on a shared build host, every account on the box can read the user's
// identity, and nothing about the CLI's behaviour would tell them so. The
// directory matters as much as the file: a 0755 directory with a 0600 file inside
// still leaks the file's existence and lets a hostile process replace it.
func TestTokenCacheIsNotWorldReadable(t *testing.T) {
	store, home := tempStore(t)

	if err := store.Put("https://bakery.example.com", Token{
		IDToken: "id", AccessToken: "at", RefreshToken: "rt",
		Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fi, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat the token cache: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("token cache mode = %04o, want 0600 -- the token is readable by other users", got)
	}

	di, err := os.Stat(filepath.Join(home, "bakery"))
	if err != nil {
		t.Fatalf("stat the config dir: %v", err)
	}

	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("config dir mode = %04o, want 0700", got)
	}
}

// TestTokenCacheTightensAPreExistingLooseDirectory covers the upgrade path: a
// directory left behind at 0755 by an earlier version (or by `mkdir -p`) must be
// tightened, not trusted. MkdirAll applies its mode only to directories it
// CREATES, so a plain MkdirAll would silently leave it wide open.
func TestTokenCacheTightensAPreExistingLooseDirectory(t *testing.T) {
	store, home := tempStore(t)

	dir := filepath.Join(home, "bakery")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := store.Put("https://bakery.example.com", Token{IDToken: "id"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("config dir mode = %04o, want 0700 -- a pre-existing loose dir was not tightened", got)
	}
}

func TestTokenCacheRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		server string
		lookup string
		want   bool
	}{
		{name: "exact", server: "https://a.example.com", lookup: "https://a.example.com", want: true},
		{
			name: "trailing slash is the same server", server: "https://a.example.com",
			lookup: "https://a.example.com/", want: true,
		},
		{
			name: "a different server is a different entry", server: "https://a.example.com",
			lookup: "https://b.example.com", want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A private store per subtest: these must not share a file.
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)

			s, err := NewTokenStore()
			if err != nil {
				t.Fatalf("NewTokenStore: %v", err)
			}

			if err := s.Put(tc.server, Token{IDToken: "id-token"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			got, ok := s.Get(tc.lookup)
			if ok != tc.want {
				t.Fatalf("Get(%q) found = %v, want %v", tc.lookup, ok, tc.want)
			}

			if tc.want && got.IDToken != "id-token" {
				t.Errorf("IDToken = %q, want %q", got.IDToken, "id-token")
			}
		})
	}
}

// TestTokenCacheDeleteIsIdempotent: logging out twice, or logging out when you
// were never signed in, is not an error.
func TestTokenCacheDeleteIsIdempotent(t *testing.T) {
	store, _ := tempStore(t)

	had, err := store.Delete("https://nobody.example.com")
	if err != nil {
		t.Fatalf("Delete on an empty cache: %v", err)
	}

	if had {
		t.Error("Delete on an empty cache reported a token, want none")
	}

	if err := store.Put("https://a.example.com", Token{IDToken: "id"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	had, err = store.Delete("https://a.example.com")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if !had {
		t.Error("Delete reported no token, want one")
	}

	if _, ok := store.Get("https://a.example.com"); ok {
		t.Error("the token survived Delete")
	}

	had, err = store.Delete("https://a.example.com")
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	if had {
		t.Error("the second Delete reported a token")
	}
}

// TestTokenCacheIsPerServer: staging and production are separate logins. Signing
// out of one must not sign you out of the other.
func TestTokenCacheIsPerServer(t *testing.T) {
	store, _ := tempStore(t)

	if err := store.Put("https://staging.example.com", Token{IDToken: "staging"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.Put("https://prod.example.com", Token{IDToken: "prod"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := store.Delete("https://staging.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, ok := store.Get("https://prod.example.com")
	if !ok || got.IDToken != "prod" {
		t.Fatalf("prod token = %q found=%v, want %q true", got.IDToken, ok, "prod")
	}

	// And the surviving entry must still be 0600.
	fi, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := fi.Mode().Perm(); got != fs.FileMode(0o600) {
		t.Errorf("mode after rewrite = %04o, want 0600", got)
	}
}

func TestTokenStale(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		expiry time.Time
		want   bool
	}{
		{name: "fresh", expiry: now.Add(time.Hour), want: false},
		{name: "expired", expiry: now.Add(-time.Second), want: true},
		{
			// Inside the stale window: valid now, dead by the time it lands. Refresh.
			name: "about to expire", expiry: now.Add(30 * time.Second), want: true,
		},
		{
			// No recorded expiry means we could not read one out of the JWT. Refresh
			// rather than send a token we cannot reason about.
			name: "no expiry", expiry: time.Time{}, want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Token{Expiry: tc.expiry}).Stale(now); got != tc.want {
				t.Errorf("Stale = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIDTokenExpiry(t *testing.T) {
	want := time.Unix(1_800_000_000, 0)

	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{name: "valid", token: fakeJWT(t, want), ok: true},
		{name: "not a jwt", token: "opaque-token", ok: false},
		{name: "empty", token: "", ok: false},
		{name: "bad base64", token: "a.!!!.c", ok: false},
		{
			name:  "no exp claim",
			token: "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c",
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idTokenExpiry(tc.token)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}

			if ok && !got.Equal(want) {
				t.Errorf("expiry = %v, want %v", got, want)
			}
		})
	}
}
