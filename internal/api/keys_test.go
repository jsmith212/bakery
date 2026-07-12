package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// theToken is the plaintext the fake minter hands back. Every assertion below is
// about where this string is, and -- far more importantly -- where it is not.
const theToken = "bkry_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func keyFixture(t *testing.T) *fakeStore {
	t.Helper()

	store := fixtureStore(t)
	firmware := mustUUID(t, projFirmwareID)

	store.keys = []repository.ListAPIKeysForProjectRow{
		{
			ID: mustUUID(t, keyAnnaID), UserID: mustUUID(t, userAnnaID), ProjectID: firmware,
			Name: "ci-writer", TokenPrefix: "bkry_a7f13d00", Scope: auth.ScopeWrite,
		},
		{
			ID: mustUUID(t, keyMarkoID), UserID: mustUUID(t, userMarkoID), ProjectID: firmware,
			Name: "marko-dev", TokenPrefix: "bkry_e8d7b600", Scope: auth.ScopeRead,
		},
	}

	return store
}

// TestKeyTokenIsShownExactlyOnce is the show-once assertion, and it is written to
// fail loudly rather than to pass quietly.
//
// It asserts the plaintext appears in the CREATE response, and then it goes looking
// for that exact string in the raw bytes of every other response the key endpoints
// can produce. Not "the Token field is empty" -- the raw body, because a leak that
// arrives through an embedded struct, a stray `omitempty`, or a debug field would
// pass a field-level check and fail this one.
func TestKeyTokenIsShownExactlyOnce(t *testing.T) {
	store := keyFixture(t)
	minter := &fakeMinter{token: theToken, err: nil, got: auth.CreateKeyInput{}}
	a := testAPI(t, store, minter)

	admin := principals(t)["proj_admin"]

	// 1. Create. The token IS here, exactly once, and this is the only place.
	created := do(t, a, admin, http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/keys",
		`{"name":"ci-writer-2","scope":"write"}`)

	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", created.Code, created.Body.String())
	}

	var out CreatedAPIKey
	if err := json.Unmarshal(created.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	if out.Token != theToken {
		t.Fatalf("create response token = %q, want the plaintext %q", out.Token, theToken)
	}

	if !strings.Contains(created.Body.String(), theToken) {
		t.Fatal("the create response body does not carry the plaintext token")
	}

	// Register the key as if it had been persisted, so it shows up in the list.
	store.keys = append(store.keys, repository.ListAPIKeysForProjectRow{
		ID: out.ID2(t), UserID: mustUUID(t, userAnnaID),
		ProjectID: mustUUID(t, projFirmwareID),
		Name:      "ci-writer-2", TokenPrefix: "bkry_abcd1234", Scope: auth.ScopeWrite,
	})

	// 2. Every OTHER response. The plaintext must appear in NONE of them.
	others := []struct {
		name           string
		method, target string
		body           string
	}{
		{"list keys", http.MethodGet, Prefix + "/orgs/acme/projects/firmware/keys", ""},
		{"get project", http.MethodGet, Prefix + "/orgs/acme/projects/firmware", ""},
		{"list projects", http.MethodGet, Prefix + "/orgs/acme/projects", ""},
		{"list members", http.MethodGet, Prefix + "/orgs/acme/projects/firmware/members", ""},
		{"me", http.MethodGet, Prefix + "/me", ""},
		{"list orgs", http.MethodGet, Prefix + "/orgs", ""},
	}

	for _, tt := range others {
		t.Run("the token is absent from: "+tt.name, func(t *testing.T) {
			w := do(t, a, admin, tt.method, tt.target, tt.body)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			if strings.Contains(w.Body.String(), theToken) {
				t.Fatalf("THE PLAINTEXT TOKEN LEAKED into %s: %s", tt.name, w.Body.String())
			}

			// Nor may any *prefix* of the secret escape beyond the 8 display
			// characters the schema deliberately keeps.
			if strings.Contains(w.Body.String(), theToken[:20]) {
				t.Fatalf("a substring of the token leaked into %s", tt.name)
			}
		})
	}
}

// ID2 parses the wire id back to a pgtype.UUID for the fixture top-up above.
func (k APIKey) ID2(t *testing.T) pgtype.UUID {
	t.Helper()

	return mustUUID(t, k.ID)
}

// TestListedKeysCarryNoSecretField is the structural half of show-once: the type
// the list endpoint returns has NO field that could hold a token, so a leak there
// is not a bug that can be introduced by a careless edit -- it is a compile error.
//
// If someone adds a `Token` (or `Hash`, or `Secret`) field to APIKey, this fails,
// and they are made to think about why CreatedAPIKey is a separate type.
func TestListedKeysCarryNoSecretField(t *testing.T) {
	forbidden := []string{"token", "secret", "hash", "plaintext", "key"}

	typ := reflect.TypeFor[APIKey]()

	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.ToLower(field.Name)

		// token_prefix is the one allowed exception: it is 8 characters of a
		// 256-bit random, deliberately non-secret, and it is what lets the console
		// tell keys apart after the reveal.
		if field.Name == "TokenPrefix" {
			continue
		}

		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("APIKey.%s looks like it could hold a secret. "+
					"The list type must be structurally incapable of carrying the token; "+
					"put it on CreatedAPIKey instead.", field.Name)
			}
		}
	}
}

// TestRevokeKeyIsNotAnIDOR.
//
// repository.RevokeAPIKey takes a bare id and revokes it, with no project check --
// there is nothing at the query layer to check against. So a handler that passed
// r.PathValue("key") straight through would let ANY project reader ANYWHERE revoke
// ANY key in the installation, given only its id. Three lines of entirely
// reasonable-looking code.
//
// The handler therefore looks the id up in the AUTHORIZED project's key list, and
// anything not on it is a 404 -- regardless of whether it exists elsewhere.
func TestRevokeKeyIsNotAnIDOR(t *testing.T) {
	// A key that exists, but in another tenant's project.
	foreignKeyID := "99999999-9999-9999-9999-999999999999"

	tests := []struct {
		name    string
		role    string
		keyID   string
		want    int
		revoked bool
		whyNot  string
	}{
		{
			name: "an admin revokes a key in their project", role: "proj_admin",
			keyID: keyMarkoID, want: http.StatusNoContent, revoked: true,
		},
		{
			name: "a reader revokes their OWN key", role: "own_key_reader",
			keyID: keyMarkoID, want: http.StatusNoContent, revoked: true,
		},
		{
			name: "a reader may NOT revoke someone else's key", role: "proj_read",
			keyID: keyMarkoID, want: http.StatusForbidden, revoked: false,
			whyNot: "a reader revoked a colleague's key",
		},
		{
			name: "a key from another project is a 404, not a revoke", role: "proj_admin",
			keyID: foreignKeyID, want: http.StatusNotFound, revoked: false,
			whyNot: "THE IDOR: a key outside the authorized project was revoked",
		},
		{
			name: "an outsider cannot reach the project at all", role: "outsider",
			keyID: keyMarkoID, want: http.StatusNotFound, revoked: false,
			whyNot: "a member of another org revoked a key",
		},
	}

	cast := principals(t)

	// A reader who happens to OWN keyMarko: same user id as the key's owner.
	ownKeyReader := &fakePrincipal{
		userID: mustUUID(t, userMarkoID), email: "marko@acme.dev", displayName: "Marko Ilic",
		method: auth.MethodSession, siteRole: auth.SiteRoleUser,
		orgs:     map[pgtype.UUID]auth.OrgRole{mustUUID(t, orgAcmeID): auth.OrgRoleMember},
		projects: map[pgtype.UUID]auth.ProjectRole{mustUUID(t, projFirmwareID): auth.ProjectRoleReader},
		key:      nil,
	}
	cast["own_key_reader"] = ownKeyReader

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := keyFixture(t)
			a := testAPI(t, store, nil)

			w := do(t, a, cast[tt.role], http.MethodDelete,
				Prefix+"/orgs/acme/projects/firmware/keys/"+tt.keyID, "")

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.want, w.Body.String())
			}

			got := len(store.revokedKeys) > 0
			if got != tt.revoked {
				t.Errorf("key revoked = %v, want %v: %s", got, tt.revoked, tt.whyNot)
			}
		})
	}
}

// TestListKeysHidesOtherPeoplesKeysFromNonAdmins: a project reader sees their own
// keys, not the whole project's. Enumerating a colleague's key names, owners and
// last-used times is reconnaissance for no operational benefit; an ADMIN sees them
// all, because an admin is who revokes the leaked key belonging to someone who left.
func TestListKeysHidesOtherPeoplesKeysFromNonAdmins(t *testing.T) {
	tests := []struct {
		name string
		role string
		want []string
	}{
		{"a project admin sees every key", "proj_admin", []string{"ci-writer", "marko-dev"}},
		{"an org admin sees every key", "org_admin", []string{"ci-writer", "marko-dev"}},
		{"a reader sees only their own", "reader_marko", []string{"marko-dev"}},
	}

	cast := principals(t)
	cast["reader_marko"] = &fakePrincipal{
		userID: mustUUID(t, userMarkoID), email: "marko@acme.dev", displayName: "Marko Ilic",
		method: auth.MethodSession, siteRole: auth.SiteRoleUser,
		orgs:     map[pgtype.UUID]auth.OrgRole{mustUUID(t, orgAcmeID): auth.OrgRoleMember},
		projects: map[pgtype.UUID]auth.ProjectRole{mustUUID(t, projFirmwareID): auth.ProjectRoleReader},
		key:      nil,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAPI(t, keyFixture(t), nil)

			w := do(t, a, cast[tt.role], http.MethodGet,
				Prefix+"/orgs/acme/projects/firmware/keys", "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}

			var body ListResponse[APIKey]
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			got := make([]string, 0, len(body.Items))
			for _, k := range body.Items {
				got = append(got, k.Name)
			}

			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCreateKeyRejectsAnInvalidScope: the scope vocabulary is closed.
func TestCreateKeyRejectsAnInvalidScope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"read is valid", `{"name":"k","scope":"read"}`, http.StatusCreated},
		{"write is valid", `{"name":"k","scope":"write"}`, http.StatusCreated},
		{"admin is not a key scope", `{"name":"k","scope":"admin"}`, http.StatusUnprocessableEntity},
		{"an empty scope is refused", `{"name":"k","scope":""}`, http.StatusUnprocessableEntity},
		{"an empty name is refused", `{"name":"","scope":"read"}`, http.StatusUnprocessableEntity},
		{
			"an unknown field is refused, not silently dropped",
			`{"name":"k","scope":"read","user_id":"someone-else"}`,
			http.StatusBadRequest,
		},
	}

	admin := principals(t)["proj_admin"]

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minter := &fakeMinter{token: theToken, err: nil, got: auth.CreateKeyInput{}}
			a := testAPI(t, keyFixture(t), minter)

			w := do(t, a, admin, http.MethodPost,
				Prefix+"/orgs/acme/projects/firmware/keys", tt.body)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

// TestCreateKeyCannotMintForAnotherUser.
//
// "Mint a key on behalf of user X" is a credential-forging primitive: it makes the
// per-user key model -- and every audit trail that says "this cache write came from
// this human" -- a fiction. The request type has no user_id field, auth.CreateKeyInput
// has no user_id field, and DisallowUnknownFields means a client that sends one is
// TOLD, rather than having it silently ignored.
//
// The last part matters: a silently-ignored user_id would let an attacker believe
// they had failed, while an implementer who later ADDED the field would have no
// test standing in the way.
func TestCreateKeyCannotMintForAnotherUser(t *testing.T) {
	minter := &fakeMinter{token: theToken, err: nil, got: auth.CreateKeyInput{}}
	a := testAPI(t, keyFixture(t), minter)

	admin := principals(t)["proj_admin"]

	w := do(t, a, admin, http.MethodPost, Prefix+"/orgs/acme/projects/firmware/keys",
		`{"name":"k","scope":"write","user_id":"`+userMarkoID+`"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: a user_id must be REFUSED, not ignored", w.Code, http.StatusBadRequest)
	}

	if len(store(a).calls) != 0 {
		t.Errorf("the store was written to: %v", store(a).calls)
	}

	// And the type itself has no field to carry it.
	typ := reflect.TypeFor[CreateKeyRequest]()
	for i := range typ.NumField() {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "user") {
			t.Errorf("CreateKeyRequest.%s: a key is always minted for the CALLER; "+
				"a user_id field is a forgery primitive", typ.Field(i).Name)
		}
	}
}

// store narrows an API's Store back to the fake, for assertions.
func store(a *API) *fakeStore {
	s, ok := a.store.(*fakeStore)
	if !ok {
		panic("api test: store is not a *fakeStore")
	}

	return s
}
