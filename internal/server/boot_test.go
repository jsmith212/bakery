package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// TestBootEndToEnd boots the SAME code path `bakery serve` boots -- Boot(), not a
// lookalike assembled in a test file -- against a real Postgres, and drives the
// whole M1 control plane through it over real sockets:
//
//	dev-login -> create org -> create project -> mint an API key -> GET /me
//	with that key -> /metrics is on the metrics port and NOT the public one
//	-> /healthz and /readyz.
//
// This is the test that catches the failure mode M1 is most exposed to: every
// package compiles, every unit test is green, and nothing is wired together.
func TestBootEndToEnd(t *testing.T) {
	t.Parallel()

	_, dsn := dbtest.NewWithDSN(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan [2]string, 1)
	done := make(chan error, 1)

	go func() {
		done <- Boot(ctx, BootParams{
			Cmd: config.ServeCmd{
				DBFlags:     config.DBFlags{DBURL: dsn},
				Host:        "127.0.0.1",
				Port:        0,
				MetricsAddr: "127.0.0.1:0",
				StorageDir:  t.TempDir(),
				// The one and only way this can be turned on. No API path, no UI
				// control, no database column.
				DevLoginEnabled: true,
			},
			Version: "test",
			Dist:    testDist(),
			Ready: func(public, metricsAddr net.Addr) {
				ready <- [2]string{public.String(), metricsAddr.String()}
			},
		})
	}()

	var addrs [2]string

	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("Boot returned before binding: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("server never became ready")
	}

	public, private := "http://"+addrs[0], "http://"+addrs[1]

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}

	c := &http.Client{Jar: jar}

	// ---- dev login. No credential; it mints a session for the seeded site admin.
	var login map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/auth/dev-login", "", nil, http.StatusOK, &login)

	if login["email"] != "dev@bakery.local" {
		t.Fatalf("dev-login: got email %v, want dev@bakery.local", login["email"])
	}

	// ---- GET /me on the session the cookie jar just picked up.
	var me map[string]any

	do(t, c, http.MethodGet, public+"/api/v1/me", "", nil, http.StatusOK, &me)

	if me["is_site_admin"] != true {
		t.Fatalf("/me: got is_site_admin %v, want true", me["is_site_admin"])
	}

	// A session established by dev-login reports method=dev, not method=session:
	// the audit trail must not launder a credential-less login into a normal one.
	if me["method"] != "dev" {
		t.Fatalf("/me: got method %v, want dev", me["method"])
	}

	// ---- create an org.
	//
	// THE HEADLINE FLOW, AND IT IS THE ONE M1 COULD NOT DO: create org -> create
	// project -> mint a key, all inside the BRAND-NEW org, with no group map, no
	// LDAP round-trip and no pre-existing membership anywhere.
	//
	// Under M1 this dead-ended at the project-member grant: org membership was 100%
	// claim-derived, so a fresh org had no members -- not even the site admin who
	// had just created it -- the {user} path segment resolved against the org roster
	// and 404'd, and CreateAPIKey then refused with scope_exceeds_role because it
	// requires a real project membership and site admin deliberately does not bypass
	// it. M1.5 gives the creator a LOCAL owner grant in the same transaction as the
	// org. That is what makes every line below this one work.
	var org map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs", "",
		map[string]any{"slug": "acme", "name": "Acme"}, http.StatusCreated, &org)

	if org["slug"] != "acme" {
		t.Fatalf("create org: got slug %v, want acme", org["slug"])
	}

	userID, _ := me["user_id"].(string)
	if userID == "" {
		t.Fatal("/me returned no user_id")
	}

	// ---- the creator is on the new org's roster, as a LOCAL owner. Provenance is
	// visible: an in-app grant that no group claim justifies must be legible as one,
	// not laundered into something the IdP appears to have said.
	var roster map[string]any

	do(t, c, http.MethodGet, public+"/api/v1/orgs/acme/members", "", nil, http.StatusOK, &roster)

	items, _ := roster["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("new org roster: got %d members, want exactly the creator: %+v", len(items), roster)
	}

	creator, _ := items[0].(map[string]any)
	if creator["org_role"] != "owner" || creator["local_role"] != "owner" {
		t.Fatalf("creator of a new org: got org_role=%v local_role=%v, want owner/owner -- the "+
			"creator MUST own the org they just made, or it is an org nobody can ever join: %+v",
			creator["org_role"], creator["local_role"], creator)
	}

	// The effective role is owner because the LOCAL half says so and the claim half
	// says nothing. Reporting it as claim-derived would be a lie -- and one the next
	// login would rightly reconcile away, taking the org's only owner with it.
	if creator["oidc_role"] != nil || creator["org_role_source"] != "local" {
		t.Errorf("creator's grant is not reported as purely local: oidc_role=%v source=%v",
			creator["oidc_role"], creator["org_role_source"])
	}

	if creator["granted_by_email"] != "dev@bakery.local" || creator["granted_at"] == nil {
		t.Errorf("the local grant carries no provenance: granted_by_email=%v granted_at=%v",
			creator["granted_by_email"], creator["granted_at"])
	}

	// ---- create a project IN THE NEW ORG.
	var project map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs/acme/projects", "",
		map[string]any{"slug": "widget", "name": "Widget"}, http.StatusCreated, &project)

	// ---- grant the caller a project role. Org owner is enough to ADMINISTER the
	// project, but a key is a per-user delegation of a project role, and the schema
	// enforces that with a foreign key onto project_memberships: there is no row to
	// cap the scope against until this call has happened. This is the call that
	// 404'd under M1, because the {user} segment resolves against the ORG roster --
	// which was empty.
	do(t, c, http.MethodPut, public+"/api/v1/orgs/acme/projects/widget/members/"+userID, "",
		map[string]any{"role": "admin"}, http.StatusOK, nil)

	// ---- mint an API key. The plaintext is returned exactly once, here. Under M1
	// this was a 409 scope_exceeds_role, and the console's headline flow dead-ended.
	var key map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs/acme/projects/widget/keys", "",
		map[string]any{"name": "ci", "scope": "write"}, http.StatusCreated, &key)

	token, _ := key["token"].(string)
	if !strings.HasPrefix(token, "bkry_") {
		t.Fatalf("create key: got token %q, want a bkry_ prefix", token)
	}

	// ---- GET /me as the KEY, on a fresh client with no session cookie. The key
	// must authenticate on its own, and must NOT inherit its owner's site admin:
	// a delegation is not a master key.
	var keyMe map[string]any

	do(t, &http.Client{}, http.MethodGet, public+"/api/v1/me", token, nil, http.StatusOK, &keyMe)

	if keyMe["method"] != "api_key" {
		t.Errorf("/me as a key: got method %v, want api_key", keyMe["method"])
	}

	if keyMe["is_site_admin"] != false {
		t.Errorf("/me as a key: got is_site_admin %v, want false -- an API key must not "+
			"inherit its owner's site admin", keyMe["is_site_admin"])
	}

	// ---- an unauthenticated call is 401, not a 500 and not a 200.
	do(t, &http.Client{}, http.MethodGet, public+"/api/v1/me", "", nil, http.StatusUnauthorized, nil)

	// ---- metrics: private port only.
	if body := get(t, private+"/metrics"); !strings.Contains(body, "bakery_auth_attempts_total") {
		t.Errorf("metrics port did not serve the exposition")
	}

	// The storage layer must actually be WIRED at boot, not just configured: if
	// Boot never constructs the byte store, --storage-dir is dead config and the
	// bakery_storage_* family never appears. The instrumented store pre-registers
	// its op series at zero, so this line proves the store was built.
	if body := get(t, private+"/metrics"); !strings.Contains(body, `bakery_storage_operations_total{driver="local",op="put",result="hit"} 0`) {
		t.Errorf("metrics port did not expose the local storage series -- the byte store was not wired at boot")
	}

	if body := get(t, public+"/metrics"); strings.Contains(body, "bakery_auth_attempts_total") {
		t.Error("the PUBLIC listener served the metrics exposition")
	}

	// ---- health and readiness. readyz really pings the pool.
	if body := get(t, public+"/healthz"); body != "ok\n" {
		t.Errorf("/healthz: got %q", body)
	}

	if body := get(t, public+"/readyz"); body != "ok\n" {
		t.Errorf("/readyz: got %q", body)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Boot: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Boot did not return after the context was cancelled")
	}
}

// TestBootRefusesASecondInstance proves the boot lock. Two bakeries on one
// database would each hold their own route cache and their own LRU, and neither
// would know when the other invalidated a row.
func TestBootRefusesASecondInstance(t *testing.T) {
	t.Parallel()

	_, dsn := dbtest.NewWithDSN(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan struct{}, 1)
	first := make(chan error, 1)

	go func() {
		first <- Boot(ctx, BootParams{
			Cmd: config.ServeCmd{
				DBFlags:         config.DBFlags{DBURL: dsn},
				Host:            "127.0.0.1",
				Port:            0,
				MetricsAddr:     "127.0.0.1:0",
				StorageDir:      t.TempDir(),
				DevLoginEnabled: true,
			},
			Version: "test",
			Dist:    testDist(),
			Ready:   func(_, _ net.Addr) { ready <- struct{}{} },
		})
	}()

	select {
	case <-ready:
	case err := <-first:
		t.Fatalf("the first instance failed to boot: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the first instance never became ready")
	}

	second := Boot(t.Context(), BootParams{
		Cmd: config.ServeCmd{
			DBFlags:     config.DBFlags{DBURL: dsn},
			Host:        "127.0.0.1",
			Port:        0,
			MetricsAddr: "127.0.0.1:0",
			StorageDir:  t.TempDir(),
		},
		Version: "test",
		Dist:    testDist(),
	})

	if second == nil {
		t.Fatal("a second instance booted on the same database")
	}

	if !strings.Contains(second.Error(), "boot lock") {
		t.Fatalf("second instance: got %v, want a boot-lock refusal", second)
	}

	cancel()
	<-first
}

// TestBootRejectsAnUnusableStorageDir proves --storage-dir is LIVE config, not
// decoration: an unwritable or nonsensical path must be a loud boot failure, not a
// green boot that EACCESes the first time a cache backend writes an object.
//
// The bad path is chosen so it fails for every uid, root included: it is a
// directory nested UNDER a regular file, so the store's MkdirAll gets ENOTDIR
// rather than a permission error a root-run CI would sail past.
func TestBootRejectsAnUnusableStorageDir(t *testing.T) {
	t.Parallel()

	_, dsn := dbtest.NewWithDSN(t)

	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed a regular file: %v", err)
	}

	// A path whose parent is a file: MkdirAll cannot create it (ENOTDIR) no matter
	// who runs the test.
	badDir := filepath.Join(notADir, "objects")

	// Boot must FAIL, and must fail before it binds a listener. Run it in a
	// goroutine with its own cancelable context so that a regression -- Boot
	// sailing past the storage dir and serving forever -- is a prompt test failure
	// (Ready fires, or the deadline elapses) rather than a ten-minute package hang.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan struct{}, 1)
	done := make(chan error, 1)

	go func() {
		done <- Boot(ctx, BootParams{
			Cmd: config.ServeCmd{
				DBFlags:         config.DBFlags{DBURL: dsn},
				Host:            "127.0.0.1",
				Port:            0,
				MetricsAddr:     "127.0.0.1:0",
				StorageDir:      badDir,
				DevLoginEnabled: true,
			},
			Version: "test",
			Dist:    testDist(),
			// If Boot ever binds a listener with this storage dir, it validated nothing.
			Ready: func(_, _ net.Addr) { ready <- struct{}{} },
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Boot returned nil for an unusable --storage-dir; the directory was never constructed or probed")
		}

		if !strings.Contains(err.Error(), "storage") {
			t.Fatalf("Boot: got %v, want a storage-directory failure", err)
		}
	case <-ready:
		cancel()
		<-done
		t.Fatal("Boot bound a listener with an unusable --storage-dir; the path was never validated")
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Boot neither failed nor became ready with an unusable --storage-dir")
	}
}

// do issues one API call. token, when set, is presented as a Bearer API key; body,
// when set, is sent as JSON (which is also what satisfies the API's content-type
// CSRF gate).
func do(
	t *testing.T,
	c *http.Client,
	method, target, token string,
	body any,
	wantStatus int,
	out any,
) {
	t.Helper()

	var rdr io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, target, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// The API's CSRF gate rejects a write without a JSON content type, so every
	// write carries one -- even the bodiless dev-login POST.
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, redact(target), err)
	}

	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, redact(target), err)
	}

	if res.StatusCode != wantStatus {
		t.Fatalf("%s %s: got %d, want %d\nbody: %s",
			method, redact(target), res.StatusCode, wantStatus, raw)
	}

	if out == nil {
		return
	}

	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: decode %s: %v", method, redact(target), raw, err)
	}
}

func redact(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}

	return u.Path
}
