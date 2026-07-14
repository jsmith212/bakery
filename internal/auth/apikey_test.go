package auth

import (
	"crypto/sha256"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// tokenPrefixPattern is the CHECK constraint the schema puts on token_prefix. A
// generated prefix that violates it fails at the INSERT, so the generator and the
// schema must agree -- and this is where they are made to.
var tokenPrefixPattern = regexp.MustCompile(`^bkry_[A-Za-z0-9_-]{6,12}$`)

func TestGenerateAPIKey(t *testing.T) {
	seen := make(map[string]struct{}, 200)

	for range 200 {
		key, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() error = %v", err)
		}

		if !strings.HasPrefix(key.Token, TokenPrefix) {
			t.Fatalf("Token = %q, want the %q prefix so a leaked key is greppable", key.Token, TokenPrefix)
		}

		// 5 ("bkry_") + 43 (base64url of 32 bytes, unpadded) = 48.
		if len(key.Token) != 48 {
			t.Fatalf("len(Token) = %d, want 48 (bkry_ + 256 bits base64url)", len(key.Token))
		}

		if !tokenPrefixPattern.MatchString(key.Prefix) {
			t.Fatalf("Prefix = %q, want it to satisfy the schema's CHECK %s", key.Prefix, tokenPrefixPattern)
		}

		if !strings.HasPrefix(key.Token, key.Prefix) {
			t.Fatalf("Prefix %q is not a prefix of Token %q", key.Prefix, key.Token)
		}

		// The stored form is the hash and ONLY the hash. If the plaintext ever
		// appears here, the schema's CHECK (octet_length = 32) rejects the INSERT --
		// but catching it in Go is cheaper than catching it in production.
		if len(key.Hash) != sha256.Size {
			t.Fatalf("len(Hash) = %d, want %d raw bytes", len(key.Hash), sha256.Size)
		}

		if strings.Contains(string(key.Hash), key.Token) {
			t.Fatal("the stored hash contains the plaintext token")
		}

		// A collision in 200 draws from a 256-bit space would mean the CSPRNG is
		// not one -- which is exactly the bug math/rand would give us.
		if _, dup := seen[key.Token]; dup {
			t.Fatalf("GenerateAPIKey() repeated a token: %q", key.Token)
		}

		seen[key.Token] = struct{}{}
	}
}

func TestHashTokenAndConstantTimeCompare(t *testing.T) {
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}

	// HashToken is deterministic and matches what was stored at creation.
	if !TokenMatchesHash(key.Token, key.Hash) {
		t.Error("TokenMatchesHash() rejected the token it was generated with")
	}

	// SHA-256 of the FULL token, prefix included -- not of the random part alone.
	want := sha256.Sum256([]byte(key.Token))
	if !TokenMatchesHash(key.Token, want[:]) {
		t.Error("HashToken() is not sha256 of the full presented token")
	}

	tests := []struct {
		name  string
		token string
		hash  []byte
		want  bool
	}{
		{name: "the right token", token: key.Token, hash: key.Hash, want: true},
		{name: "a different token", token: key.Token + "x", hash: key.Hash, want: false},
		{name: "a truncated token", token: key.Token[:len(key.Token)-1], hash: key.Hash, want: false},
		{name: "the empty token", token: "", hash: key.Hash, want: false},
		{name: "an empty hash", token: key.Token, hash: nil, want: false},
		{name: "a short hash", token: key.Token, hash: key.Hash[:16], want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TokenMatchesHash(tt.token, tt.hash); got != tt.want {
				t.Errorf("TokenMatchesHash() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAPIKeyHashComparisonIsConstantTime makes the SOURCE the enforcement
// mechanism for the timing-safe hash comparison, the way
// TestPrincipalHasNoExportedConstructor does for the sealed Principal.
//
// TokenMatchesHash and authenticateKey both compare a presented key's SHA-256
// against the stored one, and both MUST do it with crypto/subtle.ConstantTimeCompare.
// bytes.Equal -- and == on the hex form -- short-circuits on the first differing
// byte, which turns the DURATION of a rejection into a byte-at-a-time oracle for
// the very secret that guards every cache write.
//
// A behavioural test cannot catch a regression here: ConstantTimeCompare and
// bytes.Equal return the identical boolean, so TestHashTokenAndConstantTimeCompare
// stays green under either. The property is about HOW the comparison is made, not
// what it returns, so it has to be asserted at the source level. A refactor to
// bytes.Equal drops the crypto/subtle import and adds a "bytes" one -- either half
// fails this test on its own.
func TestAPIKeyHashComparisonIsConstantTime(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "apikey.go", nil, 0)
	if err != nil {
		t.Fatalf("parse apikey.go: %v", err)
	}

	imports := map[string]bool{}
	for _, imp := range file.Imports {
		imports[strings.Trim(imp.Path.Value, `"`)] = true
	}

	if !imports["crypto/subtle"] {
		t.Error("apikey.go no longer imports crypto/subtle: the API-key hash comparison is not constant-time, " +
			"which makes rejection latency a byte-at-a-time oracle for the stored secret")
	}

	if imports["bytes"] {
		t.Error("apikey.go imports \"bytes\": bytes.Equal short-circuits on the first differing byte and is a " +
			"timing oracle on a secret hash -- the comparison must use crypto/subtle.ConstantTimeCompare")
	}

	// The primitive must not merely be imported, it must be CALLED at both sites --
	// TokenMatchesHash and authenticateKey -- so an import kept alive by one call
	// while the other reverts to bytes.Equal or == cannot pass.
	calls := 0

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			calls++
		}

		return true
	})

	if calls < 2 {
		t.Errorf("apikey.go calls subtle.ConstantTimeCompare %d time(s), want >= 2: both TokenMatchesHash and "+
			"authenticateKey compare a key hash and both must be constant-time", calls)
	}
}

func TestLooksLikeAPIKey(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "a real key", token: "bkry_" + strings.Repeat("a", 43), want: true},
		{name: "a JWT is not a key", token: "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.sig", want: false},
		{name: "the prefix alone", token: "bkry_", want: false},
		{name: "too short to hold a prefix", token: "bkry_abc", want: false},
		{name: "empty", token: "", want: false},
		{name: "the wrong prefix", token: "bake_" + strings.Repeat("a", 43), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeAPIKey(tt.token); got != tt.want {
				t.Errorf("looksLikeAPIKey(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

// TestAuthenticateKey drives the hot path against a fake store: one probe in, one
// Principal out, and every rejection shaped identically so nothing is an oracle.
func TestAuthenticateKey(t *testing.T) {
	good, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}

	other, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}

	store := newFakeKeyStore()
	store.put(good.Hash, keyGrantRow{
		id: uuid(0xff), userID: uuid(0x01), projectID: projectA, scope: ScopeWrite, hash: nil,
	})

	svc := &Service{keys: store, toucher: newKeyToucher(store)}

	t.Run("a live key authenticates", func(t *testing.T) {
		p, err := svc.authenticateKey(t.Context(), good.Token)
		if err != nil {
			t.Fatalf("authenticateKey() error = %v", err)
		}

		if p.Method() != MethodAPIKey {
			t.Errorf("Method() = %q, want %q", p.Method(), MethodAPIKey)
		}

		if !p.CanWriteProject(orgA, projectA) {
			t.Error("a write-scoped key cannot write its own project")
		}

		if p.CanWriteProject(orgB, projectB) {
			t.Error("the key authorized a project it was not minted for")
		}
	})

	t.Run("an unknown key is refused", func(t *testing.T) {
		if _, err := svc.authenticateKey(t.Context(), other.Token); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("authenticateKey() with an unknown key = %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("a malformed key never reaches the database", func(t *testing.T) {
		before := store.validate

		if _, err := svc.authenticateKey(t.Context(), "not-a-key"); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("authenticateKey() with a malformed key = %v, want ErrKeyInvalid", err)
		}

		if store.validate != before {
			t.Error("a malformed key hit the database; the shape check must short-circuit it")
		}
	})

	t.Run("a store failure is not a silent deny", func(t *testing.T) {
		broken := newFakeKeyStore()
		broken.desired = errFake

		svc := &Service{keys: broken, toucher: newKeyToucher(broken)}

		_, err := svc.authenticateKey(t.Context(), good.Token)
		if !errors.Is(err, errFake) {
			t.Fatalf("authenticateKey() with a broken store = %v, want the store's error to surface", err)
		}
	})
}

// TestAuthenticateToken drives the seam hashserv authenticates through.
//
// hashserv's credential arrives IN-BAND, in a WebSocket `auth` RPC, so there is no
// *http.Request to hand to AuthenticateCache and no HTTP layer in front of this
// path at all. What has to be true is that it is the SAME probe, not a second,
// weaker one -- so this runs against a REAL database. Revocation and expiry are
// enforced by validateKeySQL's predicate, and a fake store cannot pose that
// question: a row it was never given is not a row that was revoked.
func TestAuthenticateToken(t *testing.T) {
	t.Parallel()

	ts := newTestService(t, testGroupMap, false)
	ctx := t.Context()

	orgID, projectID, owner := seedMember(t, ts, ProjectRoleWriter)

	// The key owner is made a SITE ADMIN, which is what turns the IsSiteAdmin
	// assertion below into a real question instead of a tautology. A key is a
	// delegation capped at one project and one scope; if this path ever loaded its
	// owner's site role, the read-scoped key minted here would be a master key for
	// the whole installation -- and hashserv, whose perms table (spec §4) reads
	// exactly these capabilities, would honour it.
	//
	// site_role itself is GENERATED (greatest of the two halves), so the claim half
	// is what there is to write.
	tag, err := ts.pool.Exec(ctx,
		`UPDATE users SET site_role_oidc = 'admin' WHERE id = $1`, owner.UserID())
	if err != nil {
		t.Fatalf("promote the key owner to site admin: %v", err)
	}

	// An UPDATE that matched nothing would leave the owner an ordinary user and make
	// every IsSiteAdmin assertion below pass for the wrong reason.
	if tag.RowsAffected() != 1 {
		t.Fatalf("promoting the key owner updated %d rows, want 1", tag.RowsAffected())
	}

	mint := func(name string, scope Scope, expires *time.Time) (string, pgtype.UUID) {
		t.Helper()

		key, row, err := ts.CreateAPIKey(ctx, owner, CreateKeyInput{
			OrgID: pgtype.UUID{}, ProjectID: projectID, Name: name, Scope: scope, ExpiresAt: expires,
		})
		if err != nil {
			t.Fatalf("CreateAPIKey(%q): %v", name, err)
		}

		return key.Token, row.ID
	}

	writeToken, _ := mint("ci-write", ScopeWrite, nil)
	readToken, _ := mint("ci-read", ScopeRead, nil)

	revokedToken, revokedID := mint("revoked", ScopeWrite, nil)
	if _, err := ts.store.RevokeAPIKey(ctx, revokedID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	// Aged, not born expired: api_keys_expires_after_created refuses
	// expires_at <= created_at, so both timestamps move into the past.
	expiredToken, expiredID := mint("expired", ScopeWrite, ptr(time.Now().Add(time.Hour)))
	if _, err := ts.pool.Exec(ctx, `
		UPDATE api_keys
		   SET created_at = now() - interval '2 hours',
		       expires_at = now() - interval '1 hour'
		 WHERE id = $1`, expiredID); err != nil {
		t.Fatalf("age the key: %v", err)
	}

	tests := []struct {
		name      string
		token     string
		wantErr   bool
		wantRead  bool
		wantWrite bool
	}{
		{
			name:  "a write-scoped key reads and writes its project",
			token: writeToken, wantErr: false, wantRead: true, wantWrite: true,
		},
		{
			name:  "a read-scoped key reads its project and writes nothing",
			token: readToken, wantErr: false, wantRead: true, wantWrite: false,
		},
		{
			name:  "a revoked key is refused",
			token: revokedToken, wantErr: true, wantRead: false, wantWrite: false,
		},
		{
			name:  "an expired key is refused",
			token: expiredToken, wantErr: true, wantRead: false, wantWrite: false,
		},
		{
			// The auth RPC is client-supplied JSON. Garbage is a rejection, never a panic.
			name:  "a malformed token is refused",
			token: "bkry_", wantErr: true, wantRead: false, wantWrite: false,
		},
		{
			// What an anonymous client's auth RPC carries if it sends one at all.
			name:  "the empty token is refused",
			token: "", wantErr: true, wantRead: false, wantWrite: false,
		},
		{
			name:  "a token that is not a bkry_ token at all is refused",
			token: "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.sig", wantErr: true, wantRead: false, wantWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ts.AuthenticateToken(ctx, tt.token)

			if tt.wantErr {
				// Every rejection is the SAME error. Distinguishing "unknown" from
				// "revoked" from "expired" to a client that just failed to authenticate
				// is an oracle, and the in-band invoke-error hashserv sends back carries
				// whatever this says.
				if !errors.Is(err, ErrKeyInvalid) {
					t.Fatalf("AuthenticateToken() = (%v, %v), want ErrKeyInvalid", p, err)
				}

				if p != nil {
					t.Fatalf("AuthenticateToken() returned a Principal (%#v) alongside an error", p)
				}

				return
			}

			if err != nil {
				t.Fatalf("AuthenticateToken() error = %v", err)
			}

			if p.Method() != MethodAPIKey {
				t.Errorf("Method() = %q, want %q", p.Method(), MethodAPIKey)
			}

			if got := p.CanReadProject(orgID, projectID); got != tt.wantRead {
				t.Errorf("CanReadProject() = %v, want %v", got, tt.wantRead)
			}

			if got := p.CanWriteProject(orgID, projectID); got != tt.wantWrite {
				t.Errorf("CanWriteProject() = %v, want %v", got, tt.wantWrite)
			}

			// The cap, on THIS path. The owning user is a site admin in the database.
			if p.IsSiteAdmin() {
				t.Error("IsSiteAdmin() = true: the key principal inherited its owner's site admin")
			}

			grant, ok := p.APIKey()
			if !ok {
				t.Fatal("APIKey() reported no grant on a principal built from a key")
			}

			if grant.ProjectID != projectID {
				t.Errorf("APIKey().ProjectID = %v, want the project the key was minted for (%v)",
					grant.ProjectID, projectID)
			}
		})
	}

	// And a key for THIS project authorizes nothing in another one: hashserv is
	// multi-tenant on exactly this check.
	p, err := ts.AuthenticateToken(ctx, writeToken)
	if err != nil {
		t.Fatalf("AuthenticateToken() error = %v", err)
	}

	if p.CanReadProject(orgB, projectB) || p.CanWriteProject(orgB, projectB) {
		t.Error("the key authorized a project it was not minted for")
	}
}

// TestKeyToucherCoalesces: last_used_at must never be written on the request path.
// One CI machine drives a whole build with ONE key, so an inline UPDATE funnels
// thousands of parallel HEADs into a row-lock convoy on the hottest row in the
// database. The toucher batches them instead.
func TestKeyToucherCoalesces(t *testing.T) {
	store := newFakeKeyStore()
	toucher := newKeyToucher(store)

	// The same key, seen a thousand times, as an sstate HEAD storm would.
	for range 1000 {
		toucher.mark(uuid(0xaa))
	}

	toucher.mark(uuid(0xbb))

	// Nothing has been written yet: mark() does no I/O at all.
	if len(store.touched) != 0 {
		t.Fatalf("the toucher wrote %d rows on the request path; it must write none", len(store.touched))
	}

	if err := toucher.flush(t.Context()); err != nil {
		t.Fatalf("flush() error = %v", err)
	}

	// 1001 marks collapse to 2 rows in 1 statement.
	if len(store.touched) != 2 {
		t.Fatalf("flush() wrote %d ids, want 2 (the marks must coalesce)", len(store.touched))
	}

	// And the pending set is drained, so an idle flush is free.
	if err := toucher.flush(t.Context()); err != nil {
		t.Fatalf("flush() error = %v", err)
	}

	if len(store.touched) != 2 {
		t.Fatalf("a second flush wrote more rows: %d, want the set to have been drained", len(store.touched))
	}
}
