package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// snippetToken is the plaintext the fake minter hands back to the snippet endpoint.
const snippetToken = "bkry_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

// TestSnippetDefaultYocto is the happy path: a project writer asks for the default
// snippet (empty body) and gets the verified Yocto local.conf, a netrc line with the
// freshly-minted token, and the push commands -- all pointed at this server's host,
// this org and this project.
func TestSnippetDefaultYocto(t *testing.T) {
	store := fixtureStore(t)
	minter := &fakeMinter{token: snippetToken}
	a := testAPI(t, store, minter)

	w := do(t, a, principals(t)["proj_write"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", "")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}

	var out SnippetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The default is yocto at WRITE scope: the snippet documents the push, and a
	// write key reads too, so one key drives the whole workflow.
	if minter.got.Scope != auth.ScopeWrite {
		t.Fatalf("minted scope = %q, want write", minter.got.Scope)
	}

	if out.Tool != SnippetToolYocto {
		t.Fatalf("tool = %q, want yocto", out.Tool)
	}

	// httptest's default host is example.com; no TLS => http.
	wantBase := "http://example.com/cache/acme/firmware"
	if out.BaseURL != wantBase {
		t.Fatalf("base_url = %q, want %q", out.BaseURL, wantBase)
	}

	if out.Host != "example.com" {
		t.Fatalf("host = %q, want example.com", out.Host)
	}

	// The verified local.conf lines, with the resolved base URL and the mandatory
	// downloadfilename suffix.
	for _, want := range []string{
		`SSTATE_MIRRORS ?= "file://.* http://example.com/cache/acme/firmware/sstate/PATH;downloadfilename=PATH"`,
		`INHERIT += "own-mirrors"`,
		`SOURCE_MIRROR_URL ?= "http://example.com/cache/acme/firmware/downloads"`,
	} {
		if !strings.Contains(out.LocalConf, want) {
			t.Errorf("local_conf missing line:\n  want %q\n  in\n%s", want, out.LocalConf)
		}
	}

	// No secret ever lands in local.conf -- bitbake takes it from netrc/env.
	if strings.Contains(out.LocalConf, snippetToken) {
		t.Error("local_conf must not contain the token; the credential belongs in netrc")
	}

	// The netrc line carries the token, keyed by hostname.
	if !strings.Contains(out.Netrc, "machine example.com") || !strings.Contains(out.Netrc, snippetToken) {
		t.Errorf("netrc = %q, want machine example.com with the token", out.Netrc)
	}

	// Both push commands, with the token via BAKERY_API_KEY and the CLI's positional
	// org/project/dir grammar.
	if len(out.PushCommands) != 2 {
		t.Fatalf("push_commands = %d, want 2 (%v)", len(out.PushCommands), out.PushCommands)
	}

	if !strings.Contains(out.PushCommands[0], "bakery sstate push acme firmware") {
		t.Errorf("sstate push command = %q", out.PushCommands[0])
	}

	if !strings.Contains(out.PushCommands[1], "bakery downloads push acme firmware") {
		t.Errorf("downloads push command = %q", out.PushCommands[1])
	}

	// The minted token is on the wire in the key object.
	if out.APIKey.Token != snippetToken {
		t.Fatalf("api_key.token = %q, want the plaintext", out.APIKey.Token)
	}
}

// TestSnippetHonorsForwardedHeaders proves the snippet points at the client-facing
// origin, not the private hop a TLS-terminating proxy forwards over.
func TestSnippetHonorsForwardedHeaders(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, &fakeMinter{token: snippetToken})

	mux := http.NewServeMux()
	a.routes = nil
	a.mount(mux)

	r := httptest.NewRequest(http.MethodPost, Prefix+"/orgs/acme/projects/firmware/snippet", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "bakery.corp")
	r = r.WithContext(withPrincipal(r.Context(), principals(t)["proj_write"]))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}

	var out SnippetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.BaseURL != "https://bakery.corp/cache/acme/firmware" {
		t.Fatalf("base_url = %q, want https://bakery.corp/...", out.BaseURL)
	}
}

// TestSnippetReaderCannotRequestWriteScope: the scope cap lives in auth.CreateAPIKey,
// so a reader asking for a write snippet is refused with a 403, never handed a key
// beyond their role. The fake minter stands in for that cap by returning the sentinel.
func TestSnippetReaderCannotRequestWriteScope(t *testing.T) {
	store := fixtureStore(t)
	minter := &fakeMinter{err: auth.ErrScopeExceedsRole}
	a := testAPI(t, store, minter)

	w := do(t, a, principals(t)["proj_read"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", `{"scope":"write"}`)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
}

// TestSnippetReaderGetsReadScope: a reader may still generate a read-scoped snippet
// for themselves -- ProjectRead is the route floor precisely so this works.
func TestSnippetReaderGetsReadScope(t *testing.T) {
	store := fixtureStore(t)
	minter := &fakeMinter{token: snippetToken}
	a := testAPI(t, store, minter)

	w := do(t, a, principals(t)["proj_read"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", `{"scope":"read"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}

	if minter.got.Scope != auth.ScopeRead {
		t.Fatalf("minted scope = %q, want read", minter.got.Scope)
	}
}

// TestSnippetRejectsUnknownTool: an unknown tool is a 422 at request time, not an
// empty snippet a user pastes and then wonders why nothing caches. bazel is a VALID
// tool as of M4, so the invalid example is buck2 -- a real build tool Bakery does not
// target, which is exactly the paste this gate is meant to catch.
func TestSnippetRejectsUnknownTool(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, &fakeMinter{token: snippetToken})

	w := do(t, a, principals(t)["proj_write"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", `{"tool":"buck2"}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
}

// snippetFor mints a snippet for tool at scope and returns the decoded response. It
// drives the same handler the yocto tests use; the forwarded-proto header lets a test
// choose http vs https (and so grpc vs grpcs) deterministically.
func snippetFor(t *testing.T, tool, scope, forwardedProto, forwardedHost string) SnippetResponse {
	t.Helper()

	store := fixtureStore(t)
	a := testAPI(t, store, &fakeMinter{token: snippetToken})

	mux := http.NewServeMux()
	a.routes = nil
	a.mount(mux)

	body := fmt.Sprintf(`{"tool":%q,"scope":%q}`, tool, scope)
	r := httptest.NewRequest(http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	if forwardedProto != "" {
		r.Header.Set("X-Forwarded-Proto", forwardedProto)
	}

	if forwardedHost != "" {
		r.Header.Set("X-Forwarded-Host", forwardedHost)
	}

	r = r.WithContext(withPrincipal(r.Context(), principals(t)["proj_write"]))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("tool %s: status = %d, want 201 (body %s)", tool, w.Code, w.Body.String())
	}

	var out SnippetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return out
}

// snippetFileContent returns the Content of the single expected config file, failing
// if the file count is not one.
func snippetFileContent(t *testing.T, out SnippetResponse) string {
	t.Helper()

	if len(out.Files) != 1 {
		t.Fatalf("files = %d, want 1 (%+v)", len(out.Files), out.Files)
	}

	return out.Files[0].Content
}

// snippetEnv returns the value of the named env var, failing if it is absent.
func snippetEnv(t *testing.T, out SnippetResponse, name string) string {
	t.Helper()

	for _, e := range out.Env {
		if e.Name == name {
			return e.Value
		}
	}

	t.Fatalf("env var %q not found in %+v", name, out.Env)

	return ""
}

// TestSnippetMoonTokenIsAName is moon's two silent traps: the token is the NAME of an
// env var, so it must be ABSENT from workspace.yml and present only in the export; and
// the host needs a scheme AND a port (grpcs://host:443), because hostOnly() -- which
// strips the port -- is the wrong helper for a gRPC endpoint. No push commands.
func TestSnippetMoonTokenIsAName(t *testing.T) {
	out := snippetFor(t, SnippetToolMoon, "write", "https", "bakery.corp")

	yaml := snippetFileContent(t, out)

	// The literal env-var NAME is present; the token value is NOT in the file.
	if !strings.Contains(yaml, "token: 'BAKERY_TOKEN'") {
		t.Errorf("workspace.yml must carry the env-var NAME `token: 'BAKERY_TOKEN'`:\n%s", yaml)
	}

	if strings.Contains(yaml, snippetToken) {
		t.Errorf("workspace.yml must NOT contain the token -- that silently disables moon's cache:\n%s", yaml)
	}

	// The credential lives in the export, and only there.
	if got := snippetEnv(t, out, "BAKERY_TOKEN"); got != snippetToken {
		t.Errorf("BAKERY_TOKEN = %q, want the token", got)
	}

	// host carries a scheme and an explicit port; https => grpcs, :443.
	if !strings.Contains(yaml, "host: 'grpcs://bakery.corp:443'") {
		t.Errorf("workspace.yml host must be grpcs://bakery.corp:443 (scheme + port):\n%s", yaml)
	}

	if len(out.PushCommands) != 0 {
		t.Errorf("moon has no push path; push_commands = %v", out.PushCommands)
	}
}

// TestSnippetCcacheTraps covers ccache's four traps: @layout=bazel is mandatory, the
// userinfo carries a colon (token as username, empty password), the scheme is http
// (ccache cannot speak https), and @connect-timeout=1000 overrides the 100ms default.
func TestSnippetCcacheTraps(t *testing.T) {
	out := snippetFor(t, SnippetToolCcache, "write", "https", "bakery.corp")

	conf := snippetFileContent(t, out)

	if !strings.Contains(conf, "@layout=bazel") {
		t.Errorf("ccache.conf must set @layout=bazel (default subdirs => an unrouted 404):\n%s", conf)
	}

	// Userinfo must be `<token>:@host` -- token as username, empty password, colon
	// mandatory. The password-then-username fallback in auth is what makes it work.
	if !strings.Contains(conf, "http://"+snippetToken+":@") {
		t.Errorf("ccache userinfo must be http://<token>:@host (colon mandatory):\n%s", conf)
	}

	// http only, even though the request arrived over https.
	if strings.Contains(conf, "https://") {
		t.Errorf("ccache cannot speak https; the config must be http:// only:\n%s", conf)
	}

	if !strings.Contains(conf, "@connect-timeout=1000") {
		t.Errorf("ccache.conf must set @connect-timeout=1000 (default 100ms is too tight):\n%s", conf)
	}

	// A write key does not get read-only.
	if strings.Contains(conf, "read-only=true") {
		t.Errorf("a write-scoped ccache snippet must not be read-only:\n%s", conf)
	}
}

// TestSnippetCcacheReadOnly: a read-scoped key emits read-only=true, so ccache never
// issues the PUT whose 403 would latch the whole backend (reads included) off.
func TestSnippetCcacheReadOnly(t *testing.T) {
	out := snippetFor(t, SnippetToolCcache, "read", "", "")

	conf := snippetFileContent(t, out)

	if !strings.Contains(conf, "@read-only=true") {
		t.Errorf("a read-scoped ccache snippet must set @read-only=true:\n%s", conf)
	}
}

// TestSnippetSccacheKeyPrefix: SCCACHE_WEBDAV_KEY_PREFIX is REQUIRED (without it the
// keys land at a prefix Bakery does not serve), the endpoint is https, and the token
// rides SCCACHE_WEBDAV_TOKEN as a Bearer credential.
func TestSnippetSccacheKeyPrefix(t *testing.T) {
	out := snippetFor(t, SnippetToolSccache, "write", "https", "bakery.corp")

	if got := snippetEnv(t, out, "SCCACHE_WEBDAV_KEY_PREFIX"); got != "sccache" {
		t.Errorf("SCCACHE_WEBDAV_KEY_PREFIX = %q, want sccache (required)", got)
	}

	if got := snippetEnv(t, out, "SCCACHE_WEBDAV_ENDPOINT"); got != "https://bakery.corp/cache/acme/firmware" {
		t.Errorf("SCCACHE_WEBDAV_ENDPOINT = %q", got)
	}

	if got := snippetEnv(t, out, "SCCACHE_WEBDAV_TOKEN"); got != snippetToken {
		t.Errorf("SCCACHE_WEBDAV_TOKEN = %q, want the token", got)
	}
}

// TestSnippetBazelNoCompression: the .bazelrc carries the cache endpoint, the instance
// name (gRPC cannot carry a URL path) and the Bearer header -- and MUST NOT set
// --remote_cache_compression, which hard-fails the connection because we advertise
// IDENTITY only.
func TestSnippetBazelNoCompression(t *testing.T) {
	out := snippetFor(t, SnippetToolBazel, "write", "https", "bakery.corp")

	rc := snippetFileContent(t, out)

	for _, want := range []string{
		"build --remote_cache=grpcs://bakery.corp:443",
		"build --remote_instance_name=acme/firmware",
		"build --remote_header=authorization=Bearer " + snippetToken,
	} {
		if !strings.Contains(rc, want) {
			t.Errorf(".bazelrc missing %q:\n%s", want, rc)
		}
	}

	if strings.Contains(rc, "remote_cache_compression") {
		t.Errorf(".bazelrc must NOT set --remote_cache_compression (we advertise IDENTITY only):\n%s", rc)
	}
}

// TestSnippetRequiresProjectRead: an outsider gets nothing.
func TestSnippetRequiresProjectRead(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, &fakeMinter{token: snippetToken})

	w := do(t, a, principals(t)["outsider"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", "")

	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 403/404 for an outsider (body %s)", w.Code, w.Body.String())
	}
}
