package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The DB-backed half of B1 (spec docs/design/specs/2026-08-15-spa-api-wiring.md).
//
// Everything in snippets_test.go drives a fake minter, which can prove the handler
// did not CALL CreateAPIKey. Only a real database can prove the thing that actually
// matters: that no api_keys ROW exists afterwards. These tests run against the real
// chain -- real session, real auth.Service, real serviceKeyMinter -- through the
// same harness the rest of this package's end-to-end tests use.

// devScope is the org/project dev-login seeds, with the dev user as project admin.
const devScope = "/orgs/dev-org/projects/playground"

// apiKeyCount counts every api_keys row in the installation, revoked or not.
func (h *harness) apiKeyCount() int64 {
	h.t.Helper()

	var n int64
	if err := h.store.Pool().QueryRow(h.t.Context(), `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
		h.t.Fatalf("count api_keys: %v", err)
	}

	return n
}

// createBackend configures a backend on dev-org/playground through the real API.
func (h *harness) createBackend(kind string) {
	h.t.Helper()

	status, body := h.req(http.MethodPost, Prefix+devScope+"/backends",
		fmt.Sprintf(`{"kind":%q,"config":{}}`, kind), nil)
	if status != http.StatusCreated {
		h.t.Fatalf("create %s backend: status = %d, body %s", kind, status, body)
	}
}

// TestSnippetPreviewMintsNothing is the gating test for B1's preview mode, and the
// assertion is on the DATABASE, not on a mock.
//
// Before preview existed, every POST to this endpoint minted a live write-scoped
// credential unconditionally, so a screen rendering nine tool tiles by fetching each
// one minted nine keys per page view -- with no revocation story and no way to look
// at a config without paying for it. Preview is the read path. It must leave the
// api_keys table exactly as it found it, for every tool, however many times it is
// called; and the mint path immediately afterwards must still work, because a
// preview that broke minting would be a worse bug than the one it fixes.
func TestSnippetPreviewMintsNothing(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	// A real, fully-configured project, so the previews below are full configs
	// rather than warnings -- the point is that a COMPLETE snippet costs nothing.
	for _, kind := range []string{"sstate", "downloads", "hashserv", "bazel", "oci"} {
		h.createBackend(kind)
	}

	// The gRPC listener is on in this installation, so bazel/moon are not 409s.
	h.api.grpcAddr = "127.0.0.1:9092"

	if before := h.apiKeyCount(); before != 0 {
		t.Fatalf("api_keys = %d before any request, want 0", before)
	}

	for _, tool := range snippetTools {
		body := fmt.Sprintf(`{"tool":%q,"preview":true}`, tool)

		status, raw := h.req(http.MethodPost, Prefix+devScope+"/snippet", body, nil)
		if status != http.StatusOK {
			t.Fatalf("%s preview: status = %d, want 200 (body %s)", tool, status, raw)
		}

		var out SnippetResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s preview: decode: %v", tool, err)
		}

		if !out.Preview || out.APIKey != nil {
			t.Errorf("%s preview: preview = %v, api_key = %+v; want true / absent",
				tool, out.Preview, out.APIKey)
		}

		// docker is the one tool whose config carries no credential at ALL -- Docker
		// Engine has no per-mirror credential slot -- so it has nowhere to put a
		// placeholder either. Every other tool must show one.
		if tool != SnippetToolDocker && !strings.Contains(string(raw), snippetTokenPlaceholder) {
			t.Errorf("%s preview: the placeholder must appear where the credential would:\n%s", tool, raw)
		}

		if n := h.apiKeyCount(); n != 0 {
			t.Fatalf("%s preview wrote %d api_keys row(s); a preview mints NOTHING", tool, n)
		}
	}

	// Previewing every tool cost zero credentials. The explicit gesture still mints
	// exactly one.
	status, raw := h.req(http.MethodPost, Prefix+devScope+"/snippet", `{"tool":"yocto"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("mint: status = %d, want 201 (body %s)", status, raw)
	}

	var minted SnippetResponse
	if err := json.Unmarshal(raw, &minted); err != nil {
		t.Fatalf("mint: decode: %v", err)
	}

	if minted.Preview {
		t.Error("a minting response must report preview = false")
	}

	if minted.APIKey == nil || !strings.HasPrefix(minted.APIKey.Token, "bkry_") {
		t.Fatalf("mint: api_key = %+v, want a bkry_-prefixed plaintext token", minted.APIKey)
	}

	if n := h.apiKeyCount(); n != 1 {
		t.Fatalf("api_keys = %d after one explicit mint, want exactly 1", n)
	}
}

// TestSnippetReadsRealBackends is the end-to-end half of critique 5: the generator
// reads this project's cache_backends through the REAL store, not a fake.
//
// The negative leg is the one that matters. An sstate-only project must not be told
// to set BB_HASHSERVE: that URL 404s, bb.siggen catches the failure and degrades to
// unihash = taskhash, and the build goes green while every sstate object misses on
// its unihash-embedded filename.
func TestSnippetReadsRealBackends(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	preview := func() SnippetResponse {
		t.Helper()

		status, raw := h.req(http.MethodPost, Prefix+devScope+"/snippet",
			`{"tool":"yocto","preview":true}`, nil)
		if status != http.StatusOK {
			t.Fatalf("preview: status = %d, want 200 (body %s)", status, raw)
		}

		var out SnippetResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("preview: decode: %v", err)
		}

		return out
	}

	// Nothing configured: no config at all, and a warning per omitted backend.
	if out := preview(); out.LocalConf != "" || len(out.Warnings) != 3 {
		t.Fatalf("a project with no backends must emit no local.conf and 3 warnings; got %q / %v",
			out.LocalConf, out.Warnings)
	}

	// sstate only: the sstate block, NO hashserv block, and the loud warning.
	h.createBackend("sstate")

	out := preview()
	if !strings.Contains(out.LocalConf, "SSTATE_MIRRORS") {
		t.Errorf("the sstate block must be emitted:\n%s", out.LocalConf)
	}

	if strings.Contains(out.LocalConf, "BB_HASHSERVE") {
		t.Errorf("BB_HASHSERVE must NOT be emitted without a hashserv backend:\n%s", out.LocalConf)
	}

	if !strings.Contains(strings.Join(out.Warnings, " "), "unihash = taskhash") {
		t.Errorf("the omission warning must name the failure it prevents: %v", out.Warnings)
	}

	// Now add hashserv: the block appears, and so does the second netrc line.
	h.createBackend("hashserv")

	out = preview()
	if !strings.Contains(out.LocalConf, `BB_SIGNATURE_HANDLER = "OEEquivHash"`) ||
		!strings.Contains(out.LocalConf, "BB_HASHSERVE = ") {
		t.Errorf("both hash-equivalence lines must appear once hashserv is configured:\n%s", out.LocalConf)
	}

	if lines := netrcLines(out.Netrc); len(lines) != 2 {
		t.Fatalf("netrc = %d lines, want 2 (hostname-keyed + full-URL-keyed):\n%s", len(lines), out.Netrc)
	}
}
