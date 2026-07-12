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

	// ---- create an org (site admin only).
	var org map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs", "",
		map[string]any{"slug": "acme", "name": "Acme"}, http.StatusCreated, &org)

	if org["slug"] != "acme" {
		t.Fatalf("create org: got slug %v, want acme", org["slug"])
	}

	// ---- create a project.
	//
	// In dev-org, not in acme, and that is not an accident: ORG membership is
	// derived from OIDC group claims and reconciled at login, so a brand new org
	// has no members at all until the group map names it -- not even the site admin
	// who just created it. The dev seed puts the dev user in dev-org, which is the
	// only org anyone is a member of on this deployment. Creating a project in acme
	// would work; getting a project ROLE in it, which is what a key is minted
	// against, would not.
	var project map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs/dev-org/projects", "",
		map[string]any{"slug": "widget", "name": "Widget"}, http.StatusCreated, &project)

	// ---- grant the caller a project role. Site admin is enough to ADMINISTER the
	// project, but a key is a per-user delegation of a project role, and the schema
	// enforces that with a foreign key onto project_memberships: there is no row to
	// cap the scope against until this call has happened. Project roles are managed
	// in-app -- unlike org roles, which are claim-derived and read-only here.
	userID, _ := me["user_id"].(string)
	if userID == "" {
		t.Fatal("/me returned no user_id")
	}

	do(t, c, http.MethodPut, public+"/api/v1/orgs/dev-org/projects/widget/members/"+userID, "",
		map[string]any{"role": "admin"}, http.StatusOK, nil)

	// ---- mint an API key. The plaintext is returned exactly once, here.
	var key map[string]any

	do(t, c, http.MethodPost, public+"/api/v1/orgs/dev-org/projects/widget/keys", "",
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
