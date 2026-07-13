// Package cli is the `bakery` binary's API client: login, logout, whoami, and
// the CRUD verbs over /api/v1.
//
// It CONSUMES internal/api (its wire types), internal/auth (AuthConfig) and
// nothing else of the server. It holds no database handle and no Principal: the
// CLI is an untrusted client that talks HTTP, exactly like the console does.
package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File modes for the credential cache.
//
// These are not decoration. The cache holds a live OIDC ID token -- a bearer
// credential for the whole control plane -- and a refresh token, which is a
// credential for MINTING more of them. A world-readable token cache on a shared
// build host hands every account on the box the user's identity, and it does so
// silently: nothing about the CLI's behaviour changes.
const (
	credFileMode fs.FileMode = 0o600
	credDirMode  fs.FileMode = 0o700
)

// credFile is the cache's basename inside the bakery config directory.
const credFile = "credentials.json"

// Token is one server's cached credentials.
//
// IDToken is the credential we PRESENT: the server verifies an OIDC ID token per
// request (there is no static admin token and no server-minted bearer). The
// access token is kept only because the IdP handed it to us; nothing in Bakery
// consumes it. The refresh token is what saves the user from being bounced back
// to a browser every hour.
type Token struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// staleWindow is how far before expiry a token is treated as already stale, so a
// token that is valid when we check and expired when it lands at the server does
// not turn into a spurious "run bakery login".
const staleWindow = 60 * time.Second

// Stale reports whether the ID token is at or near expiry.
func (t Token) Stale(now time.Time) bool {
	if t.Expiry.IsZero() {
		// No expiry recorded means we could not read one out of the JWT. Treat it
		// as stale: a refresh is cheap, and a request with a dead token is a 401
		// the user cannot act on.
		return true
	}

	return !now.Before(t.Expiry.Add(-staleWindow))
}

// credentials is the on-disk document.
//
// Keyed by server URL, because one workstation talks to a staging Bakery and a
// production Bakery, and logging into one must not sign you out of the other.
type credentials struct {
	Servers map[string]Token `json:"servers"`
}

// TokenStore is the credential cache on disk.
type TokenStore struct {
	path string
}

// NewTokenStore locates the cache under XDG_CONFIG_HOME, falling back to
// ~/.config, per the XDG base directory spec.
func NewTokenStore() (*TokenStore, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}

	return &TokenStore{path: filepath.Join(dir, credFile)}, nil
}

// configDir resolves $XDG_CONFIG_HOME/bakery, or ~/.config/bakery.
//
// os.UserConfigDir already implements exactly this precedence on Linux, but it
// ignores a RELATIVE XDG_CONFIG_HOME (the spec says a relative path is invalid
// and must be ignored), which is the behaviour we want and would otherwise have
// to re-implement.
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user config directory: %w", err)
	}

	return filepath.Join(base, "bakery"), nil
}

// Path is where the cache lives. Printed by `bakery logout` so the user can see
// what was cleared.
func (s *TokenStore) Path() string { return s.path }

// Get returns the cached token for a server.
func (s *TokenStore) Get(server string) (Token, bool) {
	creds, err := s.load()
	if err != nil {
		return Token{}, false
	}

	t, ok := creds.Servers[canonicalServer(server)]
	if !ok || t.IDToken == "" {
		return Token{}, false
	}

	return t, true
}

// Put writes a server's token, creating the cache with 0600 / 0700 if absent.
func (s *TokenStore) Put(server string, t Token) error {
	creds, err := s.load()
	if err != nil {
		return err
	}

	if creds.Servers == nil {
		creds.Servers = make(map[string]Token)
	}

	creds.Servers[canonicalServer(server)] = t

	return s.save(creds)
}

// Delete drops a server's token. It is idempotent: logging out twice is not an
// error, and neither is logging out when you were never logged in.
func (s *TokenStore) Delete(server string) (bool, error) {
	creds, err := s.load()
	if err != nil {
		return false, err
	}

	key := canonicalServer(server)

	if _, ok := creds.Servers[key]; !ok {
		return false, nil
	}

	delete(creds.Servers, key)

	if len(creds.Servers) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("remove %s: %w", s.path, err)
		}

		return true, nil
	}

	return true, s.save(creds)
}

// load reads the cache. A missing file is an empty cache, not an error.
func (s *TokenStore) load() (credentials, error) {
	raw, err := os.ReadFile(s.path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return credentials{Servers: map[string]Token{}}, nil
	case err != nil:
		return credentials{}, fmt.Errorf("read %s: %w", s.path, err)
	}

	var creds credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return credentials{}, fmt.Errorf("parse %s: %w", s.path, err)
	}

	if creds.Servers == nil {
		creds.Servers = map[string]Token{}
	}

	return creds, nil
}

// save writes the cache atomically, with the permissions locked down.
//
// The write goes to a temp file in the SAME directory and is renamed over the
// target, so a crash mid-write leaves the old cache intact rather than a
// truncated JSON document that every later command refuses to parse. The temp
// file is created 0600 before a single byte of token is written to it -- creating
// it 0644 and chmod'ing afterwards would leave a window in which the token is
// world-readable on disk.
func (s *TokenStore) save(creds credentials) error {
	dir := filepath.Dir(s.path)

	if err := os.MkdirAll(dir, credDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// MkdirAll applies the mode only to directories it CREATES, and subtracts the
	// umask even then. An existing ~/.config/bakery from an older, laxer version
	// must still end up 0700.
	if err := os.Chmod(dir, credDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}

	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}

	tmp, err := os.CreateTemp(dir, credFile+".*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}

	tmpName := tmp.Name()

	// os.CreateTemp already makes the file 0600, but that is a documented default
	// and not a guarantee we want to inherit silently, and the umask does not
	// apply to it. Say it out loud.
	if err := tmp.Chmod(credFileMode); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}

	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return fmt.Errorf("write %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("close %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("rename %s to %s: %w", tmpName, s.path, err)
	}

	return nil
}

// canonicalServer normalizes a server URL so that `--server https://b.example.com/`
// and `--server https://b.example.com` are one cache entry, not two.
func canonicalServer(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}

// idTokenExpiry reads the `exp` claim out of a JWT WITHOUT verifying it.
//
// This is not a security check and must never be mistaken for one -- the server
// verifies the signature, the issuer and the audience, and it is the only party
// whose opinion of this token counts. Here the claim answers one local question:
// "should I refresh before I send this?" An attacker who tampered with `exp`
// would achieve nothing but making our own CLI refresh at the wrong time.
func idTokenExpiry(raw string) (time.Time, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}

	// JWTs are base64url with no padding.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}

	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}

	if claims.Exp == 0 {
		return time.Time{}, false
	}

	return time.Unix(claims.Exp, 0), true
}
