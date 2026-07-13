package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
)

// TestMain gives the DB-backed tests below a real, ephemeral Postgres. dbtest
// spins a container (or uses TEST_DB_URL), migrates a template, and hands each
// test a private database that is dropped on cleanup.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// ---------------------------------------------------------------------------
// The end-to-end harness
//
// This is the ONLY place a real auth.Principal exists in this package's tests --
// and it exists because dev-login mints a real session, which auth.Service turns
// into a real Principal. Everything above uses a fake api.Principal, which cannot
// become an auth.Principal (sealed). So the tests here are what prove the real
// wiring -- serviceKeyMinter's type assertion, the LoadAndSave chain, the database
// CHECK constraints, and the downgrade transaction -- actually work end to end.
// ---------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	store  *db.Store
	auth   *auth.Service

	// api is the live *API the server is serving. It is here so a test can flip a
	// deployment flag (--allow-local-site-admins) on a RUNNING installation, which is
	// the only honest way to ask "can an operator still clean up the local grants that
	// predate the lockdown?".
	api *API
}

// newHarness boots the full stack: real Postgres, real auth.Service with dev-login
// on, real API, real HTTP server, and a cookie-jar client.
func newHarness(t *testing.T) *harness {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	log := discardLogger()

	sessions := auth.NewSessionManager(auth.NewSessionStore(pool, log), false)

	svc, err := auth.New(auth.Deps{
		Store: store, Sessions: sessions, Provider: nil, Groups: nil,
		Metrics: nil, Log: log, DevLogin: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	if err := svc.SeedDevLogin(t.Context()); err != nil {
		t.Fatalf("seed dev login: %v", err)
	}

	a, err := New(Config{
		Store: store, Auth: svc, Metrics: metrics.New(), Log: log,
		AllowSelfServeOrgs: true, AllowLocalSiteAdmins: true,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	return &harness{
		t: t, server: server, store: store, auth: svc, api: a,
		client: &http.Client{Jar: jar},
	}
}

// req issues a request through the real chain.
func (h *harness) req(method, path, body string, hdr map[string]string) (int, []byte) {
	h.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	r, err := http.NewRequestWithContext(h.t.Context(), method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}

	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}

	for k, v := range hdr {
		r.Header.Set(k, v)
	}

	resp, err := h.client.Do(r)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, raw
}

// devLogin establishes a real session for dev@bakery.local (site admin, owner of
// dev-org, admin of dev-org/playground).
func (h *harness) devLogin() {
	h.t.Helper()

	status, body := h.req(http.MethodPost, Prefix+"/auth/dev-login", "", nil)
	if status != http.StatusOK {
		h.t.Fatalf("dev-login: status = %d, body %s", status, body)
	}
}

// ---------------------------------------------------------------------------

// TestEndToEndDevLoginAndMe drives the whole chain against a real database: the
// metrics middleware, scs LoadAndSave, authenticate, the mux, and the guard.
func TestEndToEndDevLoginAndMe(t *testing.T) {
	h := newHarness(t)

	// Anonymous first: /me must be a 401 with the error envelope, not a panic and
	// not an empty body.
	status, body := h.req(http.MethodGet, Prefix+"/me", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous /me: status = %d, want 401 (body %s)", status, body)
	}

	var envelope ErrorBody
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the 401 is not the error envelope: %q", body)
	}

	if envelope.Error.Code != CodeUnauthorized {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, CodeUnauthorized)
	}

	h.devLogin()

	status, body = h.req(http.MethodGet, Prefix+"/me", "", nil)
	if status != http.StatusOK {
		t.Fatalf("/me: status = %d, body %s", status, body)
	}

	var me Me
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatalf("decode /me: %v", err)
	}

	if me.Email != auth.DevEmail {
		t.Errorf("email = %q, want %q", me.Email, auth.DevEmail)
	}

	if !me.IsSiteAdmin {
		t.Error("the dev user must be a site admin")
	}

	if me.Method != string(auth.MethodDev) {
		t.Errorf("method = %q, want %q", me.Method, auth.MethodDev)
	}

	if len(me.Orgs) != 1 || me.Orgs[0].Slug != auth.DevOrgSlug {
		t.Errorf("orgs = %+v, want exactly dev-org", me.Orgs)
	}

	if len(me.Projects) != 1 || me.Projects[0].Slug != auth.DevProjectSlug {
		t.Errorf("projects = %+v, want exactly playground", me.Projects)
	}
}

// TestEndToEndReservedSlugHitsTheDatabaseCheckToo.
//
// The Go check (slug.Check) renders the friendly 422. But it is a MIRROR, not the
// authority: bakery_slug_ok is a CHECK constraint on the column. This asserts both
// halves against a live Postgres -- the API's 422, and then that the database
// itself refuses the same slug when handed it directly. If someone deletes the Go
// check, the second half still holds and the failure is a 422 from toAPIError's
// 23514 mapping rather than a corrupted routing table.
func TestEndToEndReservedSlugHitsTheDatabaseCheckToo(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	for _, reserved := range []string{"cache", "api", "v2", "ac", "cas", "blobs"} {
		t.Run(reserved, func(t *testing.T) {
			status, body := h.req(http.MethodPost, Prefix+"/orgs",
				`{"slug":"`+reserved+`","name":"x"}`, nil)

			if status != http.StatusUnprocessableEntity {
				t.Errorf("API: status = %d, want 422 (body %s)", status, body)
			}

			// And the DATABASE refuses it too, with the API bypassed entirely.
			_, err := h.store.CreateOrganization(t.Context(),
				repositoryCreateOrg(reserved))
			if err == nil {
				t.Errorf("the database ACCEPTED reserved slug %q; "+
					"bakery_slug_ok is not enforcing the denylist", reserved)
			}
		})
	}

	// The control: a valid slug is accepted by both.
	status, body := h.req(http.MethodPost, Prefix+"/orgs", `{"slug":"widgets","name":"Widgets"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("a valid slug was refused: status = %d, body %s", status, body)
	}
}

// TestEndToEndAPIKeyLifecycle exercises the REAL key minter -- serviceKeyMinter,
// which type-asserts back to auth.Principal -- with a REAL Principal.
//
// This is the show-once assertion at full strength: the token comes from
// crypto/rand inside internal/auth, is written to the database only as a SHA-256,
// and is then hunted for in every subsequent response AND in the database row
// itself.
func TestEndToEndAPIKeyLifecycle(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	status, body := h.req(http.MethodPost, base+"/keys", `{"name":"ci-writer","scope":"write"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create key: status = %d, body %s", status, body)
	}

	var created CreatedAPIKey
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	token := created.Token

	if !strings.HasPrefix(token, auth.TokenPrefix) {
		t.Fatalf("token = %q, want the %q prefix", token, auth.TokenPrefix)
	}

	if len(token) != len(auth.TokenPrefix)+43 {
		t.Errorf("token length = %d, want %d (bkry_ + 43 base64url chars)",
			len(token), len(auth.TokenPrefix)+43)
	}

	// 1. The token appears in NO other response.
	status, listBody := h.req(http.MethodGet, base+"/keys", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list keys: status = %d, body %s", status, listBody)
	}

	if strings.Contains(string(listBody), token) {
		t.Fatalf("THE TOKEN LEAKED into the list response: %s", listBody)
	}

	var keys ListResponse[APIKey]
	if err := json.Unmarshal(listBody, &keys); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	if len(keys.Items) != 1 {
		t.Fatalf("listed %d keys, want 1", len(keys.Items))
	}

	if keys.Items[0].TokenPrefix != created.TokenPrefix {
		t.Errorf("token_prefix = %q, want %q", keys.Items[0].TokenPrefix, created.TokenPrefix)
	}

	// 2. The token is not in the DATABASE either -- only its hash is. This is the
	// claim that makes "shown exactly once" true rather than aspirational: there is
	// no column that could hold it.
	rows, err := h.store.Pool().Query(t.Context(),
		`SELECT token_prefix, token_sha256 FROM api_keys`)
	if err != nil {
		t.Fatalf("query api_keys: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var prefix string

		var hash []byte

		if err := rows.Scan(&prefix, &hash); err != nil {
			t.Fatalf("scan: %v", err)
		}

		if strings.Contains(prefix, token) {
			t.Fatal("the plaintext token is stored in token_prefix")
		}

		if string(hash) == token {
			t.Fatal("the plaintext token is stored in token_sha256")
		}

		if !auth.TokenMatchesHash(token, hash) {
			t.Error("the stored hash does not match the issued token")
		}
	}

	// 3. The key WORKS as a cache credential...
	status, meBody := h.req(http.MethodGet, Prefix+"/me", "",
		map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusOK {
		t.Fatalf("/me with the key: status = %d, body %s", status, meBody)
	}

	var keyMe Me
	if err := json.Unmarshal(meBody, &keyMe); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if keyMe.Method != string(auth.MethodAPIKey) {
		t.Errorf("method = %q, want api_key", keyMe.Method)
	}

	// ...and the key principal does NOT inherit the owner's site admin, even though
	// the dev user IS one. A delegation must not become a master key.
	if keyMe.IsSiteAdmin {
		t.Error("an API-key principal inherited its owner's site-admin role")
	}

	if len(keyMe.Orgs) != 0 {
		t.Errorf("an API-key principal reported org memberships: %+v", keyMe.Orgs)
	}

	// 4. ...but it is REFUSED on the control plane. The key is for cache traffic.
	status, _ = h.req(http.MethodGet, base+"/keys", "",
		map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusForbidden {
		t.Errorf("listing keys with an API key: status = %d, want 403", status)
	}
}

// TestEndToEndRoleDowngradeRevokesWriteKeysInOneTransaction.
//
// This is the invariant the whole join-free key design rests on. Key validation
// deliberately does NOT join project_memberships -- a second index probe on a path
// that runs thousands of times in parallel at the start of every BitBake build is
// not affordable -- so a key's scope is capped at the member's role AT GRANT TIME
// and never re-checked. The consequence: a role downgrade must revoke the keys that
// now exceed it, or a demoted writer keeps write access to the cache indefinitely.
//
// The revocation is in the SAME TRANSACTION as the downgrade. Two statements would
// leave a window -- and a crash between them would make that window permanent.
func TestEndToEndRoleDowngradeRevokesWriteKeysInOneTransaction(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	// The dev user is a project ADMIN, so they may mint a write key.
	status, body := h.req(http.MethodPost, base+"/keys", `{"name":"ci-writer","scope":"write"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create write key: status = %d, body %s", status, body)
	}

	var writeKey CreatedAPIKey
	if err := json.Unmarshal(body, &writeKey); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A read key too -- it must SURVIVE the downgrade. Revoking everything would
	// pass a naive test and be wrong.
	status, body = h.req(http.MethodPost, base+"/keys", `{"name":"ci-reader","scope":"read"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create read key: status = %d, body %s", status, body)
	}

	var readKey CreatedAPIKey
	if err := json.Unmarshal(body, &readKey); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Both keys authenticate right now.
	for _, k := range []CreatedAPIKey{writeKey, readKey} {
		if status, _ := h.req(http.MethodGet, Prefix+"/me", "",
			map[string]string{"Authorization": "Bearer " + k.Token}); status != http.StatusOK {
			t.Fatalf("key %q does not authenticate before the downgrade: status = %d", k.Name, status)
		}
	}

	// THE DOWNGRADE: project admin -> reader.
	status, body = h.req(http.MethodPut, base+"/members/"+auth.DevEmail, `{"role":"reader"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("downgrade: status = %d, body %s", status, body)
	}

	// The WRITE key is dead.
	status, _ = h.req(http.MethodGet, Prefix+"/me", "",
		map[string]string{"Authorization": "Bearer " + writeKey.Token})
	if status != http.StatusUnauthorized {
		t.Errorf("the write key still authenticates after the downgrade: status = %d, want 401.\n"+
			"A demoted writer retains write access to the cache -- the key was minted at "+
			"write scope and validation never re-checks the role.", status)
	}

	// The READ key still lives: it is within a reader's authority.
	status, _ = h.req(http.MethodGet, Prefix+"/me", "",
		map[string]string{"Authorization": "Bearer " + readKey.Token})
	if status != http.StatusOK {
		t.Errorf("the read key was revoked by the downgrade: status = %d, want 200. "+
			"Only keys EXCEEDING the new role may be revoked.", status)
	}

	// And the revocation is visible in the metadata, with revoked_at set.
	status, listBody := h.req(http.MethodGet, base+"/keys", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list keys: status = %d", status)
	}

	var keys ListResponse[APIKey]
	if err := json.Unmarshal(listBody, &keys); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, k := range keys.Items {
		switch k.Name {
		case "ci-writer":
			if k.RevokedAt == nil {
				t.Error("the write key's revoked_at is null after the downgrade")
			}
		case "ci-reader":
			if k.RevokedAt != nil {
				t.Error("the read key was revoked")
			}
		}
	}
}

// TestEndToEndBackendCRUD covers the config rows. No backend serves traffic in M1;
// these rows are what M2's backends will hang off, and what blob.Service keys object
// metadata on.
func TestEndToEndBackendCRUD(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug + "/backends"

	status, body := h.req(http.MethodPost, base,
		`{"kind":"sstate","config":{"max_bytes":1000}}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create backend: status = %d, body %s", status, body)
	}

	var created Backend
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Defaults: enabled, and reads require auth.
	if !created.Enabled || !created.ReadAuthRequired {
		t.Errorf("defaults = enabled:%v read_auth_required:%v, want both true",
			created.Enabled, created.ReadAuthRequired)
	}

	// UNIQUE (project_id, kind): a second sstate mount on the same project is a 409,
	// not a 500. That uniqueness IS the routing grammar -- /cache/{org}/{project}/sstate/...
	// must name exactly one mount.
	status, body = h.req(http.MethodPost, base, `{"kind":"sstate"}`, nil)
	if status != http.StatusConflict {
		t.Errorf("a duplicate sstate backend: status = %d, want 409 (body %s)", status, body)
	}

	// An unknown kind is a 422 with the closed vocabulary, not a 500 from the enum.
	status, _ = h.req(http.MethodPost, base, `{"kind":"redis"}`, nil)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("an unknown backend kind: status = %d, want 422", status)
	}

	// Patch: a partial update leaves the untouched fields alone.
	status, body = h.req(http.MethodPatch, base+"/sstate", `{"enabled":false}`, nil)
	if status != http.StatusOK {
		t.Fatalf("patch: status = %d, body %s", status, body)
	}

	var patched Backend
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if patched.Enabled {
		t.Error("enabled was not set to false")
	}

	if !patched.ReadAuthRequired {
		t.Error("read_auth_required was clobbered by a patch that did not mention it")
	}

	if !strings.Contains(string(patched.Config), "max_bytes") {
		t.Errorf("config was clobbered by a patch that did not mention it: %s", patched.Config)
	}

	// The project now reports the backend.
	status, body = h.req(http.MethodGet,
		Prefix+"/orgs/"+auth.DevOrgSlug+"/projects/"+auth.DevProjectSlug, "", nil)
	if status != http.StatusOK {
		t.Fatalf("get project: status = %d", status)
	}

	var project Project
	if err := json.Unmarshal(body, &project); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(project.Backends) != 1 || project.Backends[0] != "sstate" {
		t.Errorf("project.backends = %v, want [sstate]", project.Backends)
	}

	status, _ = h.req(http.MethodDelete, base+"/sstate", "", nil)
	if status != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", status)
	}

	status, _ = h.req(http.MethodGet, base+"/sstate", "", nil)
	if status != http.StatusNotFound {
		t.Errorf("get after delete: status = %d, want 404", status)
	}
}

// TestEndToEndLogoutDestroysTheSession.
func TestEndToEndLogoutDestroysTheSession(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	if status, _ := h.req(http.MethodGet, Prefix+"/me", "", nil); status != http.StatusOK {
		t.Fatalf("/me before logout: status = %d", status)
	}

	if status, _ := h.req(http.MethodPost, Prefix+"/auth/logout", "", nil); status != http.StatusNoContent {
		t.Fatalf("logout: status = %d, want 204", status)
	}

	if status, _ := h.req(http.MethodGet, Prefix+"/me", "", nil); status != http.StatusUnauthorized {
		t.Errorf("/me after logout: status = %d, want 401 -- the session survived logout", status)
	}
}
