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

// TestSnippetContainerdBothAuthPaths pins the containerd trap this snippet exists to
// avoid: the hosts.toml must carry BOTH the default Bearer-challenge path (which needs
// no credential in the file at all) and the commented-out static-header alternative
// that pins the real token -- and it must not append /v2 itself, since containerd does
// that.
func TestSnippetContainerdBothAuthPaths(t *testing.T) {
	out := snippetFor(t, SnippetToolContainerd, "read", "https", "bakery.corp")

	toml := snippetFileContent(t, out)

	// The mount is the docker/v2 family, with NO trailing /v2 -- containerd appends it.
	mount := "https://bakery.corp/cache/acme/firmware/docker"
	if !strings.Contains(toml, fmt.Sprintf(`[host.%q]`, mount)) {
		t.Errorf("hosts.toml missing the [host] block for %s:\n%s", mount, toml)
	}

	if strings.Contains(toml, mount+"/v2") {
		t.Errorf("hosts.toml must NOT append /v2 itself -- containerd does that:\n%s", toml)
	}

	// Default path: the challenge is implicit (no WWW-Authenticate line belongs in a
	// config file), but the block must say so and must NOT require a credential.
	if !strings.Contains(toml, "Bearer challenge") {
		t.Errorf("hosts.toml must document the default Bearer-challenge auth path:\n%s", toml)
	}

	// Alternative path: the static header table, commented out, carrying the real token.
	if !strings.Contains(toml, fmt.Sprintf(`[host.%q.header]`, mount)) {
		t.Errorf("hosts.toml missing the static [host.\"...\".header] alternative:\n%s", toml)
	}

	if !strings.Contains(toml, `Authorization = "Bearer `+snippetToken+`"`) {
		t.Errorf("hosts.toml header alternative must pin the real token:\n%s", toml)
	}
}

// TestSnippetBuildkitTraps pins BuildKit's mirror shape (the OPPOSITE prefix position
// from containerd: no /cache, no /docker -- Bakery's second route family) and the
// bare-Basic skip: BuildKit silently disables a mirror whose credential has an empty
// half, so the doc comment must tell the operator to set BOTH fields.
func TestSnippetBuildkitTraps(t *testing.T) {
	out := snippetFor(t, SnippetToolBuildkit, "read", "https", "bakery.corp")

	toml := snippetFileContent(t, out)

	if !strings.Contains(toml, `mirrors = ["bakery.corp/acme/firmware"]`) {
		t.Errorf("buildkitd.toml mirrors must be the bare tenant path (no /cache, no /docker):\n%s", toml)
	}

	if strings.Contains(toml, "/cache/") || strings.Contains(toml, "/docker") {
		t.Errorf("buildkitd.toml must not carry containerd's /cache/.../docker shape:\n%s", toml)
	}

	if !strings.Contains(toml, "docker login bakery.corp -u "+snippetToken+" -p "+snippetToken) {
		t.Errorf("buildkitd.toml must document BOTH credential fields (bare Basic silently skips the mirror):\n%s", toml)
	}
}

// TestSnippetPodmanNoNamespace pins podman's shape: containers/image never sends
// ?ns=, so the mirror is a bare path with no query param, and its credentials cannot
// be inherited from a docker.io login (cross-domain creds are stripped) so the doc must
// carry a direct `podman login` line.
func TestSnippetPodmanNoNamespace(t *testing.T) {
	out := snippetFor(t, SnippetToolPodman, "read", "https", "bakery.corp")

	toml := snippetFileContent(t, out)

	if !strings.Contains(toml, `location = "bakery.corp/acme/firmware"`) {
		t.Errorf("registries.conf mirror location must be the bare tenant path:\n%s", toml)
	}

	if strings.Contains(toml, `location = "bakery.corp/acme/firmware?ns=`) {
		t.Errorf("registries.conf mirror location must not carry a ?ns= query -- containers/image never sends it:\n%s", toml)
	}

	if !strings.Contains(toml, "default_upstream") {
		t.Errorf("registries.conf must note the backend needs default_upstream set:\n%s", toml)
	}

	if !strings.Contains(toml, "podman login bakery.corp -u "+snippetToken+" -p "+snippetToken) {
		t.Errorf("registries.conf must document a direct podman login (cross-domain creds are stripped):\n%s", toml)
	}
}

// TestSnippetDockerHubOnlyWarning pins three things at once: the daemon.json is valid
// JSON with no token embedded (Docker Engine has no per-mirror credential slot), the
// mirror URL is Hub-only and path-prefixed, and the loud credential-transit warning
// (product decision: support Docker Engine, but warn) surfaces on the response.
func TestSnippetDockerHubOnlyWarning(t *testing.T) {
	out := snippetFor(t, SnippetToolDocker, "read", "https", "bakery.corp")

	body := snippetFileContent(t, out)

	var decoded struct {
		RegistryMirrors []string `json:"registry-mirrors"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("daemon.json must be valid JSON (no comments -- dockerd parses it strictly): %v\n%s", err, body)
	}

	want := []string{"https://bakery.corp/cache/acme/firmware/docker"}
	if len(decoded.RegistryMirrors) != 1 || decoded.RegistryMirrors[0] != want[0] {
		t.Errorf("registry-mirrors = %v, want %v", decoded.RegistryMirrors, want)
	}

	if strings.Contains(body, snippetToken) {
		t.Errorf("daemon.json must NOT contain the token -- Docker Engine has no per-mirror credential slot:\n%s", body)
	}

	if len(out.Warnings) == 0 {
		t.Fatal("docker snippet must carry a Warnings entry -- daemon.json can't carry a comment")
	}

	joined := strings.Join(out.Warnings, " ")
	for _, want := range []string{"Docker Hub", "unscoped", "never logged", "authenticated"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warning text missing %q:\n%s", want, joined)
		}
	}
}

// TestSnippetOCIToolsHaveNoPushCommands: pull-through only, no registry push API --
// every M5 client gets an empty PushCommands, exactly like the M4 clients.
func TestSnippetOCIToolsHaveNoPushCommands(t *testing.T) {
	for _, tool := range []string{SnippetToolContainerd, SnippetToolBuildkit, SnippetToolPodman, SnippetToolDocker} {
		out := snippetFor(t, tool, "read", "https", "bakery.corp")
		if len(out.PushCommands) != 0 {
			t.Errorf("%s: push_commands = %v, want none (pull-through only)", tool, out.PushCommands)
		}
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
