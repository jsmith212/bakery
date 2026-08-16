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

// TestCredentialsFileBackcompat is the load-bearing test in this widening: an
// EXISTING credentials.json, written by a version of this CLI that predates
// user tokens and robot/project keys, must parse UNCHANGED. serverCreds embeds
// Token anonymously precisely so this file's shape -- id_token/access_token/
// refresh_token/expiry, nothing else -- decodes straight into the promoted
// fields, with UserToken and Keys simply absent. No forced re-login on upgrade.
func TestCredentialsFileBackcompat(t *testing.T) {
	store, home := tempStore(t)

	dir := filepath.Join(home, "bakery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The pre-widening shape, verbatim: one server, one OIDC session, no
	// user_token or keys field at all -- because those did not exist yet.
	old := `{
		"servers": {
			"https://bakery.example.com": {
				"id_token": "old-id-token",
				"access_token": "old-access-token",
				"refresh_token": "old-refresh-token",
				"expiry": "2030-01-01T00:00:00Z"
			}
		}
	}`

	if err := os.WriteFile(store.Path(), []byte(old), 0o600); err != nil {
		t.Fatalf("write the old-shape file: %v", err)
	}

	tok, ok := store.Get("https://bakery.example.com")
	if !ok {
		t.Fatal("Get found no token in the old-shape file")
	}

	if tok.IDToken != "old-id-token" || tok.AccessToken != "old-access-token" ||
		tok.RefreshToken != "old-refresh-token" {
		t.Errorf("Token = %+v, want the three old fields verbatim", tok)
	}

	// Nothing new is invented out of an absent field.
	if _, ok := store.GetUserToken("https://bakery.example.com"); ok {
		t.Error("GetUserToken found a token in a file that never had a user_token field")
	}

	if _, ok := store.GetKey("https://bakery.example.com", "acme"); ok {
		t.Error("GetKey found a key in a file that never had a keys field")
	}

	if _, ok := store.DefaultServer(); ok {
		t.Error("DefaultServer found one in a file that never had a default_server field")
	}

	// And writing through the WIDENED type must not perturb the old bytes any
	// more than JSON round-tripping is expected to (same keys, same values).
	if err := store.PutUserToken("https://bakery.example.com", "bkru_new"); err != nil {
		t.Fatalf("PutUserToken: %v", err)
	}

	tok, ok = store.Get("https://bakery.example.com")
	if !ok || tok.IDToken != "old-id-token" {
		t.Errorf("the OIDC session did not survive writing a user token alongside it: %+v (found=%v)", tok, ok)
	}
}

// TestTokenStoreWidenedRoundTrip: Put (the OIDC session), PutUserToken and
// PutKey each touch a DIFFERENT field of the same server entry, and none may
// clobber another -- the whole reason serverCreds groups them instead of three
// separate top-level maps keyed by server.
func TestTokenStoreWidenedRoundTrip(t *testing.T) {
	store, _ := tempStore(t)
	const server = "https://bakery.example.com"

	if err := store.Put(server, Token{IDToken: "id-1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.PutUserToken(server, "bkru_abc"); err != nil {
		t.Fatalf("PutUserToken: %v", err)
	}

	if err := store.PutKey(server, "acme", "bkro_org"); err != nil {
		t.Fatalf("PutKey(org): %v", err)
	}

	if err := store.PutKey(server, "acme/firmware", "bkry_proj"); err != nil {
		t.Fatalf("PutKey(org/project): %v", err)
	}

	tok, ok := store.Get(server)
	if !ok || tok.IDToken != "id-1" {
		t.Errorf("Get = %+v (found=%v), want id-1 surviving every later write", tok, ok)
	}

	if got, ok := store.GetUserToken(server); !ok || got != "bkru_abc" {
		t.Errorf("GetUserToken = %q (found=%v), want bkru_abc", got, ok)
	}

	if got, ok := store.GetKey(server, "acme"); !ok || got != "bkro_org" {
		t.Errorf("GetKey(org) = %q (found=%v), want bkro_org", got, ok)
	}

	if got, ok := store.GetKey(server, "acme/firmware"); !ok || got != "bkry_proj" {
		t.Errorf("GetKey(org/project) = %q (found=%v), want bkry_proj", got, ok)
	}

	// A later Put (an ordinary re-login) must not evict the keys or the user
	// token minted in between -- see Put's doc.
	if err := store.Put(server, Token{IDToken: "id-2"}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if got, ok := store.GetUserToken(server); !ok || got != "bkru_abc" {
		t.Errorf("GetUserToken after a second Put = %q (found=%v), want it to survive", got, ok)
	}

	if got, ok := store.GetKey(server, "acme/firmware"); !ok || got != "bkry_proj" {
		t.Errorf("GetKey(org/project) after a second Put = %q (found=%v), want it to survive", got, ok)
	}
}

// TestTokenStoreDeletePreservesUserTokenAndKeys: `bakery logout` ends the
// browser-derived session only. A cached user token or project/robot key is an
// independently-lived credential a CI job may depend on, and must not
// disappear as a side effect of a human logging out of their own session.
func TestTokenStoreDeletePreservesUserTokenAndKeys(t *testing.T) {
	store, _ := tempStore(t)
	const server = "https://bakery.example.com"

	if err := store.Put(server, Token{IDToken: "id-1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.PutUserToken(server, "bkru_abc"); err != nil {
		t.Fatalf("PutUserToken: %v", err)
	}

	if err := store.PutKey(server, "acme/firmware", "bkry_proj"); err != nil {
		t.Fatalf("PutKey: %v", err)
	}

	had, err := store.Delete(server)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if !had {
		t.Fatal("Delete reported no session, want one")
	}

	if _, ok := store.Get(server); ok {
		t.Error("the OIDC session survived Delete")
	}

	if got, ok := store.GetUserToken(server); !ok || got != "bkru_abc" {
		t.Errorf("GetUserToken after Delete = %q (found=%v), want it to survive logout", got, ok)
	}

	if got, ok := store.GetKey(server, "acme/firmware"); !ok || got != "bkry_proj" {
		t.Errorf("GetKey after Delete = %q (found=%v), want it to survive logout", got, ok)
	}

	// The file itself must survive too -- deleting it would drop the surviving
	// key and user token right along with it.
	if _, err := os.Stat(store.Path()); err != nil {
		t.Errorf("credentials.json was removed even though it still holds a key: %v", err)
	}
}

// TestTokenStoreDefaultServerRoundTrip covers `bakery login --set-default`.
func TestTokenStoreDefaultServerRoundTrip(t *testing.T) {
	store, _ := tempStore(t)

	if _, ok := store.DefaultServer(); ok {
		t.Error("DefaultServer found one before any was set")
	}

	if err := store.SetDefaultServer("https://staging.example.com/"); err != nil {
		t.Fatalf("SetDefaultServer: %v", err)
	}

	got, ok := store.DefaultServer()
	if !ok {
		t.Fatal("DefaultServer found none after SetDefaultServer")
	}

	// canonicalServer strips the trailing slash, same as the server-keyed map
	// itself -- a default recorded with or without one must be the same value.
	if got != "https://staging.example.com" {
		t.Errorf("DefaultServer = %q, want the trailing slash stripped", got)
	}

	if err := store.SetDefaultServer("https://prod.example.com"); err != nil {
		t.Fatalf("second SetDefaultServer: %v", err)
	}

	if got, _ := store.DefaultServer(); got != "https://prod.example.com" {
		t.Errorf("DefaultServer after a second Set = %q, want the latest value", got)
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
