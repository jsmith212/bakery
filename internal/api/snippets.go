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
const SnippetToolYocto = "yocto"

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
	// the write path.
	PushCommands []string `json:"push_commands"`

	// APIKey is the freshly-minted key, INCLUDING the plaintext token exactly once.
	APIKey CreatedAPIKey `json:"api_key"`
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

	if tool != SnippetToolYocto {
		return errValidation("tool", `tool must be "yocto"`)
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
		LocalConf:    yoctoLocalConf(baseURL),
		Netrc:        netrcLine(hostOnly(host), key.Token),
		PushCommands: yoctoPushCommands(s.OrgSlug, s.ProjectSlug, key.Token),
		APIKey:       created,
	})

	return nil
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
