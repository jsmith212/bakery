package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The two credential surfaces stage 3 added: /user/tokens and
// /orgs/{org}/robots. Both are one-time-reveal mints behind a door no machine
// credential may come through, and both close an IDOR in the STATEMENT rather
// than in the handler.

const (
	userTokenPlaintext = "bkru_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	orgTokenPlaintext  = "bkro_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func credentialFixture(t *testing.T) *fakeStore {
	t.Helper()

	store := fixtureStore(t)

	store.userTokens = []repository.ListUserTokensForUserRow{
		{
			ID: mustUUID(t, keyAnnaID), UserID: mustUUID(t, userAnnaID),
			Name: "laptop", TokenPrefix: "bkru_a7f13d00", MaxScope: auth.ScopeWrite,
		},
		{
			ID: mustUUID(t, keyMarkoID), UserID: mustUUID(t, userMarkoID),
			Name: "marko-laptop", TokenPrefix: "bkru_e8d7b600", MaxScope: auth.ScopeRead,
		},
	}

	store.robots = []repository.Robot{
		{
			ID: mustUUID(t, keyAnnaID), OrgID: mustUUID(t, orgAcmeID), Name: "ci",
			Description: "the build fleet",
			CreatedBy:   mustUUID(t, userAnnaID), CreatedByEmail: "anna@example.com",
		},
	}

	store.orgTokens = []repository.ListOrgTokensForOrgRow{
		{
			ID: mustUUID(t, keyMarkoID), RobotID: mustUUID(t, keyAnnaID),
			OrgID: mustUUID(t, orgAcmeID), Name: "primary",
			TokenPrefix: "bkro_a7f13d00", Scope: auth.ScopeWrite,
			CreatedByEmail: "anna@example.com",
		},
	}

	return store
}

func newCredentialMinter(token string) *fakeMinter {
	return &fakeMinter{
		token: token, err: nil,
		got:          auth.CreateKeyInput{},
		gotUserToken: auth.CreateUserTokenInput{},
		gotOrgToken:  auth.CreateOrgTokenInput{},
	}
}

// ---------------------------------------------------------------------------
// Personal access tokens.
// ---------------------------------------------------------------------------

// TestUserTokenIsShownExactlyOnce mirrors TestKeyTokenIsShownExactlyOnce, and for
// the same reason: it searches the RAW BYTES of every other response, not a
// field, so a leak arriving through an embedded struct or a stray tag is caught.
func TestUserTokenIsShownExactlyOnce(t *testing.T) {
	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(userTokenPlaintext))
	anna := principals(t)["proj_admin"]

	created := do(t, a, anna, http.MethodPost, Prefix+"/user/tokens",
		`{"name":"ci","scope":"write"}`)

	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", created.Code, created.Body)
	}

	if !strings.Contains(created.Body.String(), userTokenPlaintext) {
		t.Fatalf("the create response does not carry the plaintext: %s", created.Body)
	}

	listed := do(t, a, anna, http.MethodGet, Prefix+"/user/tokens", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (%s)", listed.Code, listed.Body)
	}

	if strings.Contains(listed.Body.String(), userTokenPlaintext) {
		t.Errorf("the LIST response carries a plaintext token: %s", listed.Body)
	}

	// Metadata only, and scoped to the caller: Marko's token must not be in Anna's
	// list. The production query carries user_id in its predicate; the fake mirrors
	// it, so this assertion holds against Postgres too.
	var body struct {
		Items []UserToken `json:"items"`
	}

	if err := json.Unmarshal(listed.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Items) != 1 || body.Items[0].Name != "laptop" {
		t.Errorf("list = %+v, want only the caller's own token", body.Items)
	}
}

// TestUserTokenRevokeIsOwnerScoped is the IDOR gate.
//
// {token} is a caller-supplied uuid and RevokeUserToken takes an id. A handler
// that passed it through would let any signed-in human revoke any token in the
// installation given only its id. The predicate carries user_id, so another
// user's id matches nothing -- and the response is the SAME 204 as an
// already-revoked token, because "not yours" must not be distinguishable from
// "no such token".
func TestUserTokenRevokeIsOwnerScoped(t *testing.T) {
	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(userTokenPlaintext))
	anna := principals(t)["proj_admin"]

	// Marko's token, presented by Anna.
	res := do(t, a, anna, http.MethodDelete, Prefix+"/user/tokens/"+keyMarkoID, "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204 (%s)", res.Code, res.Body)
	}

	// The call reached the store -- so the scoping really is the statement's, not a
	// handler's early return -- and it matched nothing.
	if !hasCall(store, "RevokeUserToken:"+keyMarkoID) {
		t.Error("the revoke never reached the store; the scoping is not in the statement")
	}

	// Anna's own token revokes for real.
	res = do(t, a, anna, http.MethodDelete, Prefix+"/user/tokens/"+keyAnnaID, "")
	if res.Code != http.StatusNoContent {
		t.Errorf("revoking own token = %d, want 204 (%s)", res.Code, res.Body)
	}
}

func TestUserTokenValidation(t *testing.T) {
	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(userTokenPlaintext))
	anna := principals(t)["proj_admin"]

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	far := time.Now().Add(100 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"an empty name", `{"name":"  ","scope":"read"}`, http.StatusUnprocessableEntity},
		{"an unknown scope", `{"name":"ci","scope":"admin"}`, http.StatusUnprocessableEntity},
		{"an expiry in the past", `{"name":"ci","scope":"read","expires_at":"` + past + `"}`,
			http.StatusUnprocessableEntity},
		{"an expiry a century out", `{"name":"ci","scope":"read","expires_at":"` + far + `"}`,
			http.StatusUnprocessableEntity},
		{"no expiry at all is FINE -- never is a supported deployment",
			`{"name":"ci","scope":"read"}`, http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := do(t, a, anna, http.MethodPost, Prefix+"/user/tokens", tt.body)
			if res.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", res.Code, tt.want, res.Body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Robots.
// ---------------------------------------------------------------------------

func TestRobotListCarriesItsTokensAndTheAuditTrail(t *testing.T) {
	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(orgTokenPlaintext))
	admin := principals(t)["org_admin"]

	res := do(t, a, admin, http.MethodGet, Prefix+"/orgs/acme/robots", "")
	if res.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (%s)", res.Code, res.Body)
	}

	var body struct {
		Items []Robot `json:"items"`
	}

	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Items) != 1 {
		t.Fatalf("robots = %d, want 1", len(body.Items))
	}

	robot := body.Items[0]
	if len(robot.Tokens) != 1 || robot.Tokens[0].Name != "primary" {
		t.Errorf("robot tokens = %+v, want the one live token", robot.Tokens)
	}

	// The ROBOTS card is justified as the audit surface for a write-everywhere
	// credential, so the audit columns have to be on the wire. created_by_email is
	// the one that survives the human -- see 000016.
	if robot.CreatedByEmail != "anna@example.com" {
		t.Errorf("created_by_email = %q, want the snapshot that survives the human",
			robot.CreatedByEmail)
	}
}

// TestRobotTokenExpiryIsRequiredAndCapped.
//
// org_tokens.expires_at is NOT NULL because a robot deliberately outlives its
// creator: the mint-time cap and the membership cascade that revoke an API key do
// not exist for it, so expiry is the only countervailing control and "never" must
// not be reachable -- including by omitting the field and having the API invent a
// default.
func TestRobotTokenExpiryIsRequiredAndCapped(t *testing.T) {
	store := credentialFixture(t)
	minter := newCredentialMinter(orgTokenPlaintext)
	a := testAPI(t, store, minter)
	admin := principals(t)["org_admin"]

	target := fmt.Sprintf("%s/orgs/acme/robots/%s/tokens", Prefix, keyAnnaID)

	ok := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	tooFar := time.Now().Add(auth.MaxOrgTokenLifetime + 24*time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"absent expiry", `{"name":"primary","scope":"write"}`, http.StatusUnprocessableEntity},
		{"an expiry in the past",
			`{"name":"primary","scope":"write","expires_at":"` + past + `"}`,
			http.StatusUnprocessableEntity},
		{"beyond 365 days",
			`{"name":"primary","scope":"write","expires_at":"` + tooFar + `"}`,
			http.StatusUnprocessableEntity},
		{"within a year", `{"name":"primary","scope":"write","expires_at":"` + ok + `"}`,
			http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := do(t, a, admin, http.MethodPost, target, tt.body)
			if res.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", res.Code, tt.want, res.Body)
			}
		})
	}

	if minter.gotOrgToken.ExpiresAt.IsZero() {
		t.Error("the minter was called with a zero expiry")
	}
}

// TestRobotFromAnotherOrgIs404 closes the cross-tenant hole.
//
// The guard resolves {org} and {project} and nothing else, so {robot} is this
// package's own responsibility. resolveRobot reads the row scoped by the guard's
// authorized org id: a well-formed id belonging to another tenant is not in that
// scope, so it is a 404 -- never a 403, which would confirm the robot exists.
func TestRobotFromAnotherOrgIs404(t *testing.T) {
	store := credentialFixture(t)

	// The same robot id, but owned by globex.
	store.robots[0].OrgID = mustUUID(t, orgOtherID)

	a := testAPI(t, store, newCredentialMinter(orgTokenPlaintext))
	admin := principals(t)["org_admin"] // acme's admin

	for _, target := range []string{
		Prefix + "/orgs/acme/robots/" + keyAnnaID,
		fmt.Sprintf("%s/orgs/acme/robots/%s/tokens/%s", Prefix, keyAnnaID, keyMarkoID),
	} {
		res := do(t, a, admin, http.MethodDelete, target, "")
		if res.Code != http.StatusNotFound {
			t.Errorf("DELETE %s = %d, want 404 (%s)", target, res.Code, res.Body)
		}
	}
}

// TestDeletingARobotIsScopedToTheOrg. The delete is one of only two structural
// revocation legs a robot token has, so it must reach the store scoped -- not be
// short-circuited in the handler, and not be reachable for another tenant.
func TestDeletingARobotIsScopedToTheOrg(t *testing.T) {
	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(orgTokenPlaintext))
	admin := principals(t)["org_admin"]

	res := do(t, a, admin, http.MethodDelete, Prefix+"/orgs/acme/robots/"+keyAnnaID, "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%s)", res.Code, res.Body)
	}

	if !hasCall(store, "DeleteRobotForOrg:"+keyAnnaID) {
		t.Error("the delete never reached the store")
	}
}

// hasCall reports whether the fake store recorded a mutating call.
func hasCall(store *fakeStore, want string) bool {
	for _, got := range store.calls {
		if got == want {
			return true
		}
	}

	return false
}

// TestUserTokenPurge: the purge is owner-scoped like the revoke (a colleague's
// revoked token is absent from your listing and answers 204 without touching the
// store -- "not yours" stays indistinguishable from "gone"), refuses your own LIVE
// token with 409 not_revoked, and erases your own revoked one.
func TestUserTokenPurge(t *testing.T) {
	revokedAt := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}

	store := credentialFixture(t)
	store.userTokens[1].RevokedAt = revokedAt // marko's, revoked

	a := testAPI(t, store, newCredentialMinter(userTokenPlaintext))
	anna := principals(t)["proj_admin"]

	// Marko's revoked token, presented by Anna: absent from her listing, so 204 and
	// no delete ever reaches the store.
	res := do(t, a, anna, http.MethodDelete, Prefix+"/user/tokens/"+keyMarkoID+"?purge=true", "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("purge of another user's token = %d, want 204 (%s)", res.Code, res.Body)
	}

	if hasCall(store, "DeleteRevokedUserToken:"+keyMarkoID) {
		t.Error("a foreign token's purge reached the store; the listing is not owner-scoped")
	}

	// Anna's own LIVE token: refused.
	res = do(t, a, anna, http.MethodDelete, Prefix+"/user/tokens/"+keyAnnaID+"?purge=true", "")
	if res.Code != http.StatusConflict || decodeErr(t, res).Code != CodeNotRevoked {
		t.Fatalf("purge of a live token = %d %s, want 409 not_revoked", res.Code, res.Body)
	}

	// Revoked, then purged: gone.
	store.userTokens[0].RevokedAt = revokedAt

	res = do(t, a, anna, http.MethodDelete, Prefix+"/user/tokens/"+keyAnnaID+"?purge=true", "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("purge of own revoked token = %d, want 204 (%s)", res.Code, res.Body)
	}

	for _, row := range store.userTokens {
		if uuidString(row.ID) == keyAnnaID {
			t.Error("the revoked token is still in the store after its purge")
		}
	}
}

// TestOrgTokenPurge: robot- and org-scoped like the revoke. A live token is refused
// (409 not_revoked); a revoked one is erased; a token that is not this robot's is
// absent and answers 204 with the store untouched.
func TestOrgTokenPurge(t *testing.T) {
	revokedAt := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}

	store := credentialFixture(t)
	a := testAPI(t, store, newCredentialMinter(userTokenPlaintext))
	admin := principals(t)["org_admin"]

	// The fixture token (keyMarkoID) belongs to robot keyAnnaID.
	target := Prefix + "/orgs/acme/robots/" + keyAnnaID + "/tokens/" + keyMarkoID + "?purge=true"

	res := do(t, a, admin, http.MethodDelete, target, "")
	if res.Code != http.StatusConflict || decodeErr(t, res).Code != CodeNotRevoked {
		t.Fatalf("purge of a live robot token = %d %s, want 409 not_revoked", res.Code, res.Body)
	}

	store.orgTokens[0].RevokedAt = revokedAt

	// Wrong robot: absent, 204, nothing erased.
	wrongRobot := Prefix + "/orgs/acme/robots/" + keyMarkoID + "/tokens/" + keyMarkoID + "?purge=true"

	res = do(t, a, admin, http.MethodDelete, wrongRobot, "")
	if res.Code != http.StatusNotFound && res.Code != http.StatusNoContent {
		t.Fatalf("purge via the wrong robot = %d, want 404 or 204 (%s)", res.Code, res.Body)
	}

	if hasCall(store, "DeleteRevokedOrgToken:"+keyMarkoID) {
		t.Fatal("a purge through the wrong robot reached the store")
	}

	// The right robot, revoked: gone.
	res = do(t, a, admin, http.MethodDelete, target, "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("purge of a revoked robot token = %d, want 204 (%s)", res.Code, res.Body)
	}

	if len(store.orgTokens) != 0 {
		t.Error("the revoked robot token is still in the store after its purge")
	}
}
