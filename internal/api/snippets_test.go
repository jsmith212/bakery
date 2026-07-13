package api

import (
	"encoding/json"
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
// empty snippet a user pastes and then wonders why nothing caches.
func TestSnippetRejectsUnknownTool(t *testing.T) {
	store := fixtureStore(t)
	a := testAPI(t, store, &fakeMinter{token: snippetToken})

	w := do(t, a, principals(t)["proj_write"], http.MethodPost,
		Prefix+"/orgs/acme/projects/firmware/snippet", `{"tool":"bazel"}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
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
