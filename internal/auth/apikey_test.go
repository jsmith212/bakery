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
