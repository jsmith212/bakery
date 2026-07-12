package cli

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
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
)

// TestMain gives the end-to-end test below a real, ephemeral Postgres. dbtest
// spins a container (or uses TEST_DB_URL) lazily, so the unit tests in this
// package do not pay for it.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// ---------------------------------------------------------------------------
// The end-to-end harness: a REAL identity provider, a REAL Bakery, a REAL
// Postgres, and the real CLI client driving all three.
//
// The unit tests above stub the server. This one does not stub anything the CLI
// talks to. It matters because almost everything that can actually be wrong here
// lives in the seams:
//
//   - the CLI presents the ID TOKEN as its bearer, not the access token. Present
//     the wrong one and every mock still says 200, while a real server -- which
//     verifies an OIDC ID token's signature, issuer, audience and expiry -- says
//     401. Nothing but a real verifier catches that.
//   - the device flow's endpoints come from the SERVER's /auth/config, which comes
//     from the IdP's discovery document. Three hops, none of them exercised by a
//     hand-written AuthConfig literal.
//   - JIT provisioning and group reconciliation happen on the first bearer
//     request. The CLI is the ONLY client that can trigger that path: the browser
//     flow provisions in the callback. If it is broken, the CLI user does not
//     exist and every command 401s.
// ---------------------------------------------------------------------------

// idpKey is the fake IdP's signing key, and the JWKS it publishes.
type idpKey struct {
	priv *rsa.PrivateKey
	kid  string
}

func newIDPKey(t *testing.T) *idpKey {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an RSA key: %v", err)
	}

	return &idpKey{priv: priv, kid: "test-key-1"}
}

// jwks renders the public half as an RFC 7517 JWK set. go-oidc fetches this and
// verifies the ID token's signature against it, so if this is wrong, /me 401s.
func (k *idpKey) jwks() map[string]any {
	pub := k.priv.PublicKey

	return map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": k.kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
}

// sign mints a real RS256 JWT. Hand-rolled rather than pulled from a library
// because the test needs to be able to produce a token the SERVER will accept,
// and doing it by hand is what proves the server is actually checking.
func (k *idpKey) sign(t *testing.T, claims map[string]any) string {
	t.Helper()

	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(raw)
	}

	header := b64(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.kid})
	payload := b64(claims)
	signing := header + "." + payload

	sum := sha256.Sum256([]byte(signing))

	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

const (
	testClientID = "bakery-cli"
	testEmail    = "dev@bakery.local"
	testGroup    = "bakery-admins"
	testOrg      = "acme"
)

// realIDP is an OIDC provider: discovery, JWKS, device authorization, token.
type realIDP struct {
	server *httptest.Server
	key    *idpKey

	// approved flips to true after one authorization_pending, so the flow goes
	// through the poll loop rather than short-circuiting to success.
	polls int
}

func newRealIDP(t *testing.T) *realIDP {
	t.Helper()

	idp := &realIDP{server: nil, key: newIDPKey(t), polls: 0}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/jwks",
			"device_authorization_endpoint":         idp.server.URL + "/device",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})

	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, http.StatusOK, idp.key.jwks())
	})

	mux.HandleFunc("POST /device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"device_code":      "device-code-1",
			"user_code":        "WDJB-MJHT",
			"verification_uri": idp.server.URL + "/activate",
			"expires_in":       600,
			"interval":         1,
		})
	})

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse the token form: %v", err)
		}

		idp.polls++

		// The first poll is pending: the user has not typed the code in yet. This
		// is the ordinary case, and it means the loop is actually exercised.
		if idp.polls == 1 && r.PostForm.Get("grant_type") == deviceGrantType {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))

			return
		}

		now := time.Now()
		idToken := idp.key.sign(t, map[string]any{
			"iss":    idp.server.URL,
			"aud":    testClientID,
			"sub":    "user-subject-1",
			"email":  testEmail,
			"name":   "Dev User",
			"groups": []string{testGroup},
			"iat":    now.Unix(),
			"exp":    now.Add(time.Hour).Unix(),
		})

		writeJSONResp(w, http.StatusOK, map[string]any{
			"access_token":  "access-token-1",
			"refresh_token": "refresh-token-1",
			"id_token":      idToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

// newBakery boots the real control plane against a real Postgres and a real IdP.
func newBakery(t *testing.T, idp *realIDP) *httptest.Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool := dbtest.New(t)
	store := db.NewStore(pool)

	provider, err := auth.NewProvider(t.Context(), auth.OIDCConfig{
		Issuer: idp.server.URL, ClientID: testClientID, ClientSecret: "",
		RedirectURL: "http://localhost:8080/api/v1/auth/callback",
		Scopes:      []string{"openid", "profile", "email", "groups", "offline_access"},
		GroupsClaim: "", DeviceAuthURL: "",
	})
	if err != nil {
		t.Fatalf("auth.NewProvider (OIDC discovery against the fake IdP): %v", err)
	}

	if !provider.HasDeviceGrant() {
		t.Fatal("the provider reports no device grant; discovery did not surface " +
			"device_authorization_endpoint")
	}

	groups, err := config.ParseGroupMap([]byte(`{
		"site_admin_groups": ["` + testGroup + `"],
		"orgs": [{"slug": "` + testOrg + `", "groups": {"` + testGroup + `": "owner"}}]
	}`))
	if err != nil {
		t.Fatalf("parse the group map: %v", err)
	}

	svc, err := auth.New(auth.Deps{
		Store: store, Sessions: auth.NewSessionManager(auth.NewSessionStore(pool, log), false),
		Provider: provider, Groups: groups, Metrics: nil, Log: log, DevLogin: false,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	if err := svc.EnsureOrgs(t.Context()); err != nil {
		t.Fatalf("EnsureOrgs: %v", err)
	}

	a, err := api.New(api.Config{Store: store, Auth: svc, Metrics: metrics.New(), Log: log})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)

	return server
}

// TestEndToEnd drives the whole CLI against the whole server: device login, then
// every CRUD verb, against a real Postgres with real OIDC verification.
func TestEndToEnd(t *testing.T) {
	idp := newRealIDP(t)
	server := newBakery(t, idp)

	tokens, _ := tempStore(t)

	c, err := NewClient(server.URL, tokens)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Do not actually sit through the poll interval.
	var slept []time.Duration

	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)

		return nil
	}

	ctx := t.Context()

	// ---- login: the device grant, end to end.
	out := new(strings.Builder)
	if err := Login(ctx, c, out); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if !strings.Contains(out.String(), "WDJB-MJHT") {
		t.Errorf("login did not print the user code:\n%s", out)
	}

	if len(slept) != 2 {
		t.Errorf("polled %d times, want 2 (one authorization_pending, then success)", len(slept))
	}

	// The bearer we cached must be the ID TOKEN. If the CLI had cached the access
	// token instead, /me below would 401 -- "access-token-1" is not a JWT and the
	// server would not verify it. That is exactly the bug this asserts against.
	tok, ok := tokens.Get(c.Server())
	if !ok {
		t.Fatal("login cached no token")
	}

	if tok.IDToken == "access-token-1" {
		t.Fatal("the CLI cached the ACCESS token as its bearer; it must present the ID token")
	}

	// ---- whoami: JIT provisioning + group reconciliation happened on that first
	// bearer request, because the device grant has no server-side callback.
	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if me.Email != testEmail {
		t.Errorf("email = %q, want %q", me.Email, testEmail)
	}

	if me.Method != string(auth.MethodBearer) {
		t.Errorf("method = %q, want %q", me.Method, auth.MethodBearer)
	}

	if !me.IsSiteAdmin {
		t.Errorf("is_site_admin = false; the %q group should have made this user a site admin",
			testGroup)
	}

	if len(me.Orgs) != 1 || me.Orgs[0].Slug != testOrg || me.Orgs[0].Role != "owner" {
		t.Fatalf("orgs = %+v, want owner of %s (the group map was not reconciled)", me.Orgs, testOrg)
	}

	// ---- org
	newOrg, err := c.CreateOrg(ctx, "widgets-co", "Widgets Co")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if newOrg.Slug != "widgets-co" {
		t.Errorf("created org slug = %q", newOrg.Slug)
	}

	orgs, err := c.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}

	if len(orgs) < 2 {
		t.Errorf("orgs = %d, want at least acme + widgets-co", len(orgs))
	}

	renamed, err := c.RenameOrg(ctx, "widgets-co", "Widgets Company")
	if err != nil {
		t.Fatalf("RenameOrg: %v", err)
	}

	if renamed.Name != "Widgets Company" {
		t.Errorf("name = %q", renamed.Name)
	}

	// ---- a reserved slug is refused, with a code the CLI can branch on.
	if _, err := c.CreateOrg(ctx, "cache", "Cache"); err == nil {
		t.Error("creating an org with the reserved slug \"cache\" succeeded")
	} else {
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != api.CodeReservedSlug {
			t.Errorf("err = %v, want an APIError with code %q", err, api.CodeReservedSlug)
		}
	}

	// ---- project
	project, err := c.CreateProject(ctx, testOrg, "yocto", "Yocto")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if project.OrgSlug != testOrg || project.Slug != "yocto" {
		t.Errorf("project = %s/%s", project.OrgSlug, project.Slug)
	}

	projects, err := c.ListProjects(ctx, testOrg)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	if len(projects) != 1 {
		t.Errorf("projects = %d, want 1", len(projects))
	}

	if _, err := c.GetProject(ctx, testOrg, "yocto"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	// ---- members
	member, err := c.SetProjectMember(ctx, testOrg, "yocto", testEmail, "admin")
	if err != nil {
		t.Fatalf("SetProjectMember: %v", err)
	}

	if member.ProjectRole != "admin" {
		t.Errorf("project role = %q, want admin", member.ProjectRole)
	}

	members, err := c.ListProjectMembers(ctx, testOrg, "yocto")
	if err != nil {
		t.Fatalf("ListProjectMembers: %v", err)
	}

	if len(members) != 1 || members[0].Email != testEmail {
		t.Fatalf("members = %+v", members)
	}

	orgMembers, err := c.ListOrgMembers(ctx, testOrg)
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}

	if len(orgMembers) != 1 || orgMembers[0].Source != api.OrgRoleSourceOIDC {
		t.Fatalf("org members = %+v, want one claim-derived member", orgMembers)
	}

	// ---- keys. The token comes back exactly once.
	key, err := c.CreateKey(ctx, testOrg, "yocto", "ci", "write", nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if !strings.HasPrefix(key.Token, "bkry_") {
		t.Errorf("token = %q, want a bkry_ prefix", key.Token)
	}

	if key.OwnerEmail != testEmail {
		t.Errorf("owner = %q, want %q", key.OwnerEmail, testEmail)
	}

	keys, err := c.ListKeys(ctx, testOrg, "yocto")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}

	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}

	// The list endpoint's type has no field a token could live in -- assert the
	// wire form too, so a future `omitempty Token` on the shared type is caught
	// here and not in an incident.
	raw, err := json.Marshal(keys[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(raw), key.Token) {
		t.Fatalf("the key LIST leaked the token: %s", raw)
	}

	if err := c.DeleteKey(ctx, testOrg, "yocto", keys[0].ID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	// ---- teardown
	if err := c.RemoveProjectMember(ctx, testOrg, "yocto", testEmail); err != nil {
		t.Fatalf("RemoveProjectMember: %v", err)
	}

	// DeleteProject is NOT exercised here, and that is a finding, not an omission.
	//
	// `DELETE /orgs/{org}/projects/{project}` is registered AccessOrgAdmin, and
	// api.resolve() populates scope.ProjectID only when access >= AccessProjectRead
	// (see Access.needsProject). So handleDeleteProject calls DeleteProject with the
	// ZERO uuid, matches no row, and returns 404 -- for every caller, including a
	// site admin who owns the org. The route cannot work.
	//
	// The fix is one word in internal/api (register it AccessProjectAdmin, which
	// also resolves the project), and internal/api is outside this task's write
	// boundary, so it is reported rather than patched here. `bakery project delete`
	// sends the right request and starts working the moment the route is fixed.
	if err := c.DeleteOrg(ctx, "widgets-co"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	// ---- logout, then a command: the server is real, so this is a real 401 path,
	// and it must still say "run bakery login".
	if _, err := tokens.Delete(c.Server()); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := c.ListOrgs(ctx); err == nil {
		t.Error("ListOrgs succeeded after logout")
	} else if err.Error() != "not signed in to this server: run bakery login" {
		t.Errorf("err = %q, want the run-bakery-login sentence", err)
	}
}

// TestGarbageBearerIsRejectedByTheRealServer: the ID token is verified, not
// trusted. A forged one -- right shape, wrong signature -- must 401, and the CLI
// must render that 401 as an instruction.
func TestGarbageBearerIsRejectedByTheRealServer(t *testing.T) {
	idp := newRealIDP(t)
	server := newBakery(t, idp)

	tokens, _ := tempStore(t)

	c, err := NewClient(server.URL, tokens)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Signed by a DIFFERENT key: everything about it is right except the one thing
	// that matters.
	attacker := newIDPKey(t)
	now := time.Now()

	forged := attacker.sign(t, map[string]any{
		"iss": idp.server.URL, "aud": testClientID, "sub": "user-subject-1",
		"email": testEmail, "groups": []string{testGroup},
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	if err := tokens.Put(c.Server(), Token{
		IDToken: forged, RefreshToken: "", Expiry: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err = c.Me(t.Context())
	if err == nil {
		t.Fatal("the server accepted a token signed by the wrong key")
	}

	if !errors.Is(err, ErrNeedsLogin) {
		t.Errorf("err = %v, want ErrNeedsLogin", err)
	}
}
