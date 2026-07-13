package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// ---------------------------------------------------------------------------
// fakeIDP: a real OIDC provider, standing on httptest, with a locally generated
// RSA key and a real JWKS document.
//
// The point of building this rather than stubbing the verifier is that the thing
// under test IS the verification. A fake that returns "yes, valid" proves nothing;
// the test that matters is the one where a token signed by the WRONG KEY has to be
// REJECTED, and only a genuine JWKS + genuine RSA signature can pose that question.
// ---------------------------------------------------------------------------

type fakeIDP struct {
	server *httptest.Server

	signKey *rsa.PrivateKey // the key whose public half is published in the JWKS
	evilKey *rsa.PrivateKey // an attacker's key. NEVER published.
	keyID   string

	clientID string

	// codes maps an authorization code to the ID token it will exchange for.
	codes map[string]string

	// lastTokenForm records what the client actually sent to /token, so PKCE can
	// be asserted on the wire rather than assumed.
	lastTokenForm map[string]string

	// withDeviceEndpoint controls whether discovery advertises
	// device_authorization_endpoint. It is RFC 8414 metadata, not core OIDC
	// Discovery, so a conformant provider may omit it -- and we must cope.
	withDeviceEndpoint bool
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}

	evilKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}

	idp := &fakeIDP{
		server:             nil,
		signKey:            signKey,
		evilKey:            evilKey,
		keyID:              "test-key-1",
		clientID:           "bakery",
		codes:              map[string]string{},
		lastTokenForm:      map[string]string{},
		withDeviceEndpoint: true,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("/jwks", idp.jwks)
	mux.HandleFunc("/token", idp.token)

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

func (f *fakeIDP) issuer() string { return f.server.URL }

func (f *fakeIDP) discovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                f.server.URL,
		"authorization_endpoint":                f.server.URL + "/auth",
		"token_endpoint":                        f.server.URL + "/token",
		"jwks_uri":                              f.server.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}

	if f.withDeviceEndpoint {
		doc["device_authorization_endpoint"] = f.server.URL + "/device"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// jwks publishes ONLY the public half of signKey. evilKey is never here -- that
// is what makes the wrong-key test meaningful.
func (f *fakeIDP) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.signKey.Public().(*rsa.PublicKey)

	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": f.keyID,
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	f.lastTokenForm = map[string]string{}
	for k := range r.Form {
		f.lastTokenForm[k] = r.Form.Get(k)
	}

	idToken, ok := f.codes[r.Form.Get("code")]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"id_token":      idToken,
	})
}

// claims is the ID token payload the fake IdP will sign.
//
// `groups` is a []string on purpose and NOT a bool+slice: nil means the claim is
// OMITTED from the token entirely (what Azure AD's overage produces), and an
// empty non-nil slice means the token carries `"groups": []`. Those are the two
// tokens the whole milestone turns on, and the harness has to be able to mint
// both.
type claims struct {
	sub      string
	email    string
	name     string
	groups   []string
	aud      string
	nonce    string
	issuedAt time.Time
	expires  time.Time

	// extra is spliced into the payload verbatim, so a test can mint the
	// `_claim_names` / `_claim_sources` overage, or a groups claim of the wrong
	// JSON type, without teaching the harness about either.
	extra map[string]any
}

func defaultClaims(f *fakeIDP) claims {
	return claims{
		sub:      "subject-1",
		email:    "jackson@example.com",
		name:     "Jackson Smith",
		groups:   []string{"platform", "yocto-admins"},
		aud:      f.clientID,
		nonce:    "",
		issuedAt: time.Now(),
		expires:  time.Now().Add(time.Hour),
		extra:    nil,
	}
}

// azureOverageClaims is the token a real, correctly-configured Azure AD user in
// more than ~200 groups actually receives: NO `groups` claim, and a
// `_claim_names` / `_claim_sources` pair pointing at Microsoft Graph instead.
//
// Read naively, this token is indistinguishable from "this user is in no
// groups". It is not that. It is "the answer is somewhere else".
func azureOverageClaims(f *fakeIDP) claims {
	c := defaultClaims(f)
	c.groups = nil // the overage REPLACES the claim; it does not shrink it
	c.extra = map[string]any{
		claimNamesClaim: map[string]any{"groups": "src1"},
		claimSourcesClaim: map[string]any{
			"src1": map[string]any{
				"endpoint": "https://graph.microsoft.com/v1.0/users/subject-1/getMemberObjects",
			},
		},
	}

	return c
}

// signIDToken mints a real RS256 JWT with the PUBLISHED key.
func (f *fakeIDP) signIDToken(t *testing.T, c claims) string {
	t.Helper()

	return f.sign(t, c, f.signKey, f.keyID)
}

// signWithWrongKey mints a token that is structurally perfect and claims the
// PUBLISHED key id -- but is signed with a key the IdP never published. This is
// the forgery an unverified decoder accepts and a real verifier must refuse.
func (f *fakeIDP) signWithWrongKey(t *testing.T, c claims) string {
	t.Helper()

	return f.sign(t, c, f.evilKey, f.keyID)
}

func (f *fakeIDP) sign(t *testing.T, c claims, key *rsa.PrivateKey, kid string) string {
	t.Helper()

	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}

	payload := map[string]any{
		"iss": f.server.URL,
		"sub": c.sub,
		"aud": c.aud,
		"exp": c.expires.Unix(),
		"iat": c.issuedAt.Unix(),
	}

	if c.email != "" {
		payload["email"] = c.email
		payload["email_verified"] = true
	}

	if c.name != "" {
		payload["name"] = c.name
	}

	if c.groups != nil {
		payload["groups"] = c.groups
	}

	if c.nonce != "" {
		payload["nonce"] = c.nonce
	}

	for k, v := range c.extra {
		payload[k] = v
	}

	signingInput := encodeSegment(t, header) + "." + encodeSegment(t, payload)

	digest := sha256.Sum256([]byte(signingInput))

	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func encodeSegment(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal jwt segment: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}

// issueCode registers an authorization code that will exchange for the given token.
func (f *fakeIDP) issueCode(code, idToken string) {
	f.codes[code] = idToken
}

// provider builds the real auth.Provider against this fake IdP.
func (f *fakeIDP) provider(t *testing.T) *Provider {
	t.Helper()

	p, err := NewProvider(t.Context(), OIDCConfig{
		Issuer:        f.issuer(),
		ClientID:      f.clientID,
		ClientSecret:  "secret",
		RedirectURL:   "https://bakery.example.com/api/v1/auth/callback",
		Scopes:        []string{"openid", "profile", "email", "groups", "offline_access"},
		GroupsClaim:   "",
		DeviceAuthURL: "",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	return p
}

// ---------------------------------------------------------------------------
// fakeKeyStore: the API key hot path, without a database.
// ---------------------------------------------------------------------------

type fakeKeyStore struct {
	rows     map[string]keyGrantRow // hex(hash) -> row
	touched  []pgtype.UUID
	desired  error
	validate int
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{rows: map[string]keyGrantRow{}, touched: nil, desired: nil, validate: 0}
}

func (f *fakeKeyStore) put(hash []byte, row keyGrantRow) {
	row.hash = hash
	f.rows[string(hash)] = row
}

func (f *fakeKeyStore) validateKey(_ context.Context, hash []byte) (keyGrantRow, error) {
	f.validate++

	if f.desired != nil {
		return keyGrantRow{}, f.desired
	}

	row, ok := f.rows[string(hash)]
	if !ok {
		return keyGrantRow{}, ErrKeyInvalid
	}

	return row, nil
}

func (f *fakeKeyStore) touchKeys(_ context.Context, ids []pgtype.UUID) error {
	f.touched = append(f.touched, ids...)

	return nil
}

// ---------------------------------------------------------------------------
// fakeSessionStore: an in-memory scs.Store whose Delete can be armed to fail, so
// the session-handler error paths (HandleLogout's 500) can be driven without a
// database. It stores data in a map so a real login can be established against it
// and the session token loaded back on the next request.
// ---------------------------------------------------------------------------

type fakeSessionStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	deleteErr error // when non-nil, Delete fails -- HandleLogout must 500
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{data: map[string][]byte{}, deleteErr: nil}
}

// The compiler pins the contract, exactly as SessionStore does for the real one.
var (
	_ scs.Store    = (*fakeSessionStore)(nil)
	_ scs.CtxStore = (*fakeSessionStore)(nil)
)

func (f *fakeSessionStore) FindCtx(_ context.Context, token string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	b, ok := f.data[token]

	return b, ok, nil
}

func (f *fakeSessionStore) CommitCtx(_ context.Context, token string, b []byte, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.data[token] = b

	return nil
}

func (f *fakeSessionStore) DeleteCtx(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.deleteErr != nil {
		return f.deleteErr
	}

	delete(f.data, token)

	return nil
}

func (f *fakeSessionStore) Find(token string) ([]byte, bool, error) {
	return f.FindCtx(context.Background(), token)
}

func (f *fakeSessionStore) Commit(token string, b []byte, expiry time.Time) error {
	return f.CommitCtx(context.Background(), token, b, expiry)
}

func (f *fakeSessionStore) Delete(token string) error {
	return f.DeleteCtx(context.Background(), token)
}

// uuid makes a deterministic, valid pgtype.UUID from a byte, for table tests.
func uuid(b byte) pgtype.UUID {
	var out pgtype.UUID

	out.Valid = true
	for i := range out.Bytes {
		out.Bytes[i] = b
	}

	return out
}

// errContains is the assertion helper for wrapped errors whose exact text comes
// from a library.
func errContains(err error, want string) bool {
	return err != nil && strings.Contains(err.Error(), want)
}

var errFake = errors.New("fake store failure")

// newRequest builds a request with an optional Authorization header.
func newRequest(t *testing.T, method, target, authorization string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(method, target, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}

	return r
}

// ---------------------------------------------------------------------------
// The DB-backed harness. dbtest hands each test its own migrated, isolated
// Postgres database (spawned via docker, or TEST_DB_URL in CI).
// ---------------------------------------------------------------------------

// TestMain is MANDATORY for a package that uses dbtest: it is Go's only
// after-the-last-test hook, and therefore the only correct place to stop the
// container the package's tests share.
func TestMain(m *testing.M) { dbtest.Main(m) }

// testGroupMap is the authorization policy the reconciliation tests run against.
const testGroupMap = `{
  "site_admin_groups": ["bakery-admins"],
  "orgs": [
    {
      "slug": "acme",
      "groups": {
        "acme-devs": "member",
        "acme-leads": "admin",
        "acme-owners": "owner"
      }
    },
    {
      "slug": "globex",
      "groups": { "globex-devs": "member" }
    }
  ]
}`

// testService stands up the real Service over a real database.
type testService struct {
	*Service

	pool  *pgxpool.Pool
	store *db.Store
}

func newTestService(t *testing.T, groupMapJSON string, devLogin bool) *testService {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var groups *config.GroupMap

	if groupMapJSON != "" {
		var err error

		groups, err = config.ParseGroupMap([]byte(groupMapJSON))
		if err != nil {
			t.Fatalf("parse the test group map: %v", err)
		}
	}

	sessions := NewSessionManager(NewSessionStore(pool, log), false)

	svc, err := New(Deps{
		Store:    store,
		Sessions: sessions,
		Provider: nil,
		Groups:   groups,
		Metrics:  metrics.New(),
		Log:      log,
		DevLogin: devLogin,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// Boot calls this, so the tests must start from the same state: every org the
	// mapping file names exists before anyone logs in.
	if err := svc.EnsureOrgs(t.Context()); err != nil {
		t.Fatalf("EnsureOrgs: %v", err)
	}

	return &testService{Service: svc, pool: pool, store: store}
}

// identity builds an Identity as though an ID token had just been verified and
// its groups claim READ. GroupsPresent is true: these are the IdP's answer.
//
// An identity whose groups could NOT be read is a different thing: it must be
// built deliberately (GroupsPresent: false), because the reconciler is required
// to treat it as a refusal, not as zero groups.
func identity(subject, email string, groups ...string) Identity {
	return Identity{
		Issuer: "https://idp.example.com", Subject: subject, Email: email,
		DisplayName: email, Groups: groups, GroupsPresent: true,
		IssuedAt: time.Now(), RefreshToken: "",
	}
}

// orgRoleOf reads a user's role in an org straight from the table, so the
// assertions are about what is DURABLE, not about what the service returned.
func orgRoleOf(t *testing.T, ts *testService, userID pgtype.UUID, slug string) (OrgRole, bool) {
	t.Helper()

	org, err := ts.store.GetOrganizationBySlug(t.Context(), slug)
	if err != nil {
		t.Fatalf("get organization %q: %v", slug, err)
	}

	m, err := ts.store.GetOrgMembership(t.Context(), repository.GetOrgMembershipParams{
		UserID: userID, OrgID: org.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}

	if err != nil {
		t.Fatalf("get org membership: %v", err)
	}

	return m.Role, true
}

func orgIDOf(t *testing.T, ts *testService, slug string) pgtype.UUID {
	t.Helper()

	org, err := ts.store.GetOrganizationBySlug(t.Context(), slug)
	if err != nil {
		t.Fatalf("get organization %q: %v", slug, err)
	}

	return org.ID
}
