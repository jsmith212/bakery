package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jsmith212/bakery/internal/auth"
)

// TestEndToEndObjectBrowserUnconfiguredBackend404s pins CLAUDE.md's invariant
// ("a project/kind with no cache_backends row 404s") for B3 specifically: this
// endpoint resolves {kind} through backendOf, the same lookup GET
// .../backends/{kind} itself uses.
func TestEndToEndObjectBrowserUnconfiguredBackend404s(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	status, _ := h.req(http.MethodGet, base+"/backends/oci/objects", "", nil)
	if status != http.StatusNotFound {
		t.Errorf("objects on an unconfigured oci backend: status = %d, want 404", status)
	}
}

// TestEndToEndObjectBrowserPrefixAndClamp covers the prefix range scan and the
// limit clamp (never a 422 for an over-large ?limit=).
func TestEndToEndObjectBrowserPrefixAndClamp(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	status, _ := h.req(http.MethodPost, base+"/backends", `{"kind":"sstate"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create backend: status = %d", status)
	}

	backendID := h.backendID(auth.DevOrgSlug, auth.DevProjectSlug, "sstate")

	for _, k := range []string{"aaa-one", "aaa-two", "bbb-one", "ccc-one"} {
		h.putObject(backendID, "", k, []byte("payload-"+k))
	}

	status, body := h.req(http.MethodGet, base+"/backends/sstate/objects?prefix=aaa-", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list objects: status = %d, body %s", status, body)
	}

	var page CacheObjectList
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(page.Items) != 2 {
		t.Fatalf("prefix=aaa- returned %d items, want 2 (%+v)", len(page.Items), page.Items)
	}

	for _, it := range page.Items {
		if it.Key != "aaa-one" && it.Key != "aaa-two" {
			t.Errorf("prefix=aaa- returned key %q, which does not start with the prefix", it.Key)
		}

		if it.Digest == "" || len(it.Digest) != 64 {
			t.Errorf("digest = %q, want 64 lowercase hex chars", it.Digest)
		}

		if it.SizeBytes == 0 {
			t.Errorf("size_bytes = 0 for key %q", it.Key)
		}
	}

	// A limit far above the ceiling is CLAMPED, never a 422 -- the gc.go:155-173
	// convention every new list endpoint in this wave copies.
	status, body = h.req(http.MethodGet, base+"/backends/sstate/objects?limit=999999", "", nil)
	if status != http.StatusOK {
		t.Errorf("an over-large ?limit= was rejected: status = %d, want 200 (body %s)", status, body)
	}

	// limit=0 IS refused: it is not a page size, it is nonsense.
	status, _ = h.req(http.MethodGet, base+"/backends/sstate/objects?limit=0", "", nil)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("?limit=0: status = %d, want 422", status)
	}
}

// TestEndToEndObjectBrowserKeysetStableUnderConcurrentInsert is the risk-#7
// gate: a KEYSET cursor must not repeat or skip a row when a write lands
// between two page requests, which an OFFSET cursor would.
func TestEndToEndObjectBrowserKeysetStableUnderConcurrentInsert(t *testing.T) {
	h := newHarness(t)
	h.devLogin()

	base := Prefix + "/orgs/" + auth.DevOrgSlug + "/projects/" + auth.DevProjectSlug

	status, _ := h.req(http.MethodPost, base+"/backends", `{"kind":"sstate"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create backend: status = %d", status)
	}

	backendID := h.backendID(auth.DevOrgSlug, auth.DevProjectSlug, "sstate")

	for _, k := range []string{"aaa-one", "aaa-two", "bbb-one", "ccc-one"} {
		h.putObject(backendID, "", k, []byte(k))
	}

	// Page 1: the first two keys.
	status, body := h.req(http.MethodGet, base+"/backends/sstate/objects?limit=2", "", nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: status = %d, body %s", status, body)
	}

	var page1 CacheObjectList
	if err := json.Unmarshal(body, &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}

	if len(page1.Items) != 2 || page1.Items[0].Key != "aaa-one" || page1.Items[1].Key != "aaa-two" {
		t.Fatalf("page 1 = %+v, want [aaa-one aaa-two]", page1.Items)
	}

	if page1.NextCursor == nil || *page1.NextCursor != "aaa-two" {
		t.Fatalf("page 1 next_cursor = %v, want aaa-two", page1.NextCursor)
	}

	// CONCURRENT WRITE, between the two page requests: a key that sorts BEFORE
	// the cursor (as if it had always been there, on page 1) and one that sorts
	// into page 2's range.
	h.putObject(backendID, "", "aaa-0", []byte("would-have-been-on-page-1"))
	h.putObject(backendID, "", "bbb-two", []byte("lands-on-page-2"))

	status, body = h.req(http.MethodGet,
		base+"/backends/sstate/objects?limit=2&after_key="+*page1.NextCursor, "", nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: status = %d, body %s", status, body)
	}

	var page2 CacheObjectList
	if err := json.Unmarshal(body, &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}

	for _, it := range page2.Items {
		if it.Key == "aaa-0" {
			t.Error("page 2 contains aaa-0, a key that sorts BEFORE the cursor -- an OFFSET " +
				"cursor would have shown it here (shifted in by the concurrent insert); a " +
				"KEYSET cursor must not")
		}

		if it.Key == "aaa-one" || it.Key == "aaa-two" {
			t.Errorf("page 2 repeats %q, already returned on page 1", it.Key)
		}
	}

	want := map[string]bool{"bbb-one": true, "bbb-two": true}

	got := map[string]bool{}
	for _, it := range page2.Items {
		got[it.Key] = true
	}

	if len(page2.Items) < 2 || !got["bbb-one"] || !got["bbb-two"] {
		t.Errorf("page 2 = %+v, want to include %v", page2.Items, want)
	}
}
