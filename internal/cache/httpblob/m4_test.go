package httpblob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// ---------------------------------------------------------------------------
// classify tests -- no DB, pure functions.
// ---------------------------------------------------------------------------

// TestClassifyCAS pins the STRICT /cas validator: a content address is exactly 64
// lowercase hex, so verifyCASKey's storage.ParseKey can never fail on a routed request.
func TestClassifyCAS(t *testing.T) {
	t.Parallel()

	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "64 lowercase hex", in: hex64},

		{name: "empty", in: "", wantErr: true},
		{name: "63 hex (too short)", in: hex64[:63], wantErr: true},
		{name: "65 hex (too long)", in: hex64 + "a", wantErr: true},
		{name: "uppercase hex is not a canonical address", in: strings.ToUpper(hex64), wantErr: true},
		{name: "non-hex char", in: strings.Repeat("g", 64), wantErr: true},
		{name: "traversal", in: "../../../../etc/passwd", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind, key, err := classifyCAS(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("classifyCAS(%q) err = nil, want error", tt.in)
				}

				return
			}

			if err != nil {
				t.Fatalf("classifyCAS(%q) err = %v, want nil", tt.in, err)
			}

			if kind != "cas" || key != tt.in {
				t.Errorf("classifyCAS(%q) = (%q, %q), want (cas, %q)", tt.in, kind, key, tt.in)
			}

			// The strict shape is exactly what makes verifyCASKey total.
			if _, verr := verifyCASKey(key); verr != nil {
				t.Errorf("verifyCASKey(%q) err = %v; classifyCAS must guarantee ParseKey cannot fail", key, verr)
			}
		})
	}
}

// TestClassifyAC pins the LENIENT /ac validator: it accepts ccache's padded key, Bazel's
// and moon's 64-hex, and NEVER requires 64 hex -- but still rejects traversal, because
// hex-only inherently excludes '/', '.' and '\'.
func TestClassifyAC(t *testing.T) {
	t.Parallel()

	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// ccache @layout=bazel: FMT("ac/{}{:.{}}", hex40, hex40, 64-40) -- a 40-hex BLAKE3-160
	// with its own first 24 chars appended, so a 64-char key that is NOT a sha256.
	const hex40 = "0123456789abcdef0123456789abcdef01234567"
	ccachePadded := hex40 + hex40[:24]

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "bazel/moon 64-hex sha256", in: hex64},
		{name: "ccache 40->64 padded key", in: ccachePadded},
		{name: "bare 40-hex", in: hex40},

		{name: "empty", in: "", wantErr: true},
		{name: "traversal is not hex", in: "../../etc/passwd", wantErr: true},
		{name: "decoded slash is not hex", in: "ab/cd", wantErr: true},
		{name: "dot is not hex", in: "..", wantErr: true},
		{name: "over the length bound", in: strings.Repeat("a", maxACKeyLen+1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind, key, err := classifyAC(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("classifyAC(%q) err = nil, want error", tt.in)
				}

				return
			}

			if err != nil {
				t.Fatalf("classifyAC(%q) err = %v, want nil", tt.in, err)
			}

			if kind != "ac" || key != tt.in {
				t.Errorf("classifyAC(%q) = (%q, %q), want (ac, %q)", tt.in, kind, key, tt.in)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WebDAV + auth tests -- no DB (they never reach a blob write).
// ---------------------------------------------------------------------------

// bazelRoute is a resolved, enabled bazel route (the kind that backs /ac, /cas and
// sccache) with fixed nonzero IDs for the authorization comparisons.
func bazelRoute() cache.Route {
	r := testRoute()
	r.Kind = repository.BackendKindBazel

	return r
}

// TestPropfindDeclaresACollection: PROPFIND -> 207 with the two fields opendal cannot
// deserialize without -- <D:collection/> and an RFC 2822 <D:getlastmodified>. Missing
// either makes sccache go silently read-only.
func TestPropfindDeclaresACollection(t *testing.T) {
	t.Parallel()

	blobs := newTestBlobs(t)

	mux := http.NewServeMux()
	NewSccache(blobs.deps(), fakeResolver{route: bazelRoute(), found: true}, &fakeAuthenticator{}).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(methodPropfind, "/cache/acme/widget/sccache/a/b/c", nil))

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, want 207", rec.Code)
	}

	body := rec.Body.String()

	if !strings.Contains(body, "<D:resourcetype><D:collection/></D:resourcetype>") {
		t.Errorf("PROPFIND body missing the collection resourcetype:\n%s", body)
	}

	// opendal parses getlastmodified as an RFC 2822 date; net/mail.ParseDate is the same
	// grammar, so it is the honest assertion.
	got := between(body, "<D:getlastmodified>", "</D:getlastmodified>")
	if got == "" {
		t.Fatalf("PROPFIND body missing getlastmodified:\n%s", body)
	}

	if _, err := mail.ParseDate(got); err != nil {
		t.Errorf("getlastmodified = %q does not parse as RFC 2822: %v", got, err)
	}
}

// TestMkcolIsCreated: MKCOL is a write and returns 201. Any non-2xx latches sccache off.
func TestMkcolIsCreated(t *testing.T) {
	t.Parallel()

	blobs := newTestBlobs(t)
	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true}}

	mux := http.NewServeMux()
	NewSccache(blobs.deps(), fakeResolver{route: bazelRoute(), found: true}, authn).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(methodMkcol, "/cache/acme/widget/sccache/a/b/c", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d, want 201", rec.Code)
	}
}

// TestCASWriteRequiresAWriteKey: even before the body is read, an anonymous /cas write is
// 401 + WWW-Authenticate and a read-scoped key is 403 -- the cache-poisoning defense,
// identical to sstate, on the verifying namespace.
func TestCASWriteRequiresAWriteKey(t *testing.T) {
	t.Parallel()

	const key = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	t.Run("no credential is 401", func(t *testing.T) {
		t.Parallel()

		blobs := newTestBlobs(t)
		authn := &fakeAuthenticator{err: errUnauth{}}

		mux := http.NewServeMux()
		NewCAS(blobs.deps(), fakeResolver{route: bazelRoute(), found: true}, authn).Register(mux)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/cache/acme/widget/cas/"+key, strings.NewReader("x")))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous /cas PUT = %d, want 401", rec.Code)
		}

		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="bakery"` {
			t.Errorf("WWW-Authenticate = %q, want Basic realm=\"bakery\"", got)
		}
	})

	t.Run("read-scoped key is 403", func(t *testing.T) {
		t.Parallel()

		blobs := newTestBlobs(t)
		authn := &fakeAuthenticator{principal: fakePrincipal{canRead: true, canWrite: false}}

		mux := http.NewServeMux()
		NewCAS(blobs.deps(), fakeResolver{route: bazelRoute(), found: true}, authn).Register(mux)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/cache/acme/widget/cas/"+key, strings.NewReader("x")))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-scoped /cas PUT = %d, want 403", rec.Code)
		}
	})
}

// TestCASRejectsContentEncoding: httpblob hashes the WIRE bytes, so a compressed body
// would hash wrong and fail VerifyDigest on legitimate traffic. A non-identity
// Content-Encoding on the verifying path is an explicit 400, before the body is read.
func TestCASRejectsContentEncoding(t *testing.T) {
	t.Parallel()

	const key = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	blobs := newTestBlobs(t)
	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true}}

	mux := http.NewServeMux()
	NewCAS(blobs.deps(), fakeResolver{route: bazelRoute(), found: true}, authn).Register(mux)

	r := httptest.NewRequest(http.MethodPut, "/cache/acme/widget/cas/"+key, strings.NewReader("compressed"))
	r.Header.Set("Content-Encoding", "zstd")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zstd-encoded /cas PUT = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// DB-backed tests: /cas skip-ingest, /ac opacity, DELETE. These route through
// blob.Service.Put/Get/Delete, whose refcount protocol lives in Postgres.
// ---------------------------------------------------------------------------

// newBazelFixture stands up a bazel-kind backend over a REAL migrated Postgres and a REAL
// local byte store, mounted with mk (NewAC/NewCAS/NewSccache). It mirrors newPutFixture
// but with the bazel kind so the cache_objects FK to cache_backends resolves.
func newBazelFixture(
	t *testing.T, authn Authenticator, mk func(cache.Deps, RouteResolver, Authenticator) *Backend,
) *putFixture {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	m := metrics.New()
	bytesStore := storage.NewInstrumented(local, m, metrics.DriverLocal)

	svc, err := blob.New(blob.Config{Reader: store, Tx: store, Storage: bytesStore, Metrics: m})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	ctx := t.Context()

	org, err := store.CreateOrganization(ctx, repository.CreateOrganizationParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	project, err := store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	backend, err := store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        project.ID,
		Kind:             repository.BackendKindBazel,
		Enabled:          true,
		ReadAuthRequired: false,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	route := cache.Route{
		OrgID:            org.ID,
		ProjectID:        project.ID,
		Org:              "acme",
		Project:          "widget",
		BackendID:        backend.ID,
		Kind:             repository.BackendKindBazel,
		Enabled:          true,
		ReadAuthRequired: false,
	}

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	mk(deps, fakeResolver{route: route, found: true}, authn).Register(mux)

	return &putFixture{handler: mux, route: route, svc: svc}
}

// countingReader records how many bytes were read out of it, so a test can prove the
// server DRAINED the body (or, under Expect: 100-continue, did not).
type countingReader struct {
	data []byte
	pos  int
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}

	n := copy(p, c.data[c.pos:])
	c.pos += n
	c.read += n

	return n, nil
}

// TestCASSkipIngestKeepsTheDrain is the redundant-PUT-storm mitigation:
//
//   - the FIRST PUT of a /cas key -> 201 (full ingest);
//   - a re-PUT of the SAME key -> 200 idempotent no-op, and the body is STILL DRAINED
//     (an early 200 without draining EPIPEs a naive client);
//   - a re-PUT with Expect: 100-continue -> 200 and the body is NOT read (Go answers the
//     expectation with the final status; the body is never sent).
func TestCASSkipIngestKeepsTheDrain(t *testing.T) {
	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true}}
	f := newBazelFixture(t, authn, NewCAS)

	body := bytes.Repeat([]byte("cas-blob-payload"), 4096) // 64 KiB
	sum := sha256.Sum256(body)
	key := hex.EncodeToString(sum[:])
	path := "/cache/acme/widget/cas/" + key

	// First PUT: full ingest, 201.
	if rec := putRaw(t, f.handler, path, bytes.NewReader(body), nil); rec.Code != http.StatusCreated {
		t.Fatalf("first /cas PUT = %d, want 201", rec.Code)
	}

	// Re-PUT: the key is present (warm in the LRU from the create), so the skip path runs.
	// It must still be a 200 AND it must drain the whole body.
	cr := &countingReader{data: body}
	if rec := putRaw(t, f.handler, path, cr, nil); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT = %d, want 200 (idempotent no-op)", rec.Code)
	}

	if cr.read != len(body) {
		t.Errorf("re-PUT drained %d of %d body bytes; a partial drain EPIPEs the client", cr.read, len(body))
	}

	// Re-PUT with Expect: 100-continue: the body is never sent, so the skip path must NOT
	// read it.
	cr2 := &countingReader{data: body}
	if rec := putRaw(t, f.handler, path, cr2, http.Header{"Expect": {"100-continue"}}); rec.Code != http.StatusOK {
		t.Fatalf("100-continue re-PUT = %d, want 200", rec.Code)
	}

	if cr2.read != 0 {
		t.Errorf("100-continue re-PUT read %d body bytes; it must skip the drain", cr2.read)
	}
}

// TestACRoundTripsOpaquely: /ac is an OPAQUE byte store. A body that does NOT hash to its
// key round-trips verbatim through PUT then GET -- which proves no digest verification and
// no re-serialization. If /ac verified, the PUT would 400.
func TestACRoundTripsOpaquely(t *testing.T) {
	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true, canRead: true}}
	f := newBazelFixture(t, authn, NewAC)

	// A key that is plainly NOT sha256(body): a ccache-shaped 64-hex, and a body of raw
	// bytes (a stand-in for a ccache CacheEntry / moon Manifest JSON).
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	body := []byte("\x00\x01\x02not-an-action-result{\"manifest\":true}\xff\xfe")
	path := "/cache/acme/widget/ac/" + key

	if rec := putRaw(t, f.handler, path, bytes.NewReader(body), nil); rec.Code != http.StatusCreated {
		t.Fatalf("/ac PUT = %d, want 201 (opaque, never verified)", rec.Code)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/ac GET = %d, want 200", rec.Code)
	}

	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("/ac GET body = %q, want the verbatim %q", rec.Body.Bytes(), body)
	}
}

// TestDeleteIs204: DELETE on /ac -> 204, present or absent. ccache treats any non-2xx
// (including the 405 an unregistered DELETE returns) as a hard failure that latches its
// whole backend off.
func TestDeleteIs204(t *testing.T) {
	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true, canRead: true}}
	f := newBazelFixture(t, authn, NewAC)

	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	path := "/cache/acme/widget/ac/" + key

	// Absent key: still 204, never 404 -- ccache would latch off on anything else.
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of absent key = %d, want 204", rec.Code)
	}

	// Now create it and delete it for real.
	if r := putRaw(t, f.handler, path, strings.NewReader("entry"), nil); r.Code != http.StatusCreated {
		t.Fatalf("/ac PUT = %d, want 201", r.Code)
	}

	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of present key = %d, want 204", rec.Code)
	}

	// And it is gone: a follow-up GET misses.
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}
}

// putRaw issues a PUT of body to path with optional extra headers.
func putRaw(t *testing.T, h http.Handler, path string, body io.Reader, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPut, path, body)
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	return rec
}

// between returns the substring of s between the first openTag and the following
// closeTag, or "" if either marker is absent.
func between(s, openTag, closeTag string) string {
	i := strings.Index(s, openTag)
	if i < 0 {
		return ""
	}

	i += len(openTag)

	j := strings.Index(s[i:], closeTag)
	if j < 0 {
		return ""
	}

	return s[i : i+j]
}
