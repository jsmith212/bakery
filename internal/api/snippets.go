package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
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
// the premirror inherit, netrc keyed by HOSTNAME not URL for the HTTP Basic path and
// by the FULL URL for hashserv -- are the whole reason this endpoint exists. Do not
// "simplify" them.
//
// # Three rules this file exists to hold (B1, spec 2026-08-15)
//
//  1. IT ONLY EMITS CONFIG FOR BACKENDS THIS PROJECT ACTUALLY HAS. Every one of
//     these clients treats a 404 as an ordinary cache miss, so config for an
//     unmounted path produces a green build that caches nothing and reports nothing.
//     A missing backend is a Warnings entry, never a block.
//  2. IT FAILS CLOSED ON THE SCHEME, AND ON THE gRPC PORT. The response carries a
//     live credential, so the origin is https unless the host is provably loopback;
//     and the REAPI authority takes its port from --grpc-addr, never from the HTTP
//     request, because those are different listeners and always have been.
//  3. A PREVIEW MINTS NOTHING. The handler returns before CreateAPIKey, so the
//     screen can render nine tool tiles without minting nine credentials.

// SnippetRequest asks for a config snippet. Every field is optional: the default is
// the Yocto tool at the caller's own scope ceiling.
type SnippetRequest struct {
	// Tool selects the client. Defaults to yocto (sstate + downloads + hashserv all
	// share one local.conf).
	Tool string `json:"tool"`

	// Scope is the minted key's scope: read|write.
	//
	// EMPTY DEFAULTS TO THE CALLER'S OWN CEILING (B1, critique 6): write when the
	// caller may write this project, read otherwise. It used to hard-default to
	// write, which meant a project READER opening the snippets screen got a 403
	// scope_exceeds_role as the very first thing the highest-value screen did --
	// for asking for the default. An explicit scope is still capped inside
	// auth.CreateAPIKey, so asking for more than your role is a 403 and never a
	// quiet downgrade; only the DEFAULT moved.
	Scope string `json:"scope"`

	// KeyName names the minted key so it is recognisable in the project's key list
	// after the one-time token reveal. Defaults to a tool-derived name.
	KeyName string `json:"key_name"`

	// Preview asks for the config WITHOUT minting a credential (B1, critique 6).
	//
	// Every POST to this endpoint used to mint a live, write-scoped key, so a
	// screen that rendered nine tool tiles by fetching each one minted nine
	// credentials per page view, with no revocation story. Preview is the read
	// path that screen actually wants: the full config with a
	// snippetTokenPlaceholder where the token goes, and NOTHING written. The mint
	// happens only on an explicit "generate with a new key" gesture, which is a
	// request WITHOUT this field.
	Preview bool `json:"preview"`
}

// snippetTokenPlaceholder stands in for the credential in a preview.
//
// It is deliberately NOT a paste-able shape -- guillemets and spaces -- so a
// preview that is copied by mistake fails at the client's own config parser
// instead of authenticating as nobody and looking like a cache that just misses.
const snippetTokenPlaceholder = "«create an API key»"

// SnippetTool is the set of tools the generator can target. It is a closed set so an
// unknown tool is a 422 at request time, not an empty snippet a user pastes and
// wonders why nothing is cached.
//
// yocto is the M2 default (sstate + downloads share one local.conf); moon, ccache,
// sccache and bazel arrive with M4. Every M4 client writes to the cache itself, so
// none of them carries a push -- PushCommands is empty for all four.
//
// containerd, buildkit, podman and docker arrive with M5, and none of them carries a
// push either -- the OCI backend is pull-through only, there is no registry push API,
// so PushCommands stays empty for these too. All four target the SAME per-project OCI
// backend through one of two route families (see cache/oci.Backend.Register); the
// client picks the family, the snippet just emits the right shape for it.
const (
	SnippetToolYocto      = "yocto"
	SnippetToolMoon       = "moon"
	SnippetToolCcache     = "ccache"
	SnippetToolSccache    = "sccache"
	SnippetToolBazel      = "bazel"
	SnippetToolContainerd = "containerd"
	SnippetToolBuildkit   = "buildkit"
	SnippetToolPodman     = "podman"
	SnippetToolDocker     = "docker"
)

// snippetTools is the closed set, in the order the 422 message lists them.
var snippetTools = []string{
	SnippetToolYocto, SnippetToolMoon, SnippetToolCcache, SnippetToolSccache, SnippetToolBazel,
	SnippetToolContainerd, SnippetToolBuildkit, SnippetToolPodman, SnippetToolDocker,
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
//
// EVERY BLOCK IS OPTIONAL, and an absent block means the project has no backend to
// serve it. Warnings then names the omission and why. So the console renders
// Warnings FIRST and unconditionally -- a snippet with a missing block and an
// unrendered warning is worse than no snippet at all, because it looks complete.
//
// On a PREVIEW (Preview true) APIKey is absent and every credential is
// snippetTokenPlaceholder. The console previews on tile select and mints only on an
// explicit gesture.
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

	// Netrc is the ~/.netrc BLOCK carrying the token -- up to TWO lines, and they are
	// keyed differently on purpose: the sstate/downloads HTTP Basic path is keyed by
	// HOSTNAME, while BB_HASHSERVE is matched by oe-core as an exact FULL-URL string.
	// See netrcLine and hashservNetrcLine. THIS is where the secret lives, and it is
	// the only place in the response besides APIKey.Token.
	Netrc string `json:"netrc"`

	// PushCommands are the `bakery sstate push` / `bakery downloads push` invocations
	// that populate the mirror after a build -- bitbake has no upload path, so this is
	// the write path. YOCTO-ONLY: every M4 client writes to the cache itself, so this
	// is empty for moon/ccache/sccache/bazel. One entry per configured backend, so an
	// sstate-only project gets the sstate push and nothing else.
	PushCommands []string `json:"push_commands"`

	// Files are the config FILES an M4/M5 tool needs written to disk (moon's
	// .moon/workspace.yml, ccache's ccache.conf, bazel's .bazelrc). omitempty because
	// the yocto response uses LocalConf instead and has nothing to put here.
	Files []SnippetFile `json:"files,omitempty"`

	// Env are the environment variables an M4 tool needs exported (moon's
	// BAKERY_TOKEN, sccache's SCCACHE_WEBDAV_* trio). THIS is where the secret lives
	// for the tools that carry it out-of-band; putting the token in the file where its
	// NAME belongs silently disables moon's cache. omitempty for the same reason.
	Env []SnippetEnvVar `json:"env,omitempty"`

	// APIKey is the freshly-minted key, INCLUDING the plaintext token exactly once.
	//
	// A POINTER, and nil on a preview. The alternative -- a zero CreatedAPIKey --
	// puts an empty string in `api_key.token`, which a client renders as a
	// one-time reveal of nothing and a user pastes into a config that then
	// authenticates as nobody. Absent is the honest encoding of "no credential
	// was created", and it is not representable as a zero value.
	APIKey *CreatedAPIKey `json:"api_key,omitempty"`

	// Preview echoes SnippetRequest.Preview: true means every credential in this
	// response is snippetTokenPlaceholder and nothing was written. The client
	// branches on this to decide whether to show the one-time reveal.
	Preview bool `json:"preview"`

	// Warnings are operator-facing cautions the UI MUST render loudly, not bury in a
	// tooltip. It exists for the docker tool: Docker Engine forwards the user's real
	// Docker Hub credentials to whatever registry-mirrors names, unscoped, on every
	// pull -- and daemon.json is strict JSON with no comment syntax, so the warning
	// cannot live inside the emitted file the way ccache's or containerd's can.
	// omitempty because every other tool has nothing to say here.
	Warnings []string `json:"warnings,omitempty"`
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

// handleGenerateSnippet returns a ready-to-paste client config, optionally minting a
// project-scoped key to embed in it. Project read is the floor -- a reader may
// generate a read-scoped snippet for themselves -- and the scope cap in
// auth.CreateAPIKey does the rest: a reader who ASKS for a write snippet is refused,
// not quietly downgraded.
//
// THE ORDER OF THIS FUNCTION IS LOAD-BEARING. Everything that can fail -- the tool,
// the scope, the backend read, the gRPC endpoint -- is resolved BEFORE the mint, so
// there is no path on which a credential is written and the request then 409s. And
// the preview branch returns before the mint entirely, which is what makes
// "preview mints nothing" structural rather than a flag someone must remember to
// check inside CreateAPIKey's caller.
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
			`tool must be one of "yocto", "moon", "ccache", "sccache", "bazel", `+
				`"containerd", "buildkit", "podman", "docker"`)
	}

	keyScope, err := a.snippetScope(req.Scope, p, s)
	if err != nil {
		return err
	}

	// THE GENERATOR READS cache_backends (B1, critique 5). Before this it touched
	// the store zero times and emitted config for mounts that may not exist -- most
	// consequentially BB_HASHSERVE for a project with no hashserv backend, which
	// 404s, which bb.siggen catches and turns into unihash = taskhash: a green build
	// whose every sstate object misses.
	backends, err := a.backendSetFor(ctx, s.ProjectID)
	if err != nil {
		return err
	}

	scheme, host := a.externalOrigin(r)
	baseURL := fmt.Sprintf("%s://%s/cache/%s/%s", scheme, host, s.OrgSlug, s.ProjectSlug)

	// Resolved here, BEFORE the mint, because its failure mode is a 409.
	grpcEP, err := a.grpcEndpointFor(tool, scheme, host)
	if err != nil {
		return err
	}

	target := snippetTarget{
		tool: tool, scheme: scheme, host: host, baseURL: baseURL,
		org: s.OrgSlug, project: s.ProjectSlug,
		token: snippetTokenPlaceholder, scope: keyScope,
		grpcEndpoint: grpcEP, backends: backends,
	}

	if req.Preview {
		writeJSON(w, http.StatusOK, snippetResponse(target, buildSnippet(target), nil))

		return nil
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

	target.token = key.Token

	created := &CreatedAPIKey{
		APIKey: APIKey{
			ID: uuidString(row.ID), Name: row.Name, ProjectID: uuidString(row.ProjectID),
			TokenPrefix: row.TokenPrefix, Scope: string(row.Scope),
			OwnerID: uuidString(row.UserID), OwnerEmail: p.Email(), OwnerName: p.DisplayName(),
			CreatedAt: row.CreatedAt.Time, ExpiresAt: timePtr(row.ExpiresAt),
			LastUsedAt: nil, RevokedAt: nil,
		},
		Token: key.Token,
	}

	writeJSON(w, http.StatusCreated, snippetResponse(target, buildSnippet(target), created))

	return nil
}

// snippetResponse assembles the wire body. key == nil is a PREVIEW: 200, no
// credential, snippetTokenPlaceholder wherever a token would be.
func snippetResponse(t snippetTarget, c snippetContent, key *CreatedAPIKey) SnippetResponse {
	return SnippetResponse{
		Tool:         t.tool,
		Host:         hostOnly(t.host),
		BaseURL:      t.baseURL,
		LocalConf:    c.localConf,
		Netrc:        c.netrc,
		PushCommands: c.pushCommands,
		Files:        c.files,
		Env:          c.env,
		APIKey:       key,
		Preview:      key == nil,
		Warnings:     c.warnings,
	}
}

// snippetScope resolves the minted key's scope.
//
// An EXPLICIT scope is passed through and capped by auth.CreateAPIKey (403 if it
// exceeds the caller's project role). An OMITTED scope resolves to the caller's own
// ceiling instead of hard-defaulting to write -- see SnippetRequest.Scope.
func (a *API) snippetScope(requested string, p Principal, s scope) (auth.Scope, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		return scopeOf(requested)
	}

	if p.CanWriteProject(s.OrgID, s.ProjectID) {
		return auth.ScopeWrite, nil
	}

	return auth.ScopeRead, nil
}

// ---------------------------------------------------------------------------
// Which backends a project actually has
// ---------------------------------------------------------------------------

// backendSet is the project's configured cache_backends, keyed by kind, with the
// value being `enabled`. Presence in the map is "configured"; the value is
// "enabled". A block is emitted only when BOTH hold -- a disabled backend serves
// nothing, so config pointing at it is the same lie as config pointing at an
// absent one, just with a different fix.
type backendSet map[repository.BackendKind]bool

// usable reports whether a block for kind may be emitted.
func (s backendSet) usable(kind repository.BackendKind) bool { return s[kind] }

// why renders the reason a block was omitted, for a Warnings entry. It is only
// ever called when usable() is false.
func (s backendSet) why(kind repository.BackendKind) string {
	if _, configured := s[kind]; !configured {
		return "is not configured"
	}

	return "is configured but disabled"
}

// backendSetFor loads the project's backends. One query, the same
// ListBackendsForProject every other backend-facing handler uses -- a project has
// at most five rows.
func (a *API) backendSetFor(ctx context.Context, projectID pgtype.UUID) (backendSet, error) {
	rows, err := a.store.ListBackendsForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("load backends for snippet: %w", err)
	}

	set := make(backendSet, len(rows))
	for _, b := range rows {
		set[b.Kind] = b.Enabled
	}

	return set, nil
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
	warnings     []string
}

// snippetTarget is everything a per-tool builder needs: the resolved origin, the
// tenancy, the credential (or its preview placeholder), the scope, the resolved gRPC
// endpoint (empty for the seven tools that do not speak REAPI), and -- the B1
// addition -- WHICH BACKENDS THIS PROJECT ACTUALLY HAS.
type snippetTarget struct {
	tool string
	// scheme/host are the resolved external origin; host MAY carry a port.
	scheme, host string
	// baseURL is scheme://host/cache/{org}/{project}.
	baseURL string
	org     string
	project string
	// token is the minted plaintext, or snippetTokenPlaceholder on a preview.
	token string
	scope auth.Scope
	// grpcEndpoint is already resolved and already validated (see grpcEndpoint).
	grpcEndpoint string
	backends     backendSet
}

// buildSnippet routes to the per-tool builder. tool is already validated against the
// closed set, so the default arm is yocto by construction.
//
// EVERY ARM IS BACKEND-GATED. A tool whose backend is absent or disabled gets NO
// config and one warning naming the omission -- never a config block pointing at a
// mount this project does not serve. That is not a nicety: config for an unmounted
// path is what these clients tolerate SILENTLY (a 404 is an ordinary cache miss to
// every one of them), so the alternative is a snippet that looks right, builds
// green, and caches nothing.
func buildSnippet(t snippetTarget) snippetContent {
	switch t.tool {
	case SnippetToolMoon:
		return gateOn(t, repository.BackendKindBazel, func() snippetContent {
			return moonSnippet(t)
		})
	case SnippetToolCcache:
		return gateOn(t, repository.BackendKindBazel, func() snippetContent {
			return ccacheSnippet(t)
		})
	case SnippetToolSccache:
		return gateOn(t, repository.BackendKindBazel, func() snippetContent {
			return sccacheSnippet(t)
		})
	case SnippetToolBazel:
		return gateOn(t, repository.BackendKindBazel, func() snippetContent {
			return bazelSnippet(t)
		})
	case SnippetToolContainerd:
		return gateOn(t, repository.BackendKindOci, func() snippetContent {
			return containerdSnippet(t)
		})
	case SnippetToolBuildkit:
		return gateOn(t, repository.BackendKindOci, func() snippetContent {
			return buildkitSnippet(t)
		})
	case SnippetToolPodman:
		return gateOn(t, repository.BackendKindOci, func() snippetContent {
			return podmanSnippet(t)
		})
	case SnippetToolDocker:
		return gateOn(t, repository.BackendKindOci, func() snippetContent {
			return dockerSnippet(t)
		})
	default: // yocto -- three independent backends, three independent blocks
		return yoctoSnippet(t)
	}
}

// gateOn runs build only when the project has kind configured AND enabled;
// otherwise it returns a content with nothing but the warning that says so.
//
// The one-backend tools all take this shape. yocto does not, because it composes
// THREE backends into one file and the useful answer to "you have sstate but no
// hashserv" is the sstate half plus a loud warning, not an empty response.
func gateOn(t snippetTarget, kind repository.BackendKind, build func() snippetContent) snippetContent {
	if t.backends.usable(kind) {
		return build()
	}

	return snippetContent{
		warnings: []string{fmt.Sprintf(
			"No %s configuration was generated: this project's %s backend %s, so "+
				"/cache/%s/%s is not served for it and every request would 404. "+
				"Configure a %s backend and generate this snippet again.",
			t.tool, kind, t.backends.why(kind), t.org, t.project, kind)},
	}
}

// grpcEndpointFor resolves the gRPC endpoint for the tools that need one, and
// returns "" for the seven that do not. Only moon and bazel speak REAPI.
func (a *API) grpcEndpointFor(tool, scheme, host string) (string, error) {
	if tool != SnippetToolMoon && tool != SnippetToolBazel {
		return "", nil
	}

	return a.grpcEndpoint(scheme, host)
}

// grpcEndpoint builds the gRPC endpoint URL with an EXPLICIT port, for moon's
// remote.host and bazel's --remote_cache. tonic (moon) and Bazel both require the
// authority to carry a port, so hostOnly -- which strips it -- is the wrong helper.
//
// THE PORT COMES FROM --grpc-addr, NEVER FROM THE HTTP ORIGIN. M4 derived the whole
// authority from the request, so it reused the HTTP port (or the scheme default when
// the Host had none). REAPI is served on a DEDICATED listener -- a shared h2c port is
// forbidden outright (grpc-go's ServeHTTP path has no Drain, and it would put the
// hashserv WebSocket at an ingress's mercy) -- so there was NO configuration,
// including a plain `bakery serve`, under which the old derivation emitted 9092. A
// moon or Bazel client pointed at a port nothing listens on does not fail the build:
// it disables its cache, logs at DEBUG, and reports 0% hits on green builds.
//
// Three cases:
//
//   - --grpc-external-endpoint set: used VERBATIM. It is the only input that can know
//     what an ingress did with the listener.
//   - --grpc-addr set: derive host-from-origin + port-from-GRPCAddr, and warn ONCE.
//     The derivation is a guess about the ingress and it is right only for the
//     single-host deployment; loopback is NOT refused, because grpcs://localhost:9092
//     is genuinely correct for local dev and the e2e job.
//   - --grpc-addr empty (the REAPI listener is off) and no override: 409. Not a
//     snippet that connects nowhere, and not not_implemented -- REAPI is implemented,
//     this deployment has it switched off.
func (a *API) grpcEndpoint(scheme, host string) (string, error) {
	if a.grpcExternalEndpoint != "" {
		return a.grpcExternalEndpoint, nil
	}

	if a.grpcAddr == "" {
		return "", errConflict(CodeConflict,
			"this server has no gRPC listener, so a Bazel/moon snippet would point at nothing; "+
				"ask your operator to set --grpc-addr or --grpc-external-endpoint")
	}

	_, port, err := net.SplitHostPort(a.grpcAddr)
	if err != nil || port == "" {
		return "", errConflict(CodeConflict,
			fmt.Sprintf("this server's --grpc-addr (%q) has no port, so the public gRPC endpoint "+
				"cannot be derived; ask your operator to set --grpc-external-endpoint", a.grpcAddr))
	}

	grpcScheme := "grpc"
	if scheme == "https" {
		grpcScheme = "grpcs"
	}

	endpoint := grpcScheme + "://" + net.JoinHostPort(hostOnly(host), port)

	a.warnGRPCOnce.Do(func() {
		a.log.Warn("api: GRPC_EXTERNAL_ENDPOINT is unset; deriving the snippet's gRPC endpoint "+
			"from the request host and the --grpc-addr port",
			"derived", endpoint,
			"fix", "set --grpc-external-endpoint / GRPC_EXTERNAL_ENDPOINT to the public gRPC authority")
	})

	return endpoint, nil
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
func moonSnippet(t snippetTarget) snippetContent {
	yaml := strings.Join([]string{
		"remote:",
		"  api: 'grpc'",
		fmt.Sprintf("  host: '%s'", t.grpcEndpoint),
		"  auth:",
		"    token: 'BAKERY_TOKEN'   # the NAME of an env var, NOT the token itself",
		"  cache:",
		fmt.Sprintf("    instanceName: '%s/%s'   # the project selector for gRPC", t.org, t.project),
		"    compression: 'none'   # Bakery advertises IDENTITY only; 'zstd' earns a",
		"                          # fallback warning against a cache that cannot serve it",
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: ".moon/workspace.yml", Language: "yaml", Content: yaml}},
		env:   []SnippetEnvVar{{Name: "BAKERY_TOKEN", Value: t.token}},
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
func ccacheSnippet(t snippetTarget) snippetContent {
	line := fmt.Sprintf("remote_storage = http://%s:@%s/cache/%s/%s @layout=bazel @connect-timeout=1000",
		t.token, t.host, t.org, t.project)

	if t.scope == auth.ScopeRead {
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
// The gotcha text rides in Warnings for the same reason docker's does: sccache is
// configured entirely through environment variables, and an `export` line has
// nowhere to put a caution the way ccache.conf or hosts.toml do. It states the
// credential shape explicitly -- ONE opaque bkry_ token, never a key-id/key-secret
// pair, which is a credential Bakery cannot issue -- because that is the drift the
// console's hand-authored copy carried.
func sccacheSnippet(t snippetTarget) snippetContent {
	return snippetContent{
		env: []SnippetEnvVar{
			{Name: "SCCACHE_WEBDAV_ENDPOINT", Value: t.baseURL},
			{Name: "SCCACHE_WEBDAV_KEY_PREFIX", Value: "sccache"},
			{Name: "SCCACHE_WEBDAV_TOKEN", Value: t.token},
		},
		warnings: []string{
			"SCCACHE_WEBDAV_TOKEN is one opaque bkry_ token sent as a Bearer credential, " +
				"not a key-id and key-secret pair. SCCACHE_WEBDAV_KEY_PREFIX is mandatory: " +
				"without it sccache writes under a prefix Bakery does not route, and its " +
				"WebDAV client answers a failed probe by running the whole process read-only " +
				"rather than failing loudly, so the cache silently never populates.",
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
func bazelSnippet(t snippetTarget) snippetContent {
	rc := strings.Join([]string{
		fmt.Sprintf("build --remote_cache=%s", t.grpcEndpoint),
		fmt.Sprintf("build --remote_instance_name=%s/%s", t.org, t.project),
		fmt.Sprintf("build --remote_header=authorization=Bearer %s", t.token),
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: ".bazelrc", Language: "bazelrc", Content: rc}},
	}
}

// dockerMirrorURL is the docker/v2 mount: containerd and Docker Engine both land
// here. containerd APPENDS /v2 to whatever host it is given (unless it already ends
// in one), and Docker Engine's ValidateMirror accepts a path -- contrary to the
// widely-repeated "mirror URLs must be domain roots" claim, which is true only of
// registry:2's own `remoteurl`. So the configured value carries NO trailing /v2 and
// DOES carry the tenant path.
func dockerMirrorURL(scheme, host, org, project string) string {
	return fmt.Sprintf("%s://%s/cache/%s/%s/docker", scheme, host, org, project)
}

// v2MirrorPath is the bare, schemeless tenant path BuildKit and podman both mirror
// onto -- Bakery's second route family, /v2/{org}/{project}/{rest...}. Neither tool
// takes a scheme in its mirror value; BuildKit has a separate `http` boolean and
// podman infers TLS from its own `insecure` setting.
func v2MirrorPath(host, org, project string) string {
	return fmt.Sprintf("%s/%s/%s", host, org, project)
}

// containerdSnippet builds one hosts.toml, with BOTH working auth paths -- because
// they trade off differently, and picking one for the user would hide the choice.
//
// DEFAULT (the block as shown): no credential configured on the host at all.
// containerd's authorizer follows the WWW-Authenticate: Bearer challenge Bakery's
// ping AND every 401 already carry, fetches a token from the advertised realm, and
// retries with it -- zero config beyond this file, and it is enough on its own
// against an open (ReadAuthRequired=false) backend.
//
// ALTERNATIVE (commented out below): skip the challenge round trip entirely with a
// static per-request header. `[host."...".header]` is unconditional -- it is applied
// to every request with no negotiation -- and AuthenticateToken accepts a bare Bearer
// bkry_ token exactly the same whether it arrived this way or via the challenge flow.
// This is also the ONLY of the two paths that works before Bakery answers a single
// request: there is no discovery round trip to get wrong.
//
// containerd APPENDS /v2 to the host URL itself unless it already ends in one (or
// override_path=true is set), so the configured host below carries NO trailing /v2.
func containerdSnippet(t snippetTarget) snippetContent {
	mount := dockerMirrorURL(t.scheme, t.host, t.org, t.project)

	toml := strings.Join([]string{
		`server = "https://registry-1.docker.io"   # one file per upstream namespace; same [host] block in each`,
		"",
		fmt.Sprintf(`[host.%q]`, mount),
		`  capabilities = ["pull", "resolve"]`,
		"",
		"  # DEFAULT: no credential here -- containerd follows Bakery's Bearer challenge",
		"  # automatically. Sufficient on its own against an open (unauthenticated-read) backend.",
		"",
		"  # ALTERNATIVE: pin the token as a static header and skip the challenge round trip",
		"  # entirely. Mutually exclusive with the default above -- uncomment to use it:",
		fmt.Sprintf(`  # [host.%q.header]`, mount),
		fmt.Sprintf(`  #   Authorization = "Bearer %s"`, t.token),
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{
			Path: "/etc/containerd/certs.d/docker.io/hosts.toml", Language: "toml", Content: toml,
		}},
	}
}

// buildkitSnippet builds buildkitd.toml's mirrors block.
//
// BuildKit puts the mirror prefix AFTER /v2 (path.Join("/v2", mirrorPath)) -- the
// OPPOSITE of containerd -- so the value here is the BARE tenant path, no /cache and
// no /docker segment: Bakery's second route family exists for exactly this shape.
//
// ⚠️ BuildKit's Basic-auth path only installs a handler when BOTH username AND secret
// are non-empty. A docker config.json entry for this host with an empty password does
// not error -- it silently skips the mirror for that host. Configure BOTH fields with
// the token (one opaque bkry_ token, no id:secret split -- see the package doc), or
// configure neither and let the Bearer/anonymous flow serve an open backend.
func buildkitSnippet(t snippetTarget) snippetContent {
	mirror := v2MirrorPath(t.host, t.org, t.project)

	toml := strings.Join([]string{
		`[registry."docker.io"]`,
		fmt.Sprintf(`  mirrors = [%q]`, mirror),
		fmt.Sprintf("  http = %t", t.scheme == "http"),
		"",
		"# Credentials (only needed if this backend requires authenticated reads -- BOTH",
		"# fields or BuildKit silently skips the mirror):",
		fmt.Sprintf("#   docker login %s -u %s -p %s", t.host, t.token, t.token),
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: "/etc/buildkit/buildkitd.toml", Language: "toml", Content: toml}},
	}
}

// podmanSnippet builds registries.conf's mirror block.
//
// containers/image NEVER sends ?ns= -- podman, skopeo and CRI-O all go through it --
// so this project's OCI backend MUST have default_upstream set server-side; there is
// no way for Bakery to learn the upstream from the request the way it does for
// containerd/BuildKit.
//
// ⚠️ containers/image STRIPS the upstream registry's credentials whenever the mirror
// domain differs from the image's own domain (verified in docker_image_src.go), so a
// docker.io login does NOT reach Bakery -- there is no inherited-credential path here
// at all, unlike Docker Engine. Authenticate to the mirror host directly.
func podmanSnippet(t snippetTarget) snippetContent {
	mirror := v2MirrorPath(t.host, t.org, t.project)

	toml := strings.Join([]string{
		`[[registry]]`,
		`  location = "docker.io"`,
		"",
		`  [[registry.mirror]]`,
		fmt.Sprintf(`    location = %q`, mirror),
		`    # no ?ns= is ever sent here -- the backend's default_upstream must be docker.io`,
		"",
		"# Credentials (only needed if this backend requires authenticated reads) do NOT",
		"# inherit from a docker.io login -- podman strips cross-domain credentials.",
		"# Authenticate to the mirror host directly:",
		fmt.Sprintf("#   podman login %s -u %s -p %s", t.host, t.token, t.token),
	}, "\n") + "\n"

	return snippetContent{
		files: []SnippetFile{{Path: "/etc/containers/registries.conf", Language: "toml", Content: toml}},
	}
}

// dockerSnippet builds daemon.json's registry-mirrors block for plain Docker Engine
// (dockerd) -- the fourth, officially-supported client. Product decision: support it,
// with a loud warning attached (see below), rather than leave it undocumented while
// operators point it at Bakery anyway.
//
// HUB-ONLY, and that is dockerd's limit, not Bakery's: registry-mirrors only ever
// mirrors Docker Hub, so this always targets the SAME docker/v2 mount containerd
// uses -- no separate route exists or is needed.
//
// A PATH PREFIX IS VALID. Docker Engine v28+'s ValidateMirror rejects only a query
// string, a fragment, or embedded userinfo -- NOT a path -- contrary to older docs
// (and this doc's own §3.1, corrected alongside this change) claiming mirror URLs
// must be domain roots. That claim is true only of registry:2's own `remoteurl`.
//
// NO TOKEN IN THIS FILE, and that is not an oversight: Docker Engine forwards the
// user's OWN Docker Hub credentials to whatever registry-mirrors names, unscoped, on
// every pull -- there is no per-mirror credential slot to put a Bakery token in.
// Consequently a forwarded Hub credential is never bkry_-shaped, so it is always
// treated as anonymous; Docker Engine therefore only works against an OCI backend
// with ReadAuthRequired=false. daemon.json is also strict JSON with no comment
// syntax, so the credential-transit warning cannot live inside the file the way
// containerd's or ccache's can -- it ships as SnippetResponse.Warnings instead, which
// the UI MUST render loudly.
func dockerSnippet(t snippetTarget) snippetContent {
	mirror := dockerMirrorURL(t.scheme, t.host, t.org, t.project)

	body := struct {
		RegistryMirrors []string `json:"registry-mirrors"`
	}{RegistryMirrors: []string{mirror}}

	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		// Unreachable: body is a fixed shape with no cyclic or unmarshalable field.
		raw = []byte("{}")
	}

	return snippetContent{
		files: []SnippetFile{{Path: "/etc/docker/daemon.json", Language: "json", Content: string(raw) + "\n"}},
		warnings: []string{
			"Docker Engine forwards your real Docker Hub login to this mirror on every " +
				"pull, unscoped -- only enable this if you accept that credential transit. " +
				"It is never logged. Works only against an OCI backend with authenticated " +
				"reads turned off: Docker Engine cannot present a Bakery key.",
		},
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

// yoctoSnippet composes conf/local.conf, ~/.netrc and the push commands out of the
// THREE independent backends a Yocto build touches: sstate, downloads and hashserv.
// Each contributes its own block, its own push command and its own netrc line, and
// each is emitted ONLY when this project has that backend configured and enabled.
//
// THE HASHSERV GATE IS THE ONE THAT MATTERS. Emitting BB_HASHSERVE for a project
// with no hashserv backend points bitbake at a path that 404s; bb.siggen CATCHES the
// resulting connection error, warns, and sets `unihash = taskhash`. The build then
// completes, green, while every sstate object misses -- the sstate filename embeds
// the unihash, so a task the server would have remapped now looks up an object that
// was never written under that name. Nothing reports a problem. So a missing
// hashserv backend suppresses the block and raises a loud warning instead: sstate
// and hash equivalence are one unit, and the honest answer to half of it is to say
// so, not to configure the half that exists and hope.
func yoctoSnippet(t snippetTarget) snippetContent {
	var (
		conf  []string
		netrc []string
		push  []string
		warn  []string
	)

	sstate := t.backends.usable(repository.BackendKindSstate)
	downloads := t.backends.usable(repository.BackendKindDownloads)
	hashserv := t.backends.usable(repository.BackendKindHashserv)

	// --- sstate. SSTATE_MIRRORS rewrites file://.* to the mirror URL and appends the
	// sstate PATH; the trailing `downloadfilename=PATH` is NOT optional -- without it
	// bitbake fetches the URL but writes it to the wrong local name and the setscene
	// object is a miss on the next build.
	if sstate {
		conf = append(conf,
			"# --- sstate mirror (read) ---",
			fmt.Sprintf(`SSTATE_MIRRORS ?= "file://.* %s/sstate/PATH;downloadfilename=PATH"`, t.baseURL),
		)
		push = append(push, fmt.Sprintf(
			"BAKERY_API_KEY=%s bakery sstate push %s %s ./build/sstate-cache", t.token, t.org, t.project))
	} else {
		warn = append(warn, fmt.Sprintf(
			"SSTATE_MIRRORS was omitted: this project's sstate backend %s, so "+
				"%s/sstate is not served and every setscene fetch against it would 404.",
			t.backends.why(repository.BackendKindSstate), t.baseURL))
	}

	// --- downloads (the source premirror). own-mirrors is the inherit that turns
	// SOURCE_MIRROR_URL into a premirror.
	if downloads {
		conf = append(conf, blankBefore(conf)...)
		conf = append(conf,
			"# --- source premirror (read) ---",
			`INHERIT += "own-mirrors"`,
			fmt.Sprintf(`SOURCE_MIRROR_URL ?= "%s/downloads"`, t.baseURL),
			`BB_GENERATE_MIRROR_TARBALLS = "1"`,
		)
		push = append(push, fmt.Sprintf(
			"BAKERY_API_KEY=%s bakery downloads push %s %s ./build/downloads", t.token, t.org, t.project))
	} else {
		warn = append(warn, fmt.Sprintf(
			"The own-mirrors source premirror lines were omitted: this project's downloads "+
				"backend %s, so %s/downloads is not served.",
			t.backends.why(repository.BackendKindDownloads), t.baseURL))
	}

	// --- hash equivalence.
	if hashserv {
		ws := hashservWSSURL(t.scheme, t.host, t.org, t.project)

		conf = append(conf, blankBefore(conf)...)
		conf = append(conf,
			"# --- hash equivalence ---",
			`BB_SIGNATURE_HANDLER = "OEEquivHash"`,
			fmt.Sprintf(`BB_HASHSERVE = "%s"`, ws),
			`# Do NOT set BB_HASHSERVE = "auto", and do NOT set BB_HASHSERVE_UPSTREAM:`,
			"# that topology spawns a LOCAL hashserv whose upstream link is pull-only and",
			"# anonymous, so Bakery would never receive a single hash report.",
		)
	} else {
		warn = append(warn, fmt.Sprintf(
			"BB_HASHSERVE was omitted: this project's hashserv backend %s. Emitting it "+
				"would point bitbake at a path that 404s, and bb.siggen answers that by "+
				"warning and setting unihash = taskhash -- the build then completes green "+
				"while every sstate object misses, because the sstate filename embeds the "+
				"unihash. sstate and hash equivalence are one unit: configure a hashserv "+
				"backend and generate this snippet again.",
			t.backends.why(repository.BackendKindHashserv)))
	}

	// --- credentials. The hostname-keyed line serves the sstate/downloads HTTP Basic
	// path; the hashserv line is keyed on the FULL URL and is a different line, not a
	// different argument to the same one. See hashservNetrcLine.
	if sstate || downloads {
		netrc = append(netrc, netrcLine(hostOnly(t.host), t.token))
	}

	if hashserv {
		netrc = append(netrc, hashservNetrcLine(hashservWSSURL(t.scheme, t.host, t.org, t.project), t.token))
	}

	return snippetContent{
		localConf:    joinBlock(conf),
		netrc:        strings.Join(netrc, ""),
		pushCommands: push,
		files:        nil,
		env:          nil,
		warnings:     warn,
	}
}

// blankBefore returns a one-element blank-line slice when lines already has
// content, and nothing when it does not -- so an omitted first block does not leave
// local.conf starting with an empty line.
func blankBefore(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}

	return []string{""}
}

// joinBlock renders a config block, or "" when every block was gated out -- an
// empty string, never a file consisting of one newline.
func joinBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}

	return strings.Join(lines, "\n") + "\n"
}

// hashservWSSURL is the WebSocket URL bitbake dials for hash equivalence. It uses
// the resolved host PORT AND ALL: hashserv rides the public listener and has no port
// of its own, unlike REAPI.
func hashservWSSURL(scheme, host, org, project string) string {
	ws := "wss"
	if scheme == "http" {
		ws = "ws"
	}

	return fmt.Sprintf("%s://%s/cache/%s/%s/hashserv", ws, host, org, project)
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

// hashservNetrcLine is the ~/.netrc entry for HASH EQUIVALENCE, and it is a
// SEPARATE function from netrcLine on purpose rather than a second argument to it.
//
// oe-core calls `netrc.authenticators(BB_HASHSERVE)` -- an exact string match on the
// FULL URL, scheme and path included. `machine bakery.corp` does not match
// `wss://bakery.corp/cache/acme/firmware/hashserv` and is silently ignored, which
// makes the build unauthenticated: hashserv denies IN-BAND, bb.siggen catches the
// denial, and the build goes green with unihash = taskhash. Collapsing these two
// keying schemes back into one parameterised helper is the single most common way to
// get a silently-unauthenticated build, and a separate name is what stops a future
// reader from doing it.
//
// THE ENVIRONMENT-VARIABLE CREDENTIAL PATH IS DELIBERATELY NOT EMITTED.
// client-config.md documents BB_HASHSERVE_USERNAME / BB_HASHSERVE_PASSWORD as an
// alternative, but bitbake only sees them if they are ALSO named in
// BB_ENV_PASSTHROUGH_ADDITIONS -- an easy line to omit and an invisible failure when
// it is. One credential mechanism, in one file, is the deliberate choice. Anyone
// adding the env form to Env[] owes BB_ENV_PASSTHROUGH_ADDITIONS alongside it.
func hashservNetrcLine(hashserveURL, token string) string {
	return fmt.Sprintf("machine %s login %s password %s\n", hashserveURL, token, token)
}

// externalOrigin resolves the scheme and host this snippet should point at.
//
// PRECEDENCE (B1): --external-url, then X-Forwarded-*, then the request's own Host.
// Config wins unconditionally, because an operator who has stated the public origin
// has stated it -- a header cannot then move a live credential to a host of the
// sender's choosing. Headers remain the fallback because behind a TLS-terminating
// proxy the binary sees plain http on a private hop and its own Host is the internal
// name; the snippet route is AccessProjectRead, so a spoofed header can at worst
// mislead the caller about their own snippet.
//
// THE SCHEME FAILS CLOSED TO https (the oci/token.go posture, and for the same
// reason): this response carries a bearer credential -- BAKERY_TOKEN,
// --remote_header=authorization=Bearer, a netrc password -- and a wrong https is an
// unreachable endpoint, a loud and debuggable client error. A wrong http is a
// credential disclosure that nothing logs. It defaults to http ONLY when the host is
// provably loopback, which is `bakery serve` on a laptop and the e2e job.
//
// X-Forwarded-Proto is CONSTRAINED to exactly "http" or "https" -- never taken
// verbatim -- and "http" is honoured only under the SAME fail-closed rule as the
// no-header default arm below: the host must be provably loopback. Without that
// constraint the header is a straight scheme-injection primitive (any string a
// caller sends becomes the literal `scheme://` prefix on a response that carries a
// live credential), and even restricted to "http" alone, taking it unconditionally
// lets a caller on a public host downgrade their own snippet's bearer credential to
// plaintext by sending one spoofable header. AccessProjectRead means only the
// requester's own snippet is at risk either way, but "at most self-inflicted" is
// not a reason to skip the same rule the default arm already enforces.
func (a *API) externalOrigin(r *http.Request) (scheme, host string) {
	if s, h, ok := a.configuredOrigin(); ok {
		return s, h
	}

	host = r.Host
	if fwd := firstForwarded(r.Header.Get("X-Forwarded-Host")); fwd != "" {
		host = fwd
	}

	switch fwd := firstForwarded(r.Header.Get("X-Forwarded-Proto")); {
	case fwd == "https":
		scheme = "https"
	case fwd == "http" && isLoopbackHost(host):
		scheme = "http"
	case r.TLS != nil:
		scheme = "https"
	case isLoopbackHost(host):
		scheme = "http"
	default:
		scheme = "https"
	}

	return scheme, host
}

// configuredOrigin parses --external-url. A value that is not an absolute URL with a
// host is IGNORED (with one warning per process) rather than fatal: it is also the
// OCI Bearer realm, and refusing every snippet over a malformed operator string
// would take a working cache's console down for a cosmetic misconfiguration.
func (a *API) configuredOrigin() (scheme, host string, ok bool) {
	if a.externalURL == "" {
		return "", "", false
	}

	u, err := url.Parse(strings.TrimSuffix(a.externalURL, "/"))
	if err != nil || u.Host == "" {
		a.warnExternalURLOnce.Do(func() {
			a.log.Warn("api: --external-url is not an absolute URL with a host; falling back to the request origin",
				"external_url", a.externalURL,
				"fix", "set --external-url / EXTERNAL_URL to e.g. https://bakery.example.com")
		})

		return "", "", false
	}

	scheme = u.Scheme
	if scheme == "" {
		scheme = "https"
	}

	return scheme, u.Host, true
}

// isLoopbackHost reports whether a host[:port] is plainly local -- the one case
// where an http origin is not a credential disclosure. Same helper, same rule, as
// oci.isLoopbackHost; the two packages do not share a dependency and this is four
// lines.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
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
