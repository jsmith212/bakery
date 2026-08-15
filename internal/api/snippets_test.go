package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// snippetToken is the plaintext the fake minter hands back to the snippet endpoint.
const snippetToken = "bkry_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

// testGRPCAddr is a stand-in for --grpc-addr's default. The PORT is the only part
// the generator reads, and it is deliberately NOT 8080: the whole point of
// TestGRPCEndpointUsesGRPCAddrPortNotHTTPPort is that the two cannot be confused.
const testGRPCAddr = "127.0.0.1:9092"

// allSnippetBackends is every kind a snippet can be generated against.
var allSnippetBackends = []repository.BackendKind{
	repository.BackendKindSstate, repository.BackendKindDownloads,
	repository.BackendKindHashserv, repository.BackendKindBazel, repository.BackendKindOci,
}

// snippetStore is fixtureStore with the named backends configured and ENABLED on
// acme/firmware.
//
// The generator reads cache_backends and emits blocks only for what is there
// (B1), so every snippet test now has to say which backends the project has --
// which is the point: the old generator touched the store zero times and would
// have emitted the same config against a project with nothing configured at all.
func snippetStore(t *testing.T, kinds ...repository.BackendKind) *fakeStore {
	t.Helper()

	store := fixtureStore(t)
	firmware := mustUUID(t, projFirmwareID)

	for i, kind := range kinds {
		store.backends = append(store.backends, repository.CacheBackend{
			ID: int64(i + 1), ProjectID: firmware, Kind: kind, Enabled: true,
		})
	}

	return store
}

// snippetAPI is testAPI with the gRPC listener configured, which is the ordinary
// deployment. The tests that care about an ABSENT listener build their own.
func snippetAPI(t *testing.T, store Store, minter keyMinter) *API {
	t.Helper()

	a := testAPI(t, store, minter)
	a.grpcAddr = testGRPCAddr

	return a
}

// snippetPost drives the handler through a real mux, so the guard runs.
func snippetPost(t *testing.T, a *API, p Principal, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	a.routes = nil
	a.mount(mux)

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	target := Prefix + "/orgs/acme/projects/firmware/snippet"

	var r *http.Request
	if reader == nil {
		r = httptest.NewRequest(http.MethodPost, target, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, target, reader)
		r.Header.Set("Content-Type", "application/json")
	}

	for k, v := range hdr {
		r.Header.Set(k, v)
	}

	r = r.WithContext(withPrincipal(r.Context(), p))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	return w
}

// decodeSnippet decodes a successful response, failing on any other status.
func decodeSnippet(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) SnippetResponse {
	t.Helper()

	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", w.Code, wantStatus, w.Body.String())
	}

	var out SnippetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return out
}

// snippetFor mints a snippet for tool at scope against a project that has EVERY
// backend, and returns the decoded response. The forwarded-proto header lets a test
// choose http vs https deterministically.
func snippetFor(t *testing.T, tool, scope, forwardedProto, forwardedHost string) SnippetResponse {
	t.Helper()

	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

	hdr := map[string]string{}
	if forwardedProto != "" {
		hdr["X-Forwarded-Proto"] = forwardedProto
	}

	if forwardedHost != "" {
		hdr["X-Forwarded-Host"] = forwardedHost
	}

	w := snippetPost(t, a, principals(t)["proj_write"],
		fmt.Sprintf(`{"tool":%q,"scope":%q}`, tool, scope), hdr)

	return decodeSnippet(t, w, http.StatusCreated)
}

// snippetFileContent returns the Content of the single expected config file, failing
// if the file count is not one.
func snippetFileContent(t *testing.T, out SnippetResponse) string {
	t.Helper()

	if len(out.Files) != 1 {
		t.Fatalf("files = %d, want 1 (%+v; warnings %v)", len(out.Files), out.Files, out.Warnings)
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

// ---------------------------------------------------------------------------
// Yocto
// ---------------------------------------------------------------------------

// TestSnippetDefaultYocto is the happy path: a project writer asks for the default
// snippet (empty body) against a project with sstate, downloads and hashserv, and
// gets the verified five-block local.conf, both netrc lines, and the push commands.
func TestSnippetDefaultYocto(t *testing.T) {
	minter := &fakeMinter{token: snippetToken}
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), minter)

	out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", nil), http.StatusCreated)

	// The default scope is the caller's own CEILING; a writer's ceiling is write, and
	// the snippet documents the push, so one key drives the whole workflow.
	if minter.got.Scope != auth.ScopeWrite {
		t.Fatalf("minted scope = %q, want write", minter.got.Scope)
	}

	if out.Tool != SnippetToolYocto {
		t.Fatalf("tool = %q, want yocto", out.Tool)
	}

	// httptest's default host is example.com, which is not loopback, so the scheme
	// FAILS CLOSED to https even with no TLS and no forwarded header.
	wantBase := "https://example.com/cache/acme/firmware"
	if out.BaseURL != wantBase {
		t.Fatalf("base_url = %q, want %q", out.BaseURL, wantBase)
	}

	if out.Host != "example.com" {
		t.Fatalf("host = %q, want example.com", out.Host)
	}

	for _, want := range []string{
		`SSTATE_MIRRORS ?= "file://.* https://example.com/cache/acme/firmware/sstate/PATH;downloadfilename=PATH"`,
		`INHERIT += "own-mirrors"`,
		`SOURCE_MIRROR_URL ?= "https://example.com/cache/acme/firmware/downloads"`,
		`BB_SIGNATURE_HANDLER = "OEEquivHash"`,
		`BB_HASHSERVE = "wss://example.com/cache/acme/firmware/hashserv"`,
	} {
		if !strings.Contains(out.LocalConf, want) {
			t.Errorf("local_conf missing line:\n  want %q\n  in\n%s", want, out.LocalConf)
		}
	}

	// No secret ever lands in local.conf -- bitbake takes it from netrc.
	if strings.Contains(out.LocalConf, snippetToken) {
		t.Error("local_conf must not contain the token; the credential belongs in netrc")
	}

	// Both push commands, with the token via BAKERY_API_KEY and the CLI's positional
	// org/project/dir grammar -- NOT a --project flag, which the CLI does not have.
	if len(out.PushCommands) != 2 {
		t.Fatalf("push_commands = %d, want 2 (%v)", len(out.PushCommands), out.PushCommands)
	}

	if !strings.Contains(out.PushCommands[0], "bakery sstate push acme firmware ./build/sstate-cache") {
		t.Errorf("sstate push command = %q", out.PushCommands[0])
	}

	if !strings.Contains(out.PushCommands[1], "bakery downloads push acme firmware ./build/downloads") {
		t.Errorf("downloads push command = %q", out.PushCommands[1])
	}

	if out.APIKey == nil || out.APIKey.Token != snippetToken {
		t.Fatalf("api_key = %+v, want the plaintext token", out.APIKey)
	}

	if out.Preview {
		t.Error("a minting request must not report preview = true")
	}
}

// TestSnippetYoctoHashservBlockOnlyWhenConfigured is the B1 headline test, and the
// regression guard for the bug that shipped: the generator touched cache_backends
// zero times, so a Yocto snippet configured hash equivalence whether or not the
// project had a hashserv backend.
//
// Emitting BB_HASHSERVE at a path that 404s is not a loud failure. bb.siggen catches
// the connection error, warns, and sets unihash = taskhash -- the build completes,
// green, and every sstate object misses because its filename embeds the unihash. So
// this asserts all three halves: present when configured, ABSENT (with a loud
// warning) when not, and the two netrc lines kept as DISTINCT lines with distinct
// keying, which is the other way this block silently unauthenticates a build.
func TestSnippetYoctoHashservBlockOnlyWhenConfigured(t *testing.T) {
	hashservURL := "wss://bakery.corp/cache/acme/firmware/hashserv"

	t.Run("configured: the full five-block form and both netrc lines", func(t *testing.T) {
		a := snippetAPI(t, snippetStore(t,
			repository.BackendKindSstate, repository.BackendKindDownloads, repository.BackendKindHashserv,
		), &fakeMinter{token: snippetToken})

		out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", map[string]string{
			"X-Forwarded-Proto": "https", "X-Forwarded-Host": "bakery.corp",
		}), http.StatusCreated)

		for _, want := range []string{
			`BB_SIGNATURE_HANDLER = "OEEquivHash"`,
			fmt.Sprintf(`BB_HASHSERVE = "%s"`, hashservURL),
			`BB_HASHSERVE = "auto"`, // named in the do-NOT-set comment
			"BB_HASHSERVE_UPSTREAM",
		} {
			if !strings.Contains(out.LocalConf, want) {
				t.Errorf("local_conf missing %q:\n%s", want, out.LocalConf)
			}
		}

		// THE TWO NETRC LINES ARE DISTINCT LINES WITH DISTINCT KEYS. oe-core matches
		// BB_HASHSERVE by exact FULL-URL string; `machine bakery.corp` does not match
		// it and is silently ignored, which is the single most common way to get a
		// silently-unauthenticated build. Collapsing the two keying schemes back into
		// one parameterised helper is exactly what this assertion forbids.
		lines := netrcLines(out.Netrc)
		if len(lines) != 2 {
			t.Fatalf("netrc = %d lines, want 2 (hostname-keyed + full-URL-keyed):\n%s", len(lines), out.Netrc)
		}

		wantHost := "machine bakery.corp login " + snippetToken + " password " + snippetToken
		wantHash := "machine " + hashservURL + " login " + snippetToken + " password " + snippetToken

		if lines[0] != wantHost {
			t.Errorf("netrc line 1 = %q, want %q", lines[0], wantHost)
		}

		if lines[1] != wantHash {
			t.Errorf("netrc line 2 = %q, want %q", lines[1], wantHash)
		}

		if lines[0] == lines[1] {
			t.Error("the two netrc lines must not be identical: one is hostname-keyed, one is full-URL-keyed")
		}

		// Nothing was omitted, so nothing is warned about.
		if len(out.Warnings) != 0 {
			t.Errorf("a fully-configured yocto project must warn about nothing, got %v", out.Warnings)
		}
	})

	t.Run("not configured: no BB_HASHSERVE, one loud warning, sstate block intact", func(t *testing.T) {
		a := snippetAPI(t, snippetStore(t,
			repository.BackendKindSstate, repository.BackendKindDownloads,
		), &fakeMinter{token: snippetToken})

		out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", map[string]string{
			"X-Forwarded-Proto": "https", "X-Forwarded-Host": "bakery.corp",
		}), http.StatusCreated)

		for _, forbidden := range []string{"BB_HASHSERVE", "BB_SIGNATURE_HANDLER", "wss://", "hashserv"} {
			if strings.Contains(out.LocalConf, forbidden) {
				t.Errorf("local_conf must not mention %q when there is no hashserv backend:\n%s",
					forbidden, out.LocalConf)
			}
		}

		// The sstate half is still emitted: half a config plus an explanation beats
		// nothing, and beats a config that points at a mount that does not exist.
		if !strings.Contains(out.LocalConf, "SSTATE_MIRRORS") {
			t.Errorf("the sstate block must still be emitted:\n%s", out.LocalConf)
		}

		// Exactly one netrc line, the hostname-keyed one.
		if lines := netrcLines(out.Netrc); len(lines) != 1 || !strings.HasPrefix(lines[0], "machine bakery.corp ") {
			t.Errorf("netrc = %q, want only the hostname-keyed line", out.Netrc)
		}

		joined := strings.Join(out.Warnings, " ")
		for _, want := range []string{"BB_HASHSERVE", "hashserv backend", "unihash = taskhash"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the omission warning must name %q:\n%v", want, out.Warnings)
			}
		}
	})

	t.Run("configured but disabled is the same as absent", func(t *testing.T) {
		store := snippetStore(t, repository.BackendKindSstate)
		store.backends = append(store.backends, repository.CacheBackend{
			ID: 99, ProjectID: mustUUID(t, projFirmwareID),
			Kind: repository.BackendKindHashserv, Enabled: false,
		})

		a := snippetAPI(t, store, &fakeMinter{token: snippetToken})
		out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", nil), http.StatusCreated)

		if strings.Contains(out.LocalConf, "BB_HASHSERVE") {
			t.Errorf("a DISABLED hashserv backend serves nothing; BB_HASHSERVE must be omitted:\n%s", out.LocalConf)
		}

		if !strings.Contains(strings.Join(out.Warnings, " "), "disabled") {
			t.Errorf("the warning must say the backend is disabled, not that it is absent: %v", out.Warnings)
		}
	})
}

// netrcLines splits a netrc body into non-empty trimmed lines.
func netrcLines(netrc string) []string {
	var out []string

	for _, line := range strings.Split(netrc, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}

	return out
}

// TestSnippetYoctoWithNoBackendsEmitsNothing: a project with no cache backends at
// all gets an empty local.conf, an empty netrc, no push commands and three
// warnings -- never a config block naming a mount this project does not serve.
func TestSnippetYoctoWithNoBackendsEmitsNothing(t *testing.T) {
	a := snippetAPI(t, snippetStore(t), &fakeMinter{token: snippetToken})

	out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", nil), http.StatusCreated)

	if out.LocalConf != "" {
		t.Errorf("local_conf = %q, want empty -- not a file consisting of one newline", out.LocalConf)
	}

	if out.Netrc != "" {
		t.Errorf("netrc = %q, want empty", out.Netrc)
	}

	if len(out.PushCommands) != 0 {
		t.Errorf("push_commands = %v, want none", out.PushCommands)
	}

	if len(out.Warnings) != 3 {
		t.Errorf("warnings = %d, want one each for sstate, downloads and hashserv: %v",
			len(out.Warnings), out.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Origin resolution
// ---------------------------------------------------------------------------

// TestSnippetHonorsForwardedHeaders proves the snippet points at the client-facing
// origin, not the private hop a TLS-terminating proxy forwards over.
func TestSnippetHonorsForwardedHeaders(t *testing.T) {
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

	out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "bakery.corp",
	}), http.StatusCreated)

	if out.BaseURL != "https://bakery.corp/cache/acme/firmware" {
		t.Fatalf("base_url = %q, want https://bakery.corp/...", out.BaseURL)
	}
}

// TestExternalURLBeatsForwardedHeaders: --external-url is the FIRST term of the
// precedence, unconditionally. An operator who has stated the public origin has
// stated it, and a header from the request cannot then move a live credential to a
// host of the sender's choosing.
func TestExternalURLBeatsForwardedHeaders(t *testing.T) {
	tests := []struct {
		name        string
		externalURL string
		hdr         map[string]string
		wantBase    string
		wantHost    string
	}{
		{
			name:        "config wins over both forwarded headers",
			externalURL: "https://bakery.example.com",
			hdr:         map[string]string{"X-Forwarded-Proto": "http", "X-Forwarded-Host": "attacker.test"},
			wantBase:    "https://bakery.example.com/cache/acme/firmware",
			wantHost:    "bakery.example.com",
		},
		{
			name:        "config carries its port through",
			externalURL: "https://bakery.example.com:8443/",
			hdr:         nil,
			wantBase:    "https://bakery.example.com:8443/cache/acme/firmware",
			wantHost:    "bakery.example.com",
		},
		{
			name:        "config may legitimately be http, e.g. an internal deployment",
			externalURL: "http://bakery.internal",
			hdr:         map[string]string{"X-Forwarded-Proto": "https"},
			wantBase:    "http://bakery.internal/cache/acme/firmware",
			wantHost:    "bakery.internal",
		},
		{
			name:        "a malformed value is ignored, not fatal: fall back to the headers",
			externalURL: "bakery.example.com",
			hdr:         map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "bakery.corp"},
			wantBase:    "https://bakery.corp/cache/acme/firmware",
			wantHost:    "bakery.corp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})
			a.externalURL = tt.externalURL

			out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", tt.hdr),
				http.StatusCreated)

			if out.BaseURL != tt.wantBase {
				t.Errorf("base_url = %q, want %q", out.BaseURL, tt.wantBase)
			}

			if out.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", out.Host, tt.wantHost)
			}
		})
	}
}

// TestSnippetSchemeFailsClosed: the response carries a live bearer credential, so
// an undeterminable scheme resolves to https. A wrong https is an unreachable
// endpoint -- a loud, debuggable client error. A wrong http is a credential
// disclosure that nothing logs. Loopback is the one case where http is provably
// right, and it must stay right, because that is `bakery serve` on a laptop.
func TestSnippetSchemeFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		fwdProto   string
		wantScheme string
	}{
		{name: "a public host with no TLS evidence assumes https", host: "bakery.corp", wantScheme: "https"},
		{name: "an unresolvable name still assumes https", host: "internal-lb-7", wantScheme: "https"},
		{name: "localhost is provably local", host: "localhost:8080", wantScheme: "http"},
		{name: "127.0.0.1 is provably local", host: "127.0.0.1:8080", wantScheme: "http"},
		{name: "::1 is provably local", host: "[::1]:8080", wantScheme: "http"},
		{
			name: "a forwarded https proto is honoured on a public host",
			host: "bakery.corp", fwdProto: "https", wantScheme: "https",
		},
		{
			name: "a forwarded http proto is honoured on a provably loopback host",
			host: "127.0.0.1:8080", fwdProto: "http", wantScheme: "http",
		},
		// R8#6: a spoofed X-Forwarded-Proto: http on a NON-loopback host must NOT
		// downgrade the scheme -- this response carries a live bearer credential, and
		// the header is exactly as spoofable by an attacker as by a legitimate proxy.
		// It fails closed to https, the same rule the no-header default arm already
		// enforces for this host.
		{
			name: "a forwarded http proto on a public host is refused, not honoured",
			host: "bakery.corp", fwdProto: "http", wantScheme: "https",
		},
		// The header must be CONSTRAINED to exactly http|https, never taken verbatim:
		// the old code assigned `scheme = fwd` unconditionally, which is scheme
		// injection (any string the caller sends becomes the literal URL prefix on a
		// response carrying a credential). Garbage falls through to the same
		// fail-closed ladder as no header at all.
		{
			name: "a forwarded proto outside http|https is ignored, not used verbatim",
			host: "bakery.corp", fwdProto: "ftp", wantScheme: "https",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

			hdr := map[string]string{"X-Forwarded-Host": tt.host}
			if tt.fwdProto != "" {
				hdr["X-Forwarded-Proto"] = tt.fwdProto
			}

			out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], "", hdr),
				http.StatusCreated)

			if !strings.HasPrefix(out.BaseURL, tt.wantScheme+"://") {
				t.Errorf("base_url = %q, want scheme %s", out.BaseURL, tt.wantScheme)
			}

			// The hashserv URL rides the same decision: https => wss, http => ws.
			wantWS := "wss://"
			if tt.wantScheme == "http" {
				wantWS = "ws://"
			}

			if !strings.Contains(out.LocalConf, `BB_HASHSERVE = "`+wantWS) {
				t.Errorf("BB_HASHSERVE must use %s for a %s origin:\n%s", wantWS, tt.wantScheme, out.LocalConf)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The gRPC endpoint
// ---------------------------------------------------------------------------

// TestGRPCEndpointUsesGRPCAddrPortNotHTTPPort is the direct regression test for
// the M4 finding, and it FAILS against the code that shipped there.
//
// REAPI is served on its OWN listener (--grpc-addr, default :9092) and never on the
// public port -- a shared h2c port is forbidden outright. The old derivation took
// the authority from the HTTP request, so it emitted the HTTP port (or the scheme
// default when the Host carried none), which means there was NO configuration,
// including a plain `bakery serve`, under which it named 9092. moon's response to
// an unreachable cache is to disable it: 0% hits, green builds, DEBUG log only.
func TestGRPCEndpointUsesGRPCAddrPortNotHTTPPort(t *testing.T) {
	tests := []struct {
		name         string
		grpcAddr     string
		fwdHost      string
		fwdProto     string
		wantEndpoint string
	}{
		{
			name:     "the HTTP port is not reused",
			grpcAddr: "127.0.0.1:9092", fwdHost: "bakery.corp:8080", fwdProto: "https",
			wantEndpoint: "grpcs://bakery.corp:9092",
		},
		{
			name:     "a host with no port does not get the scheme default",
			grpcAddr: "127.0.0.1:9092", fwdHost: "bakery.corp", fwdProto: "https",
			wantEndpoint: "grpcs://bakery.corp:9092",
		},
		{
			// R8#6: a forwarded "http" is only honoured on a provably loopback host
			// (the same fail-closed rule TestSnippetSchemeFailsClosed pins) -- a public
			// host stays https regardless of what X-Forwarded-Proto claims. This row
			// uses a loopback host so it still exercises the grpc-vs-grpcs mapping
			// itself, which is what it is named for.
			name:     "http maps to grpc, not grpcs",
			grpcAddr: "0.0.0.0:9092", fwdHost: "127.0.0.1", fwdProto: "http",
			wantEndpoint: "grpc://127.0.0.1:9092",
		},
		{
			name:     "a non-default grpc port comes through verbatim",
			grpcAddr: "0.0.0.0:15001", fwdHost: "bakery.corp:443", fwdProto: "https",
			wantEndpoint: "grpcs://bakery.corp:15001",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})
			a.grpcAddr = tt.grpcAddr

			hdr := map[string]string{"X-Forwarded-Host": tt.fwdHost, "X-Forwarded-Proto": tt.fwdProto}

			bazelOut := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"],
				`{"tool":"bazel"}`, hdr), http.StatusCreated)

			if want := "build --remote_cache=" + tt.wantEndpoint; !strings.Contains(snippetFileContent(t, bazelOut), want) {
				t.Errorf(".bazelrc missing %q:\n%s", want, snippetFileContent(t, bazelOut))
			}

			moonOut := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"],
				`{"tool":"moon"}`, hdr), http.StatusCreated)

			if want := "host: '" + tt.wantEndpoint + "'"; !strings.Contains(snippetFileContent(t, moonOut), want) {
				t.Errorf("workspace.yml missing %q:\n%s", want, snippetFileContent(t, moonOut))
			}
		})
	}
}

// TestGRPCExternalEndpointOverride: when set, it is used VERBATIM. It is the only
// input that can know what an ingress did with the listener -- neither the request
// nor --grpc-addr can, so neither may override it or "correct" it.
func TestGRPCExternalEndpointOverride(t *testing.T) {
	const override = "grpcs://reapi.bakery.corp:443"

	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})
	a.grpcExternalEndpoint = override
	// Both of the inputs the derivation would otherwise use say something else.
	a.grpcAddr = "127.0.0.1:9092"

	hdr := map[string]string{"X-Forwarded-Host": "console.bakery.corp", "X-Forwarded-Proto": "https"}

	out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], `{"tool":"bazel"}`, hdr),
		http.StatusCreated)

	rc := snippetFileContent(t, out)
	if !strings.Contains(rc, "build --remote_cache="+override) {
		t.Errorf(".bazelrc must use the override verbatim:\n%s", rc)
	}

	if strings.Contains(rc, "9092") || strings.Contains(rc, "console.bakery.corp") {
		t.Errorf(".bazelrc must not mix the override with a derived host or port:\n%s", rc)
	}

	// The override also works when the listener is disabled entirely -- that is the
	// deployment where the REAPI listener lives in another process or another pod.
	a.grpcAddr = ""

	out = decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], `{"tool":"moon"}`, hdr),
		http.StatusCreated)

	if !strings.Contains(snippetFileContent(t, out), override) {
		t.Errorf("an override must be honoured with no --grpc-addr at all:\n%s", snippetFileContent(t, out))
	}
}

// TestSnippetRefusesBazelMoonWhenGRPCDisabled: --grpc-addr empty means the REAPI
// listener is switched OFF. A bazel or moon snippet then has no endpoint to name,
// and the honest answer is a 409 -- not a snippet that connects nowhere, and not
// not_implemented (REAPI is implemented; this deployment turned it off). Every
// other tool is unaffected: none of them speaks gRPC.
func TestSnippetRefusesBazelMoonWhenGRPCDisabled(t *testing.T) {
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})
	a.grpcAddr = ""

	for _, tool := range []string{SnippetToolBazel, SnippetToolMoon} {
		t.Run(tool+" is refused", func(t *testing.T) {
			w := snippetPost(t, a, principals(t)["proj_write"], fmt.Sprintf(`{"tool":%q}`, tool), nil)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", w.Code, w.Body.String())
			}

			var envelope ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("the 409 is not the error envelope: %q", w.Body.String())
			}

			if envelope.Error.Code != CodeConflict {
				t.Errorf("code = %q, want %q", envelope.Error.Code, CodeConflict)
			}

			if !strings.Contains(envelope.Error.Message, "--grpc-addr") {
				t.Errorf("the message must name the fix: %q", envelope.Error.Message)
			}
		})
	}

	for _, tool := range []string{
		SnippetToolYocto, SnippetToolCcache, SnippetToolSccache,
		SnippetToolContainerd, SnippetToolBuildkit, SnippetToolPodman, SnippetToolDocker,
	} {
		t.Run(tool+" is unaffected", func(t *testing.T) {
			w := snippetPost(t, a, principals(t)["proj_write"], fmt.Sprintf(`{"tool":%q}`, tool), nil)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 -- %s does not speak gRPC (body %s)",
					w.Code, tool, w.Body.String())
			}
		})
	}
}

// TestSnippetRefusalMintsNothing: the 409 above is resolved BEFORE the mint, so a
// refused request leaves no credential behind. The alternative -- mint, then
// discover the misconfiguration -- litters a project with live write-scoped keys
// for requests that returned an error.
func TestSnippetRefusalMintsNothing(t *testing.T) {
	minter := &fakeMinter{token: snippetToken}
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), minter)
	a.grpcAddr = ""

	if w := snippetPost(t, a, principals(t)["proj_write"], `{"tool":"bazel"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}

	if minter.got.Name != "" {
		t.Errorf("a refused request must not reach CreateAPIKey; it minted %q", minter.got.Name)
	}
}

// ---------------------------------------------------------------------------
// Preview
// ---------------------------------------------------------------------------

// TestSnippetPreviewMintsNothingUnit is the fake-minter half of the preview
// contract: 200 (nothing was created), no api_key on the wire, the placeholder
// wherever the token would be, and the minter never called. The DB-backed half --
// zero api_keys ROWS -- is TestSnippetPreviewMintsNothing below.
func TestSnippetPreviewMintsNothingUnit(t *testing.T) {
	minter := &fakeMinter{token: snippetToken}
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), minter)

	for _, tool := range snippetTools {
		t.Run(tool, func(t *testing.T) {
			body := fmt.Sprintf(`{"tool":%q,"preview":true}`, tool)

			out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"], body, nil), http.StatusOK)

			if !out.Preview {
				t.Error("preview = false on a preview response")
			}

			if out.APIKey != nil {
				t.Errorf("api_key = %+v, want absent -- a preview creates no credential", out.APIKey)
			}

			// The whole body carries no token, anywhere.
			raw := mustMarshal(t, out)
			if strings.Contains(raw, snippetToken) {
				t.Errorf("a preview response must not contain a token:\n%s", raw)
			}

			if minter.got.Name != "" {
				t.Fatalf("preview reached CreateAPIKey and minted %q", minter.got.Name)
			}
		})
	}
}

// TestSnippetPreviewCarriesThePlaceholder: the preview is a real, complete config
// with a placeholder where the credential goes -- and the placeholder is
// deliberately not paste-able, so a preview copied by mistake fails at the client's
// own config parser instead of authenticating as nobody and looking like a cache
// that merely misses.
func TestSnippetPreviewCarriesThePlaceholder(t *testing.T) {
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

	out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"],
		`{"tool":"yocto","preview":true}`, nil), http.StatusOK)

	if !strings.Contains(out.Netrc, snippetTokenPlaceholder) {
		t.Errorf("the preview netrc must carry the placeholder:\n%s", out.Netrc)
	}

	// The config itself is the real thing -- this is a preview of the config, not a
	// preview of the screen.
	if !strings.Contains(out.LocalConf, "SSTATE_MIRRORS") || !strings.Contains(out.LocalConf, "BB_HASHSERVE") {
		t.Errorf("a preview must render the full config:\n%s", out.LocalConf)
	}

	if strings.HasPrefix(snippetTokenPlaceholder, "bkry_") {
		t.Error("the placeholder must not look like a real credential")
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(raw)
}

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

// TestSnippetDefaultScopeCapsAtTheCallersCeiling: the default used to be write
// unconditionally, so a project READER opening the snippets screen got a 403
// scope_exceeds_role as the very first thing the highest-value screen did -- for
// asking for the default. The default now resolves to the caller's own ceiling.
func TestSnippetDefaultScopeCapsAtTheCallersCeiling(t *testing.T) {
	tests := []struct {
		principal string
		want      auth.Scope
	}{
		{principal: "proj_read", want: auth.ScopeRead},
		{principal: "proj_write", want: auth.ScopeWrite},
		{principal: "proj_admin", want: auth.ScopeWrite},
		// An org member with no project role cannot write the project, so read.
		{principal: "org_member", want: auth.ScopeRead},
	}

	for _, tt := range tests {
		t.Run(tt.principal, func(t *testing.T) {
			minter := &fakeMinter{token: snippetToken}
			a := snippetAPI(t, snippetStore(t, allSnippetBackends...), minter)

			if w := snippetPost(t, a, principals(t)[tt.principal], "", nil); w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
			}

			if minter.got.Scope != tt.want {
				t.Errorf("minted scope = %q, want %q", minter.got.Scope, tt.want)
			}
		})
	}
}

// TestSnippetReaderCannotRequestWriteScope: an EXPLICIT scope is still capped in
// auth.CreateAPIKey, so a reader asking for a write snippet is refused with a 403,
// never handed a key beyond their role and never quietly downgraded. Only the
// DEFAULT moved.
func TestSnippetReaderCannotRequestWriteScope(t *testing.T) {
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{err: auth.ErrScopeExceedsRole})

	w := snippetPost(t, a, principals(t)["proj_read"], `{"scope":"write"}`, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
}

// TestSnippetReaderGetsReadScope: a reader may still generate a read-scoped snippet
// for themselves -- ProjectRead is the route floor precisely so this works.
func TestSnippetReaderGetsReadScope(t *testing.T) {
	minter := &fakeMinter{token: snippetToken}
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), minter)

	if w := snippetPost(t, a, principals(t)["proj_read"], `{"scope":"read"}`, nil); w.Code != http.StatusCreated {
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
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

	if w := snippetPost(t, a, principals(t)["proj_write"], `{"tool":"buck2"}`, nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Backend gating for the single-backend tools
// ---------------------------------------------------------------------------

// TestSnippetSingleBackendToolsAreGated: ccache, sccache, bazel and moon all ride
// the BAZEL backend (/ac, /cas and the sccache WebDAV mount are all kind=bazel);
// containerd, buildkit, podman and docker all ride the OCI backend. With that
// backend absent there is nothing to configure, so the response is a warning and NO
// config -- not a file naming a mount that 404s, which every one of these clients
// treats as an ordinary miss and reports to nobody.
func TestSnippetSingleBackendToolsAreGated(t *testing.T) {
	tests := []struct {
		tool string
		kind repository.BackendKind
	}{
		{tool: SnippetToolCcache, kind: repository.BackendKindBazel},
		{tool: SnippetToolSccache, kind: repository.BackendKindBazel},
		{tool: SnippetToolBazel, kind: repository.BackendKindBazel},
		{tool: SnippetToolMoon, kind: repository.BackendKindBazel},
		{tool: SnippetToolContainerd, kind: repository.BackendKindOci},
		{tool: SnippetToolBuildkit, kind: repository.BackendKindOci},
		{tool: SnippetToolPodman, kind: repository.BackendKindOci},
		{tool: SnippetToolDocker, kind: repository.BackendKindOci},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			// Every backend EXCEPT the one this tool needs.
			var others []repository.BackendKind

			for _, k := range allSnippetBackends {
				if k != tt.kind {
					others = append(others, k)
				}
			}

			a := snippetAPI(t, snippetStore(t, others...), &fakeMinter{token: snippetToken})

			out := decodeSnippet(t, snippetPost(t, a, principals(t)["proj_write"],
				fmt.Sprintf(`{"tool":%q}`, tt.tool), nil), http.StatusCreated)

			if len(out.Files) != 0 || len(out.Env) != 0 {
				t.Errorf("no config may be emitted without a %s backend: files %+v env %+v",
					tt.kind, out.Files, out.Env)
			}

			joined := strings.Join(out.Warnings, " ")
			if !strings.Contains(joined, string(tt.kind)) || !strings.Contains(joined, "not configured") {
				t.Errorf("the warning must name the missing %s backend: %v", tt.kind, out.Warnings)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Per-tool traps (unchanged contracts, re-asserted against the new plumbing)
// ---------------------------------------------------------------------------

// TestSnippetMoonTokenIsAName is moon's two silent traps: the token is the NAME of an
// env var, so it must be ABSENT from workspace.yml and present only in the export; and
// the host needs a scheme AND a port. It also pins the compression note: we advertise
// IDENTITY only, so 'zstd' would earn a fallback warning against a cache that cannot
// serve it, and the file says so.
func TestSnippetMoonTokenIsAName(t *testing.T) {
	out := snippetFor(t, SnippetToolMoon, "write", "https", "bakery.corp")

	yaml := snippetFileContent(t, out)

	if !strings.Contains(yaml, "token: 'BAKERY_TOKEN'") {
		t.Errorf("workspace.yml must carry the env-var NAME `token: 'BAKERY_TOKEN'`:\n%s", yaml)
	}

	if strings.Contains(yaml, snippetToken) {
		t.Errorf("workspace.yml must NOT contain the token -- that silently disables moon's cache:\n%s", yaml)
	}

	if got := snippetEnv(t, out, "BAKERY_TOKEN"); got != snippetToken {
		t.Errorf("BAKERY_TOKEN = %q, want the token", got)
	}

	if !strings.Contains(yaml, "host: 'grpcs://bakery.corp:9092'") {
		t.Errorf("workspace.yml host must be grpcs://bakery.corp:9092 (scheme + the GRPC port):\n%s", yaml)
	}

	if !strings.Contains(yaml, "compression: 'none'") || !strings.Contains(yaml, "IDENTITY only") {
		t.Errorf("workspace.yml must set compression 'none' and say why:\n%s", yaml)
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
	// mandatory. There is no id:secret split to make; a Bakery key is one opaque token.
	if !strings.Contains(conf, "http://"+snippetToken+":@") {
		t.Errorf("ccache userinfo must be http://<token>:@host (colon mandatory):\n%s", conf)
	}

	if strings.Contains(conf, "https://") {
		t.Errorf("ccache cannot speak https; the config must be http:// only:\n%s", conf)
	}

	if !strings.Contains(conf, "@connect-timeout=1000") {
		t.Errorf("ccache.conf must set @connect-timeout=1000 (default 100ms is too tight):\n%s", conf)
	}

	if strings.Contains(conf, "read-only=true") {
		t.Errorf("a write-scoped ccache snippet must not be read-only:\n%s", conf)
	}
}

// TestSnippetCcacheReadOnly: a read-scoped key emits read-only=true, so ccache never
// issues the PUT whose 403 would latch the whole backend (reads included) off.
func TestSnippetCcacheReadOnly(t *testing.T) {
	out := snippetFor(t, SnippetToolCcache, "read", "", "")

	if conf := snippetFileContent(t, out); !strings.Contains(conf, "@read-only=true") {
		t.Errorf("a read-scoped ccache snippet must set @read-only=true:\n%s", conf)
	}
}

// TestSnippetSccacheKeyPrefix: SCCACHE_WEBDAV_KEY_PREFIX is REQUIRED (without it the
// keys land at a prefix Bakery does not serve), the endpoint is https, and the token
// rides SCCACHE_WEBDAV_TOKEN as a Bearer credential. The warning carries the gotcha
// text -- an `export` line has nowhere to put a comment -- and it must say the
// credential is ONE opaque token, never a key-id/key-secret pair.
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

	joined := strings.Join(out.Warnings, " ")
	for _, want := range []string{"one opaque bkry_ token", "not a key-id and key-secret pair", "read-only"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the sccache warning must carry %q:\n%v", want, out.Warnings)
		}
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
		"build --remote_cache=grpcs://bakery.corp:9092",
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

	mount := "https://bakery.corp/cache/acme/firmware/docker"
	if !strings.Contains(toml, fmt.Sprintf(`[host.%q]`, mount)) {
		t.Errorf("hosts.toml missing the [host] block for %s:\n%s", mount, toml)
	}

	if strings.Contains(toml, mount+"/v2") {
		t.Errorf("hosts.toml must NOT append /v2 itself -- containerd does that:\n%s", toml)
	}

	if !strings.Contains(toml, "Bearer challenge") {
		t.Errorf("hosts.toml must document the default Bearer-challenge auth path:\n%s", toml)
	}

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
// half, so the doc comment must tell the operator to set BOTH fields with the SAME
// opaque token -- there is no id:secret split to make.
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

// TestSnippetNeverEmitsAnIDSecretPair: a Bakery cache credential is ONE opaque
// bkry_ token. `key_id:key_secret` is a credential that cannot exist, and it was the
// single most repeated drift in the hand-authored console copy this generator
// replaces. No emitted config, for any tool, may contain a secret half.
func TestSnippetNeverEmitsAnIDSecretPair(t *testing.T) {
	forbidden := []string{"key_id", "key_secret", "bks_", "bk_", "access_key", "secret_key"}

	for _, tool := range snippetTools {
		t.Run(tool, func(t *testing.T) {
			out := snippetFor(t, tool, "write", "https", "bakery.corp")

			raw := mustMarshal(t, out)
			for _, bad := range forbidden {
				if strings.Contains(raw, bad) {
					t.Errorf("the %s snippet mentions %q; a Bakery key has no secret half:\n%s", tool, bad, raw)
				}
			}
		})
	}
}

// TestSnippetRequiresProjectRead: an outsider gets nothing.
func TestSnippetRequiresProjectRead(t *testing.T) {
	a := snippetAPI(t, snippetStore(t, allSnippetBackends...), &fakeMinter{token: snippetToken})

	w := snippetPost(t, a, principals(t)["outsider"], "", nil)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 403/404 for an outsider (body %s)", w.Code, w.Body.String())
	}
}
