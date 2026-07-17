package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/jsmith212/bakery/internal/auth"
)

// The config-snippet generator: DESIGN.md calls it the highest-value screen, and M2
// is the first backend it can target. It emits, for a project, the EXACT verified
// Yocto local.conf lines (SSTATE_MIRRORS + own-mirrors/SOURCE_MIRROR_URL) with this
// server's host baked in and a freshly-minted key, so a user can paste-and-build
// rather than reverse-engineer the addressing and the credential mechanics.
//
// Every line here is transcribed from docs/design/protocols/client-config.md, which
// was written by reading bitbake's client source. The gotchas it documents -- the
// downloadfilename=PATH suffix that makes SSTATE_MIRRORS rewrite work, own-mirrors as
// the premirror inherit, netrc keyed by HOSTNAME not URL for the HTTP Basic path --
// are the whole reason this endpoint exists. Do not "simplify" them.

// SnippetRequest asks for a config snippet. Both fields are optional: the M2 default
// is the Yocto tool at write scope, because the snippet documents both the read
// mirror AND `bakery sstate push`, and a write-scoped key reads as well as writes --
// one key serves the whole workflow.
type SnippetRequest struct {
	// Tool selects the client. M2 knows only "yocto" (sstate + downloads share one
	// local.conf). bazel/ccache/oci arrive with M4/M5.
	Tool string `json:"tool"`

	// Scope is the minted key's scope: read|write. Defaults to write so the same key
	// drives both the bitbake read path and the push. Capped at the caller's project
	// role inside auth.CreateAPIKey -- a reader asking for write gets a 403, not a key
	// beyond their authority.
	Scope string `json:"scope"`

	// KeyName names the minted key so it is recognisable in the project's key list
	// after the one-time token reveal. Defaults to a tool-derived name.
	KeyName string `json:"key_name"`
}

// SnippetTool is the set of tools the generator can target. It is a closed set so an
// unknown tool is a 422 at request time, not an empty snippet a user pastes and
// wonders why nothing is cached.
//
// yocto is the M2 default (sstate + downloads share one local.conf); moon, ccache,
// sccache and bazel arrive with M4. Every M4 client writes to the cache itself, so
// none of them carries a push -- PushCommands is empty for all four.
const (
	SnippetToolYocto   = "yocto"
	SnippetToolMoon    = "moon"
	SnippetToolCcache  = "ccache"
	SnippetToolSccache = "sccache"
	SnippetToolBazel   = "bazel"
)

// snippetTools is the closed set, in the order the 422 message lists them.
var snippetTools = []string{
	SnippetToolYocto, SnippetToolMoon, SnippetToolCcache, SnippetToolSccache, SnippetToolBazel,
}

func knownSnippetTool(tool string) bool {
	for _, t := range snippetTools {
		if t == tool {
			return true
		}
	}

	return false
}

// SnippetResponse is the generated snippet plus the key it embeds.
//
// # The response shape (recorded for the SPA wiring wave)
//
// The console renders LocalConf in a mono block with a copy button, Netrc in a second
// block, and PushCommands as a list; APIKey.Token is shown ONCE in a reveal modal and
// never again (the schema stores only its SHA-256). Host/BaseURL are surfaced so the
// UI can show "targeting bakery.corp" without re-parsing the config text.
type SnippetResponse struct {
	// Tool echoes the resolved tool.
	Tool string `json:"tool"`

	// Host is the bare hostname (no scheme, no port) this snippet targets -- the value
	// a ~/.netrc `machine` line is keyed on.
	Host string `json:"host"`

	// BaseURL is scheme://host[:port]/cache/{org}/{project}: the prefix every cache URL
	// in the snippet is built on.
	BaseURL string `json:"base_url"`

	// LocalConf is the verified conf/local.conf block. No secret is in it: bitbake
	// takes the credential from ~/.netrc or the environment, never from the URL.
	LocalConf string `json:"local_conf"`

	// Netrc is the ~/.netrc line carrying the freshly-minted token. THIS is where the
	// secret lives, and it is the only place in the response besides APIKey.Token.
	Netrc string `json:"netrc"`

	// PushCommands are the `bakery sstate push` / `bakery downloads push` invocations
	// that populate the mirror after a build -- bitbake has no upload path, so this is
	// the write path. YOCTO-ONLY: every M4 client writes to the cache itself, so this
	// is empty for moon/ccache/sccache/bazel.
	PushCommands []string `json:"push_commands"`

	// Files are the config FILES an M4 tool needs written to disk (moon's
	// .moon/workspace.yml, ccache's ccache.conf, bazel's .bazelrc). omitempty so the
	// yocto response -- which uses LocalConf instead -- is byte-identical to M2's.
	Files []SnippetFile `json:"files,omitempty"`

	// Env are the environment variables an M4 tool needs exported (moon's
	// BAKERY_TOKEN, sccache's SCCACHE_WEBDAV_* trio). THIS is where the secret lives
	// for the tools that carry it out-of-band; putting the token in the file where its
	// NAME belongs silently disables moon's cache. omitempty for the same reason.
	Env []SnippetEnvVar `json:"env,omitempty"`

	// APIKey is the freshly-minted key, INCLUDING the plaintext token exactly once.
	APIKey CreatedAPIKey `json:"api_key"`
}

// SnippetFile is a config file the UI renders in a mono block with a copy button and
// a "write this to <path>" caption. Language is a syntax hint for the highlighter.
type SnippetFile struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Content  string `json:"content"`
}

// SnippetEnvVar is a single `export NAME=value`. The UI renders these as a shell
// block; for moon and sccache the credential lives here, not in the file.
type SnippetEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// handleGenerateSnippet mints a project-scoped key and returns a ready-to-paste client
// config with it embedded. Project read is the floor -- a reader may generate a
// read-scoped snippet for themselves -- and the scope cap in auth.CreateAPIKey does the
// rest: a reader who asks for a write snippet is refused, not quietly downgraded.
func (a *API) handleGenerateSnippet(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	p, ok := principalFrom(ctx)
	if !ok {
		return errUnauthorized("authentication required")
	}

	s := scopeFrom(ctx)

	req, err := decodeSnippetRequest(r)
	if err != nil {
		return err
	}

	tool := req.Tool
	if tool == "" {
		tool = SnippetToolYocto
	}

	if !knownSnippetTool(tool) {
		return errValidation("tool",
			`tool must be one of "yocto", "moon", "ccache", "sccache", "bazel"`)
	}

	scopeStr := req.Scope
	if scopeStr == "" {
		scopeStr = string(auth.ScopeWrite)
	}

	keyScope, err := scopeOf(strings.TrimSpace(scopeStr))
	if err != nil {
		return err
	}

	// The key name is UNIQUE per (project, user) among live keys. A fixed default
	// would 409 the second time a user generates a snippet -- and regenerating is the
	// COMMON case, since the token was shown once and is likely lost. So the default
	// carries entropy: each snippet mints a distinct, greppable key.
	name := strings.TrimSpace(req.KeyName)
	if name == "" {
		name = fmt.Sprintf("%s snippet %s", tool, randSuffix())
	}

	// Mint the key EXACTLY as handleCreateKey does: for the caller, scoped to this
	// project, capped at their role. The token exists only in this response.
	key, row, err := a.keys.CreateAPIKey(ctx, p, auth.CreateKeyInput{
		OrgID: s.OrgID, ProjectID: s.ProjectID,
		Name: name, Scope: keyScope,
	})
	if err != nil {
		// A caller-supplied key_name that duplicates a live key trips the unique
		// index; the generic 23505 mapping ("that slug is already taken") is nonsense
		// here, so name the real conflict -- exactly as handleCreateBackend does.
		if isPGCode(err, pgUniqueViolation) {
			return errConflict(CodeConflict,
				fmt.Sprintf("you already have a key named %q in this project; pass a different key_name", name))
		}

		return fmt.Errorf("mint snippet key: %w", err)
	}

	a.log.InfoContext(ctx, "generated a config snippet",
		"project", s.ProjectSlug, "tool", tool,
		"prefix", row.TokenPrefix, "scope", string(row.Scope),
	)

	scheme, host := externalOrigin(r)
	baseURL := fmt.Sprintf("%s://%s/cache/%s/%s", scheme, host, s.OrgSlug, s.ProjectSlug)

	content := buildSnippet(tool, scheme, host, baseURL, s.OrgSlug, s.ProjectSlug, key.Token, keyScope)

	created := CreatedAPIKey{
		APIKey: APIKey{
			ID: uuidString(row.ID), Name: row.Name, ProjectID: uuidString(row.ProjectID),
			TokenPrefix: row.TokenPrefix, Scope: string(row.Scope),
			OwnerID: uuidString(row.UserID), OwnerEmail: p.Email(), OwnerName: p.DisplayName(),
			CreatedAt: row.CreatedAt.Time, ExpiresAt: timePtr(row.ExpiresAt),
			LastUsedAt: nil, RevokedAt: nil,
		},
		Token: key.Token,
	}

	writeJSON(w, http.StatusCreated, SnippetResponse{
		Tool:         tool,
		Host:         hostOnly(host),
		BaseURL:      baseURL,
		LocalConf:    content.localConf,
		Netrc:        content.netrc,
		PushCommands: content.pushCommands,
		Files:        content.files,
		Env:          content.env,
		APIKey:       created,
	})

	return nil
}

// snippetContent is the tool-specific half of a SnippetResponse. yocto populates the
// localConf/netrc/pushCommands trio; the M4 tools populate files/env. Exactly one
// shape is filled -- the two are mutually exclusive by tool.
type snippetContent struct {
	localConf    string
	netrc        string
	pushCommands []string
	files        []SnippetFile
	env          []SnippetEnvVar
}

// buildSnippet routes to the per-tool builder. tool is already validated against the
// closed set, so the default is unreachable; it returns an empty content rather than
// panicking. scheme/host are the resolved external origin (host MAY carry a port);
// baseURL is scheme://host/cache/{org}/{project}; scope gates ccache's read-only line.
func buildSnippet(tool, scheme, host, baseURL, org, project, token string, scope auth.Scope) snippetContent {
	switch tool {
	case SnippetToolMoon:
		return moonSnippet(scheme, host, org, project, token)
	case SnippetToolCcache:
		return ccacheSnippet(host, org, project, token, scope)
	case SnippetToolSccache:
		return sccacheSnippet(baseURL, token)
	case SnippetToolBazel:
		return bazelSnippet(scheme, host, org, project, token)
	default: // yocto
		return snippetContent{
			localConf:    yoctoLocalConf(baseURL),
			netrc:        netrcLine(hostOnly(host), token),
			pushCommands: yoctoPushCommands(org, project, token),
		}
	}
}

// grpcEndpoint builds a gRPC endpoint URL with an EXPLICIT port, for moon's
// remote.host and bazel's --remote_cache. tonic (moon) and Bazel both require the
// authority to carry a port, so hostOnly -- which strips it -- is the wrong helper
// here: when the request host has no port we supply the scheme default (443 for TLS,
// 80 otherwise). https maps to grpcs, http to grpc.
//
// ASSUMPTION, and a known limitation: this derives the gRPC authority from the HTTP
// request's external origin, i.e. it assumes gRPC is reachable at the SAME host:port
// as the console (a single TLS-terminating ingress that muxes both). But M4 serves
// REAPI on a DEDICATED listener (--grpc-addr, default 127.0.0.1:9092), so an operator
// who exposes gRPC on a distinct port or host gets a snippet pointing at the wrong
// place -- and moon silently disables its cache rather than erroring. This is
// tolerable ONLY because the SPA snippet screen is still mock-data (SPA wiring is
// out of M4 scope): nothing live calls this yet. The SPA-wiring milestone that turns
// this on MUST resolve the external gRPC endpoint (a PUBLIC_GRPC_ENDPOINT override,
// or a documented single-ingress contract) before shipping the snippet UI.
func grpcEndpoint(scheme, host string) string {
	grpcScheme, defaultPort := "grpc", "80"
	if scheme == "https" {
		grpcScheme, defaultPort = "grpcs", "443"
	}

	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, defaultPort)
	}

	return grpcScheme + "://" + host
}

// moonSnippet builds .moon/workspace.yml + the BAKERY_TOKEN export.
//
// TWO traps, both silent: (1) auth.token is the NAME of an env var, never the token
// -- moon reads the named variable and, if it is empty, disables the remote cache
// with no error; putting the token where the name goes is that same silent failure.
// So the token is ABSENT from the yaml and lives only in Env. (2) the host needs a
// scheme AND a port (grpc/grpcs is HTTP/2-only and tonic demands the port). We
// advertise IDENTITY only, so compression is 'none' -- 'zstd' would earn a fallback
// warning against a cache that cannot serve it.
func moonSnippet(scheme, host, org, project, token string) snippetContent {
	yaml := strings.Join([]string{
		"remote:",
		"  api: 'grpc'",
		fmt.Sprintf("  host: '%s'", grpcEndpoint(scheme, host)),
		"  auth:",
		"    token: 'BAKERY_TOKEN'   # the NAME of an env var, NOT the token itself",
		"  cache:",
		fmt.Sprintf("    instanceName: '%s/%s'   # the project selector for gRPC", org, project),
		"    compression: 'none'",
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: ".moon/workspace.yml", Language: "yaml", Content: yaml}},
		env:   []SnippetEnvVar{{Name: "BAKERY_TOKEN", Value: token}},
	}
}

// ccacheSnippet builds ~/.config/ccache/ccache.conf.
//
// Four traps: (1) @layout=bazel is MANDATORY -- the default subdirs layout writes to
// /<ab>/<cdef...>, a path Bakery does not route, so every GET 404s and the first PUT
// 404 latches the whole backend (reads included) off for that translation unit.
// (2) http:// ONLY -- ccache's built-in HTTP backend has no https scheme and refuses
// the URL before it opens a connection; TLS termination in front does not help.
// (3) the userinfo MUST carry a colon: ccache's URL ctor throws on a bare user with
// no password, so the token is the username and the password is empty
// (`bkry_...:`) -- and AuthenticateCache's password-then-username fallback is what
// makes that authenticate. (4) @connect-timeout=1000 -- the default is 100ms, too
// tight for a real network. For a read-scoped key we add read-only=true so ccache
// never issues the PUT that a 403 would latch the backend on.
func ccacheSnippet(host, org, project, token string, scope auth.Scope) snippetContent {
	line := fmt.Sprintf("remote_storage = http://%s:@%s/cache/%s/%s @layout=bazel @connect-timeout=1000",
		token, host, org, project)

	if scope == auth.ScopeRead {
		line += " @read-only=true"
	}

	content := "# ccache cannot speak https: this backend is plaintext HTTP only.\n" + line + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: "~/.config/ccache/ccache.conf", Language: "ini", Content: content}},
	}
}

// sccacheSnippet builds sccache's WebDAV environment.
//
// SCCACHE_WEBDAV_KEY_PREFIX is REQUIRED (sccache shards under it; without it the keys
// land at a prefix Bakery does not serve). SCCACHE_WEBDAV_TOKEN becomes an
// `Authorization: Bearer` header, which AuthenticateCache already accepts by
// delegating to the Bearer arm of Authenticate -- no new server code. The endpoint is
// https: sccache, unlike ccache, speaks TLS.
func sccacheSnippet(baseURL, token string) snippetContent {
	return snippetContent{
		env: []SnippetEnvVar{
			{Name: "SCCACHE_WEBDAV_ENDPOINT", Value: baseURL},
			{Name: "SCCACHE_WEBDAV_KEY_PREFIX", Value: "sccache"},
			{Name: "SCCACHE_WEBDAV_TOKEN", Value: token},
		},
	}
}

// bazelSnippet builds a .bazelrc block.
//
// The project rides in --remote_instance_name (gRPC cannot carry a URL path); the
// credential rides in a --remote_header as `authorization: Bearer <token>`. There is
// deliberately NO --remote_cache_compression: we advertise IDENTITY only, and Bazel
// HARD-FAILS the connection (not degrades) if compression is set and zstd is not
// advertised. host carries an explicit port for the same reason moon's does.
func bazelSnippet(scheme, host, org, project, token string) snippetContent {
	rc := strings.Join([]string{
		fmt.Sprintf("build --remote_cache=%s", grpcEndpoint(scheme, host)),
		fmt.Sprintf("build --remote_instance_name=%s/%s", org, project),
		fmt.Sprintf("build --remote_header=authorization=Bearer %s", token),
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: ".bazelrc", Language: "bazelrc", Content: rc}},
	}
}

// decodeSnippetRequest decodes an OPTIONAL body. An empty body is the common case --
// "generate me the default snippet" -- so io.EOF is not an error here; every other
// malformation still is.
func decodeSnippetRequest(r *http.Request) (SnippetRequest, error) {
	var req SnippetRequest

	if r.Body == nil {
		return req, nil
	}

	if err := decodeJSON(r, &req); err != nil {
		// An empty body decodes to EOF through decodeJSON's wrap; treat only that as
		// "no body, use defaults". Any other decode failure is a real 400.
		if errors.Is(err, io.EOF) {
			return SnippetRequest{}, nil
		}

		var ae *apiError
		if errors.As(err, &ae) && errors.Is(ae.cause, io.EOF) {
			return SnippetRequest{}, nil
		}

		return SnippetRequest{}, err
	}

	return req, nil
}

// yoctoLocalConf is the verified conf/local.conf block, transcribed from
// client-config.md. base is scheme://host/cache/{org}/{project}.
//
// SSTATE_MIRRORS rewrites file://.* to the mirror URL and appends the sstate PATH;
// the trailing `downloadfilename=PATH` is NOT optional -- without it bitbake fetches
// the URL but writes it to the wrong local name and the setscene object is a miss on
// the next build. own-mirrors + SOURCE_MIRROR_URL is the premirror (downloads) read.
func yoctoLocalConf(base string) string {
	return strings.Join([]string{
		"# --- sstate mirror (read) ---",
		fmt.Sprintf(`SSTATE_MIRRORS ?= "file://.* %s/sstate/PATH;downloadfilename=PATH"`, base),
		"",
		"# --- source premirror (read) ---",
		`INHERIT += "own-mirrors"`,
		fmt.Sprintf(`SOURCE_MIRROR_URL ?= "%s/downloads"`, base),
		`BB_GENERATE_MIRROR_TARBALLS = "1"`,
	}, "\n") + "\n"
}

// netrcLine is the ~/.netrc entry for the HTTP Basic sstate/downloads path, keyed by
// HOSTNAME (not the full URL -- that gotcha is the hashserv path, and this is not it).
//
// The token goes in BOTH fields because a Bakery key is one opaque `bkry_` string,
// not an id:secret pair: AuthenticateCache prefers the password field and falls back
// to the username, so putting the token in each means the credential authenticates
// whichever field the fetcher populates first.
func netrcLine(host, token string) string {
	return fmt.Sprintf("machine %s login %s password %s\n", host, token, token)
}

// yoctoPushCommands are the post-build uploads. The key is passed via BAKERY_API_KEY,
// which the push subcommand reads, rather than a flag that would land the secret in
// shell history's argv. The dirs are the conventional build/ paths.
func yoctoPushCommands(org, project, token string) []string {
	return []string{
		fmt.Sprintf("BAKERY_API_KEY=%s bakery sstate push %s %s ./build/sstate-cache", token, org, project),
		fmt.Sprintf("BAKERY_API_KEY=%s bakery downloads push %s %s ./build/downloads", token, org, project),
	}
}

// externalOrigin resolves the scheme and host this snippet should point at. Behind a
// TLS-terminating proxy the binary sees plain http on a private hop, so the forwarded
// headers win when present; otherwise it reports what it actually received.
func externalOrigin(r *http.Request) (scheme, host string) {
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}

	if fwd := firstForwarded(r.Header.Get("X-Forwarded-Proto")); fwd != "" {
		scheme = fwd
	}

	host = r.Host
	if fwd := firstForwarded(r.Header.Get("X-Forwarded-Host")); fwd != "" {
		host = fwd
	}

	return scheme, host
}

// firstForwarded takes the first value of a possibly comma-listed forwarded header
// (proxies append, so the client-facing value is first).
func firstForwarded(v string) string {
	if v == "" {
		return ""
	}

	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}

	return strings.TrimSpace(v)
}

// randSuffix is a short hex token that makes the default snippet key name unique, so
// regenerating a snippet never collides on the per-(project,user) name index. On the
// vanishingly unlikely rand failure it falls back to a fixed marker -- the unique
// index and the 23505 handler above are the backstop, not this.
func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "gen"
	}

	return hex.EncodeToString(b[:])
}

// hostOnly strips a port from a host[:port], for the netrc `machine` token and the
// Host field -- netrc matches on the bare hostname. A host with no port is returned
// unchanged.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}

	return host
}
