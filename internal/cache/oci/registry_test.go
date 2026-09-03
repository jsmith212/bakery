package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// The writable namespace, as a client spells it, in both route families. Every
// behavioural test runs against both: they are two mounts into one handler set, and a
// divergence is a half-broken export (containerd works, BuildKit does not).
const (
	bcTenantPrefix = tenantPrefix + segBuildCache + "/"
	bcBuildkitPfx  = buildkitPfx + segBuildCache + "/"
)

func bcFamilies() []struct{ name, prefix string } {
	return []struct{ name, prefix string }{
		{name: "containerd+docker-engine", prefix: bcTenantPrefix},
		{name: "buildkit+podman", prefix: bcBuildkitPfx},
	}
}

// The two BuildKit cache manifest shapes, as bytes.
//
// THEY ARE HAND-WRITTEN WITH RAGGED WHITESPACE AND NON-ALPHABETICAL KEYS ON PURPOSE.
// The assertion these exist for is byte-identity, and a json.Marshal round trip
// anywhere in the store-and-serve path would reorder the keys and normalize the
// spacing -- which changes the digest and breaks Docker-Content-Digest for every
// client at once. Canonical-looking fixtures would not catch it.
const (
	// bcIndexManifest is the DEFAULT export shape: an OCI image INDEX whose manifests[]
	// entries are blob descriptors rather than manifest descriptors. That is what trips
	// validating registries; Bakery accepts it because it parses nothing.
	bcIndexManifest = `{"schemaVersion":2, "mediaType":"application/vnd.oci.image.index.v1+json",
  "manifests":[
    {"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":1234,` +
		`"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111",` +
		`"annotations":{"buildkit/createdat":"2026-09-03T00:00:00Z"}},
    {"mediaType":"application/vnd.buildkit.cacheconfig.v0","size":57,` +
		`"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}
  ]}`

	// bcImageManifest is the image-manifest=true shape: an OCI image MANIFEST whose
	// CONFIG media type is application/vnd.buildkit.cacheconfig.v0. That is what trips
	// config-allowlisting registries.
	bcImageManifest = `{"schemaVersion":2, "mediaType":"application/vnd.oci.image.manifest.v1+json",
  "config":{"mediaType":"application/vnd.buildkit.cacheconfig.v0","size":57,` +
		`"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"},
  "layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":1234,` +
		`"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"}]}`

	bcIndexType = "application/vnd.oci.image.index.v1+json"
	bcImageType = "application/vnd.oci.image.manifest.v1+json"
)

// bcManifestShapes is the table every round-trip test runs twice over.
func bcManifestShapes() []struct{ name, body, mediaType string } {
	return []struct{ name, body, mediaType string }{
		{name: "index (default export)", body: bcIndexManifest, mediaType: bcIndexType},
		{name: "image-manifest=true", body: bcImageManifest, mediaType: bcImageType},
	}
}

// testRegistryRoute is a resolved, enabled, OPEN registry route. Its BackendID differs
// from the mirror's so the two namespaces are visibly distinct rows.
func testRegistryRoute() cache.Route {
	return cache.Route{
		OrgID:            testUUID(0x0a),
		ProjectID:        testUUID(0x1a),
		Org:              "acme",
		Project:          "widget",
		BackendID:        11,
		Kind:             repository.BackendKindRegistry,
		Enabled:          true,
		ReadAuthRequired: false,
		Config:           nil, // the kind's config jsonb is empty in v1: there is no upstream
	}
}

// fakeKindResolver answers per KIND, which is what lets one mux carry the mirror and
// the writable namespace at once -- and therefore what lets a test prove that a
// buildcache URL reaches the buildcache handler rather than the mirror's {rest...}.
type fakeKindResolver struct {
	mu     sync.Mutex
	routes map[repository.BackendKind]cache.Route
}

func (f *fakeKindResolver) Resolve(
	_ context.Context, _, _ string, kind repository.BackendKind,
) (cache.Route, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	route, ok := f.routes[kind]

	return route, ok
}

func (f *fakeKindResolver) set(kind repository.BackendKind, route cache.Route) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.routes[kind] = route
}

func (f *fakeKindResolver) drop(kind repository.BackendKind) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.routes, kind)
}

// fakeKeyring resolves a fixed set of bkry_ tokens to principals and RECORDS every
// token it is asked about. That record is the assertion in the foreign-credential
// test: the shape gate must mean a forwarded Docker Hub PAT never arrives here at all.
type fakeKeyring struct {
	mu   sync.Mutex
	keys map[string]fakePrincipal
	seen []string
}

func (a *fakeKeyring) AuthenticateToken(_ context.Context, token string) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.seen = append(a.seen, token)

	p, ok := a.keys[token]
	if !ok {
		return nil, errors.New("no such key")
	}

	return p, nil
}

func (a *fakeKeyring) tokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.seen...)
}

// The two credentials the auth matrix is written against.
const (
	bcWriteKey = "bkry_write_scoped_key"
	bcReadKey  = "bkry_read_only_key"
)

func newKeyring() *fakeKeyring {
	return &fakeKeyring{
		mu: sync.Mutex{},
		keys: map[string]fakePrincipal{
			bcWriteKey: {canRead: true, canWrite: true},
			bcReadKey:  {canRead: true, canWrite: false},
		},
		seen: nil,
	}
}

// bcFixture mounts BOTH backends on ONE mux, over one blob.Service.
//
// The mirror is not scenery. It is registered with a counting fakeUpstream so that
// every buildcache test also asserts spec §8.4's anti-regression: zero upstream
// requests, ever, from the writable namespace. And because the mirror owns the
// `{rest...}` patterns these literal-segment patterns must beat, having it mounted is
// what makes the precedence assertions real rather than notional.
type bcFixture struct {
	backend  *BuildCache
	mirror   *Backend
	mux      *http.ServeMux
	reader   *fakeReader
	store    storage.Store
	authn    *fakeKeyring
	resolver *fakeKindResolver
	upstream *fakeUpstream

	clock time.Time
}

func newBCFixture(t *testing.T) *bcFixture {
	t.Helper()

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	reader := newFakeReader()
	m := metrics.New()

	svc, err := blob.New(blob.Config{Reader: reader, Tx: nil, Storage: local, Metrics: m, CacheSize: 0})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	resolver := &fakeKindResolver{mu: sync.Mutex{}, routes: map[repository.BackendKind]cache.Route{
		repository.BackendKindRegistry: testRegistryRoute(),
		repository.BackendKindOci:      testRoute(),
	}}

	up := newFakeUpstream(testIndex(t), testIndexType)
	authn := newKeyring()

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	cfg := Config{ExternalURL: "https://bakery.example.com", UpstreamAuth: nil}

	f := &bcFixture{
		backend: NewBuildCache(deps, resolver, authn, cfg, nil),
		mirror:  New(deps, resolver, authn, up, cfg),
		mux:     http.NewServeMux(),
		reader:  reader, store: local, authn: authn, resolver: resolver, upstream: up,
		clock: time.Unix(1_700_000_000, 0).UTC(),
	}

	f.mirror.Register(f.mux)
	f.backend.Register(f.mux)

	// The REST of the public mux, in server.newPublicHandler's shape, so "it registers
	// without panicking" is asserted against the patterns it actually has to live
	// beside -- the method-less /api/v1/ mount, the /cache/ and /v2/ catch-alls, and the
	// method-less SPA `/`, which is the documented ServeMux trap. The SPA answers 418
	// here so any test that reaches it says so loudly instead of looking like a hit.
	f.mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	f.mux.HandleFunc("/api/v1/", func(http.ResponseWriter, *http.Request) {})
	f.mux.Handle("/cache/", http.NotFoundHandler())
	f.mux.Handle("/v2/", http.NotFoundHandler())
	f.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	return f
}

func (f *bcFixture) seed(t *testing.T, namespace, key string, body []byte, ct string) storage.Key {
	t.Helper()

	return seedObject(t, f.store, f.reader, namespace, key, body, ct, f.clock)
}

// seedTag lands a manifest the way a completed push would: the bytes once, a
// `manifests` row keyed on the digest, and a `tags` row keyed "<repo>:<tag>" naming
// the same blob.
func (f *bcFixture) seedTag(t *testing.T, repo, tag string, raw []byte, ct string) storage.Key {
	t.Helper()

	digest := storage.KeyOf(raw)

	f.seed(t, nsManifests, digest.String(), raw, ct)
	f.seed(t, nsTags, buildCacheTagKey(repo, tag), raw, ct)

	return digest
}

func (f *bcFixture) do(
	method, target string, headers map[string]string, body []byte,
) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}

	r := httptest.NewRequest(method, target, rdr)
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)

	return w
}

// bearer is the Authorization header for a bkry_ token.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// assertNoUpstream is the anti-regression assertion every buildcache test carries: the
// writable namespace has no Fetcher at all, so the mirror's counting fake -- mounted on
// the same mux, for the same project -- must be untouched.
func (f *bcFixture) assertNoUpstream(t *testing.T) {
	t.Helper()

	resolves, manifests, stats, gets := f.upstream.counts()
	if resolves+manifests+stats+gets != 0 {
		t.Errorf("the writable namespace reached an upstream (resolve=%d manifest=%d stat=%d get=%d); "+
			"it is constructed without a Fetcher and must be incapable of it",
			resolves, manifests, stats, gets)
	}
}

// TestBuildCacheRoutesBeatTheMirror is the routing gate.
//
// The literal `buildcache` segment must win over the mirror's `{rest...}` -- ServeMux
// prefers the pattern whose matches are a strict subset -- and registering the two
// families beside the mirror's, the token endpoints, the method-less /api/v1/ mount and
// the method-less SPA catch-all must not panic. Both halves are startup failures, not
// runtime ones: a panic never reaches a test that only exercises handlers, and a
// precedence loss silently sends every export to the read-only mirror, which answers
// 404 UNSUPPORTED and fails the build.
func TestBuildCacheRoutesBeatTheMirror(t *testing.T) {
	t.Parallel()

	f := newBCFixture(t) // registers; a conflict would panic here

	raw := []byte(bcIndexManifest)
	f.seedTag(t, "impulse", "main", raw, bcIndexType)

	for _, fam := range bcFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			w := f.do(http.MethodGet, fam.prefix+"impulse/manifests/main", nil, nil)

			// A 200 with these bytes can ONLY have come from the buildcache handler: the
			// mirror would have parsed the repository as "buildcache/impulse", looked in
			// its own host-prefixed tag key space, missed, and 404ed.
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 -- the mirror's {rest...} stole the route",
					fam.prefix, w.Code)
			}

			if !bytes.Equal(w.Body.Bytes(), raw) {
				t.Error("served bytes are not the seeded manifest")
			}

			// The write verbs must be registered too: without the POST pattern this is the
			// mirror's unregistered method (405), not the write gate's 401.
			post := f.do(http.MethodPost, fam.prefix+"impulse/blobs/uploads/", nil, nil)
			if post.Code != http.StatusUnauthorized {
				t.Errorf("POST uploads (anonymous) = %d, want 401 -- the POST pattern is missing", post.Code)
			}

			put := f.do(http.MethodPut, fam.prefix+"impulse/manifests/main", nil, raw)
			if put.Code != http.StatusUnauthorized {
				t.Errorf("PUT manifest (anonymous) = %d, want 401 -- the PUT pattern is missing", put.Code)
			}
		})
	}

	// And the mirror still owns everything that is NOT under buildcache/.
	if w := f.do(http.MethodGet, buildkitPfx+"library/alpine/manifests/3.20", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("mirror pull = %d, want 404 -- the buildcache patterns must not shadow the mirror", w.Code)
	}

	assertOCIError(t, f.do(http.MethodGet, buildkitPfx+"library/alpine/manifests/3.20", nil, nil).
		Body.Bytes(), codeManifestUnknown)

	f.assertNoUpstream(t)
}

// TestBuildCacheHeadAnswersTheExactStoredDigest.
//
// containerd's push HEADs manifests/<tag> first and skips the PUT entirely when the
// response's Docker-Content-Digest EXACTLY matches the descriptor it was about to
// push. Getting this wrong is not an error anywhere: it silently re-pushes every cache
// manifest on every build. Content-Length and the STORED Content-Type must ride the
// same response -- containerd assigns the latter straight into the descriptor's
// MediaType.
func TestBuildCacheHeadAnswersTheExactStoredDigest(t *testing.T) {
	t.Parallel()

	for _, shape := range bcManifestShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			f := newBCFixture(t)
			raw := []byte(shape.body)
			digest := f.seedTag(t, "impulse", "main", raw, shape.mediaType)

			w := f.do(http.MethodHead, bcBuildkitPfx+"impulse/manifests/main", nil, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("HEAD = %d, want 200", w.Code)
			}

			if got, want := w.Header().Get("Docker-Content-Digest"), "sha256:"+digest.String(); got != want {
				t.Errorf("Docker-Content-Digest = %q, want %q -- containerd compares this EXACTLY "+
					"and re-pushes on any difference", got, want)
			}

			if got, want := w.Header().Get("Content-Length"), strconv.Itoa(len(raw)); got != want {
				t.Errorf("Content-Length = %q, want %q", got, want)
			}

			if got := w.Header().Get("Content-Type"); got != shape.mediaType {
				t.Errorf("Content-Type = %q, want the STORED %q", got, shape.mediaType)
			}

			if w.Body.Len() != 0 {
				t.Errorf("HEAD returned a %d-byte body", w.Body.Len())
			}

			f.assertNoUpstream(t)
		})
	}
}

// TestBuildCacheServesManifestsAndBlobsVerbatim covers the import path: bytes back
// byte-for-byte, by tag and by digest, plus Range on a blob (BuildKit fetches layer
// blobs lazily and honours it).
func TestBuildCacheServesManifestsAndBlobsVerbatim(t *testing.T) {
	t.Parallel()

	f := newBCFixture(t)

	raw := []byte(bcIndexManifest)
	digest := f.seedTag(t, "impulse", "main", raw, bcIndexType)

	layer := []byte("a cache layer's worth of bytes, near enough for a range request")
	layerDigest := f.seed(t, nsBlobs, storage.KeyOf(layer).String(), layer, "")

	for _, fam := range bcFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			byTag := f.do(http.MethodGet, fam.prefix+"impulse/manifests/main", nil, nil)
			if byTag.Code != http.StatusOK || !bytes.Equal(byTag.Body.Bytes(), raw) {
				t.Fatalf("GET by tag = %d, bytes equal = %v", byTag.Code, bytes.Equal(byTag.Body.Bytes(), raw))
			}

			byDigest := f.do(http.MethodGet,
				fam.prefix+"impulse/manifests/sha256:"+digest.String(), nil, nil)
			if byDigest.Code != http.StatusOK || !bytes.Equal(byDigest.Body.Bytes(), raw) {
				t.Fatalf("GET by digest = %d, bytes equal = %v",
					byDigest.Code, bytes.Equal(byDigest.Body.Bytes(), raw))
			}

			blobGet := f.do(http.MethodGet,
				fam.prefix+"impulse/blobs/sha256:"+layerDigest.String(), nil, nil)
			if blobGet.Code != http.StatusOK || !bytes.Equal(blobGet.Body.Bytes(), layer) {
				t.Fatalf("GET blob = %d", blobGet.Code)
			}

			if got := blobGet.Header().Get("Docker-Content-Digest"); got != "sha256:"+layerDigest.String() {
				t.Errorf("blob Docker-Content-Digest = %q", got)
			}

			ranged := f.do(http.MethodGet, fam.prefix+"impulse/blobs/sha256:"+layerDigest.String(),
				map[string]string{"Range": "bytes=0-4"}, nil)
			if ranged.Code != http.StatusPartialContent {
				t.Fatalf("ranged blob GET = %d, want 206", ranged.Code)
			}

			if got := ranged.Body.String(); got != string(layer[:5]) {
				t.Errorf("ranged body = %q, want %q", got, layer[:5])
			}
		})
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheMissIsACleanFourOhFourAndNeverAnUpstream.
//
// There is no Fetcher on this backend, so a miss cannot be anything but a 404 -- and
// that is spec §8.4's anti-regression, asserted against a REAL counting upstream fake
// mounted on the mirror of the same project. BuildKit's import failure is
// unconditionally soft (a debug log and a cold build), so 404 is exactly the answer a
// never-yet-exported project should give.
func TestBuildCacheMissIsACleanFourOhFourAndNeverAnUpstream(t *testing.T) {
	t.Parallel()

	f := newBCFixture(t)

	tests := []struct {
		name     string
		method   string
		tail     string
		wantCode string
	}{
		{name: "tag", method: http.MethodGet, tail: "impulse/manifests/main", wantCode: codeManifestUnknown},
		{name: "tag HEAD", method: http.MethodHead, tail: "impulse/manifests/main", wantCode: codeManifestUnknown},
		{
			name: "manifest by digest", method: http.MethodGet,
			tail:     "impulse/manifests/sha256:" + strings.Repeat("a", 64),
			wantCode: codeManifestUnknown,
		},
		{
			name: "blob", method: http.MethodGet,
			tail:     "impulse/blobs/sha256:" + strings.Repeat("b", 64),
			wantCode: codeBlobUnknown,
		},
		{
			// A legal OCI digest this store cannot address. On a READ a clean miss is
			// right; only a write turns it into a 400.
			name: "sha512 blob", method: http.MethodGet,
			tail:     "impulse/blobs/sha512:" + strings.Repeat("c", 64),
			wantCode: codeBlobUnknown,
		},
		{
			// Listing is out of scope for v1; 404 is what distribution's own tag store
			// answers for a repository it holds nothing for.
			name: "tags list", method: http.MethodGet,
			tail: "impulse/tags/list", wantCode: codeNameUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := f.do(tt.method, bcBuildkitPfx+tt.tail, bearer(bcWriteKey), nil)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404", tt.method, tt.tail, w.Code)
			}

			if tt.method != http.MethodHead {
				assertOCIError(t, w.Body.Bytes(), tt.wantCode)
			}
		})
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheAuthMatrix is spec §4 in one table.
//
// Reads follow the row's ReadAuthRequired exactly like the mirror. EVERY write verb
// requires a write-scoped key regardless -- the standing cache invariant, no
// representable unauthenticated-write state -- and the two denials are different codes
// for different reasons (see core.authorizeWrite).
func TestBuildCacheAuthMatrix(t *testing.T) {
	t.Parallel()

	// The completing PUT is deliberately absent from this table: its denials are the
	// same gate, and its ACCEPT path writes bytes, which needs a real database (see
	// registry_push_test.go). POST is the write verb that proves acceptance here.
	tests := []struct {
		name             string
		readAuthRequired bool
		method           string
		tail             string
		body             []byte
		token            string
		wantStatus       int
		wantCode         string
		wantChallenge    bool
	}{
		{
			name: "open backend serves an anonymous read", readAuthRequired: false,
			method: http.MethodGet, tail: "impulse/manifests/main", body: nil, token: "",
			wantStatus: http.StatusOK, wantCode: "", wantChallenge: false,
		},
		{
			name: "closed backend challenges an anonymous read", readAuthRequired: true,
			method: http.MethodGet, tail: "impulse/manifests/main", body: nil, token: "",
			wantStatus: http.StatusUnauthorized, wantCode: codeUnauthorized, wantChallenge: true,
		},
		{
			name: "closed backend admits a read key", readAuthRequired: true,
			method: http.MethodGet, tail: "impulse/manifests/main", body: nil, token: bcReadKey,
			wantStatus: http.StatusOK, wantCode: "", wantChallenge: false,
		},
		{
			name: "anonymous upload start is 401 with the challenge", readAuthRequired: false,
			method: http.MethodPost, tail: "impulse/blobs/uploads/", body: nil, token: "",
			wantStatus: http.StatusUnauthorized, wantCode: codeUnauthorized, wantChallenge: true,
		},
		{
			name: "anonymous manifest push is 401 with the challenge", readAuthRequired: false,
			method: http.MethodPut, tail: "impulse/manifests/main",
			body: []byte(bcIndexManifest), token: "",
			wantStatus: http.StatusUnauthorized, wantCode: codeUnauthorized, wantChallenge: true,
		},
		{
			name: "a read-only key may not start an upload", readAuthRequired: false,
			method: http.MethodPost, tail: "impulse/blobs/uploads/", body: nil, token: bcReadKey,
			wantStatus: http.StatusForbidden, wantCode: codeDenied, wantChallenge: false,
		},
		{
			name: "a read-only key may not push a manifest", readAuthRequired: false,
			method: http.MethodPut, tail: "impulse/manifests/main",
			body: []byte(bcIndexManifest), token: bcReadKey,
			wantStatus: http.StatusForbidden, wantCode: codeDenied, wantChallenge: false,
		},
		{
			name: "a write key starts an upload", readAuthRequired: false,
			method: http.MethodPost, tail: "impulse/blobs/uploads/", body: nil, token: bcWriteKey,
			wantStatus: http.StatusAccepted, wantCode: "", wantChallenge: false,
		},
		{
			// An open READ backend still refuses an anonymous write. This is the row that
			// would disappear if someone ever added a WriteAuthRequired knob.
			name: "an open backend still refuses an anonymous write", readAuthRequired: false,
			method: http.MethodPost, tail: "impulse/blobs/uploads/", body: nil, token: "",
			wantStatus: http.StatusUnauthorized, wantCode: codeUnauthorized, wantChallenge: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newBCFixture(t)
			f.seedTag(t, "impulse", "main", []byte(bcIndexManifest), bcIndexType)

			route := testRegistryRoute()
			route.ReadAuthRequired = tt.readAuthRequired
			f.resolver.set(repository.BackendKindRegistry, route)

			headers := map[string]string{}
			if tt.token != "" {
				headers = bearer(tt.token)
			}

			w := f.do(tt.method, bcBuildkitPfx+tt.tail, headers, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("%s = %d, want %d (body %q)", tt.method, w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantCode != "" {
				assertOCIError(t, w.Body.Bytes(), tt.wantCode)
			}

			challenge := w.Header().Get("WWW-Authenticate")
			if tt.wantChallenge && !strings.HasPrefix(challenge, "Bearer ") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge -- BuildKit's authorizer "+
					"only attempts auth against a host it has harvested one from", challenge)
			}

			if !tt.wantChallenge && challenge != "" {
				t.Errorf("WWW-Authenticate = %q, want none: a 403 that re-challenges makes a "+
					"read-only key retry the token dance forever", challenge)
			}

			f.assertNoUpstream(t)
		})
	}
}

// TestBuildCacheForeignCredentialIsNoCredentialOnAWrite.
//
// Docker Engine forwards the operator's REAL Docker Hub login to whatever
// registry-mirrors names, on every request, unscoped. The shape gate discards it
// before it can reach a database probe, an error metric or a log line -- so on a WRITE
// it is "no credential" (401 + challenge), not a rejected credential, and the
// authenticator must never have been asked about it.
func TestBuildCacheForeignCredentialIsNoCredentialOnAWrite(t *testing.T) {
	t.Parallel()

	f := newBCFixture(t)

	hubPAT := base64.StdEncoding.EncodeToString([]byte("dockerhubuser:dckr_pat_supersecret"))

	w := f.do(http.MethodPost, bcBuildkitPfx+"impulse/blobs/uploads/",
		map[string]string{"Authorization": "Basic " + hubPAT}, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", w.Code)
	}

	if seen := f.authn.tokens(); len(seen) != 0 {
		t.Errorf("the authenticator was asked about %d credential(s); a forwarded Docker Hub "+
			"PAT must be discarded by the shape gate before any probe", len(seen))
	}

	if !strings.Contains(w.Body.String(), codeUnauthorized) {
		t.Errorf("body = %q, want the UNAUTHORIZED envelope", w.Body.String())
	}
}

// TestBuildCacheUploadStartIsAlwaysAccepted covers the POST contract, including the
// cross-repo mount.
//
// containerd treats 200/202/204 identically ("upload started, parse Location") and
// only 201 short-circuits a mount, so answering 202 to a mount request is ALWAYS safe
// -- the client falls back to a normal upload and dedup elides the bytes anyway. The
// Location is relative and points at this backend's one stateless session.
func TestBuildCacheUploadStartIsAlwaysAccepted(t *testing.T) {
	t.Parallel()

	f := newBCFixture(t)

	for _, fam := range bcFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			for _, query := range []string{
				"",
				"?mount=sha256:" + strings.Repeat("d", 64) + "&from=other/repo",
			} {
				w := f.do(http.MethodPost, fam.prefix+"impulse/blobs/uploads/"+query,
					bearer(bcWriteKey), nil)

				if w.Code != http.StatusAccepted {
					t.Fatalf("POST %q = %d, want 202", query, w.Code)
				}

				want := fam.prefix + "impulse/blobs/uploads/" + uploadSession
				if got := w.Header().Get("Location"); got != want {
					t.Errorf("Location = %q, want the relative %q", got, want)
				}
			}
		})
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheUnregisteredVerbsAreRefused. containerd has no chunked (PATCH) upload
// at all and untagging is out of scope, so PATCH and DELETE are left UNREGISTERED --
// a truer answer than a handler that pretends.
//
// The refusal has two shapes and both are asserted, because only the second is what a
// client meets. On the backend's own patterns ServeMux answers its own 405. On the
// REAL public mux the method-less /cache/ and /v2/ catch-alls match first and answer
// 404 -- which is the point of those catch-alls: without them the method-less SPA `/`
// swallows the request and answers 200 + index.html, a poisoned "hit".
func TestBuildCacheUnregisteredVerbsAreRefused(t *testing.T) {
	t.Parallel()

	bare := http.NewServeMux()
	NewBuildCache(cache.Deps{Blobs: nil, Metrics: nil, Logger: nil}, nil, nil, Config{}, nil).Register(bare)

	f := newBCFixture(t)

	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		r := httptest.NewRequest(method, bcBuildkitPfx+"impulse/blobs/uploads/"+uploadSession, nil)
		w := httptest.NewRecorder()
		bare.ServeHTTP(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s on the backend's own patterns = %d, want ServeMux's 405", method, w.Code)
		}

		full := f.do(method, bcBuildkitPfx+"impulse/blobs/uploads/"+uploadSession,
			bearer(bcWriteKey), []byte("chunk"))

		if full.Code != http.StatusNotFound {
			t.Errorf("%s on the public mux = %d, want 404 from the /v2/ catch-all "+
				"(418 means the SPA swallowed it)", method, full.Code)
		}
	}
}

// TestBuildCacheUnconfiguredProject404sEveryVerb. An absent or disabled `registry` row
// 404s the WHOLE buildcache subtree, to everyone, before any authentication -- the
// standing "never mount what you cannot serve" invariant, and what stops a 401 from
// telling a scanner which projects exist.
func TestBuildCacheUnconfiguredProject404sEveryVerb(t *testing.T) {
	t.Parallel()

	states := []struct {
		name  string
		apply func(f *bcFixture)
	}{
		{name: "no registry row", apply: func(f *bcFixture) {
			f.resolver.drop(repository.BackendKindRegistry)
		}},
		{name: "disabled registry row", apply: func(f *bcFixture) {
			route := testRegistryRoute()
			route.Enabled = false
			f.resolver.set(repository.BackendKindRegistry, route)
		}},
	}

	verbs := []struct {
		method string
		tail   string
		body   []byte
	}{
		{method: http.MethodGet, tail: "impulse/manifests/main", body: nil},
		{method: http.MethodHead, tail: "impulse/manifests/main", body: nil},
		{method: http.MethodPost, tail: "impulse/blobs/uploads/", body: nil},
		{
			method: http.MethodPut, tail: "impulse/blobs/uploads/u?digest=sha256:" + strings.Repeat("e", 64),
			body: []byte("bytes"),
		},
		{method: http.MethodPut, tail: "impulse/manifests/main", body: []byte(bcIndexManifest)},
	}

	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			t.Parallel()

			f := newBCFixture(t)
			// Seeded so a 404 cannot be "there was nothing there anyway".
			f.seedTag(t, "impulse", "main", []byte(bcIndexManifest), bcIndexType)
			state.apply(f)

			for _, v := range verbs {
				w := f.do(v.method, bcBuildkitPfx+v.tail, bearer(bcWriteKey), v.body)
				if w.Code != http.StatusNotFound {
					t.Errorf("%s %s = %d, want 404 (body %q)", v.method, v.tail, w.Code, w.Body.String())
				}
			}

			f.assertNoUpstream(t)
		})
	}
}

// TestSplitUpload pins the push-path parser. It scans right to left for the same
// reason splitRef does -- a repository name may legally contain the marker words --
// and it must not mistake a blob named `uploadsfoo` for an upload session.
func TestSplitUpload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		rest        string
		wantName    string
		wantSession string
		ok          bool
	}{
		{name: "start", rest: "impulse/blobs/uploads/", wantName: "impulse", wantSession: "", ok: true},
		{
			name: "start without a trailing slash", rest: "impulse/blobs/uploads",
			wantName: "impulse", wantSession: "", ok: true,
		},
		{
			name: "completion", rest: "impulse/blobs/uploads/u",
			wantName: "impulse", wantSession: "u", ok: true,
		},
		{
			name: "namespaced repository", rest: "team/infra/impulse/blobs/uploads/u",
			wantName: "team/infra/impulse", wantSession: "u", ok: true,
		},
		{
			// The pathological case, same shape as splitRef's: only the RIGHTMOST marker
			// is the real separator.
			name: "repository literally named x/blobs/uploads", rest: "acme/blobs/uploads/app/blobs/uploads/u",
			wantName: "acme/blobs/uploads/app", wantSession: "u", ok: true,
		},
		{name: "leading slash is tolerated", rest: "/impulse/blobs/uploads/", wantName: "impulse", ok: true},
		{name: "a blob named uploadsfoo is not a session", rest: "impulse/blobs/uploadsfoo", ok: false},
		{name: "a session may not contain a slash", rest: "impulse/blobs/uploads/a/b", ok: false},
		{name: "empty repository name", rest: "blobs/uploads/", ok: false},
		{name: "a plain manifest reference", rest: "impulse/manifests/main", ok: false},
		{name: "empty", rest: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, session, ok := splitUpload(tt.rest)
			if ok != tt.ok {
				t.Fatalf("splitUpload(%q) ok = %v, want %v", tt.rest, ok, tt.ok)
			}

			if ok && (name != tt.wantName || session != tt.wantSession) {
				t.Errorf("splitUpload(%q) = (%q, %q), want (%q, %q)",
					tt.rest, name, session, tt.wantName, tt.wantSession)
			}
		})
	}
}

// TestBuildCacheShadowFallsThroughWithoutARegistryBackend pins spec §2's scope: the
// literal buildcache patterns shadow the mirror STATICALLY (ServeMux registration
// cannot be per-project), so on a project with NO registry backend serve() must hand
// reads back to the mirror -- an upstream repository legitimately named
// `buildcache/...` keeps working through the pull-through -- while writes never fall
// through. Without the fallback, adding this feature would have silently 404'd that
// repository on every mirror-only project in the fleet.
func TestBuildCacheShadowFallsThroughWithoutARegistryBackend(t *testing.T) {
	f := newBCFixture(t)

	// This project has only the mirror.
	f.resolver.mu.Lock()
	delete(f.resolver.routes, repository.BackendKindRegistry)
	f.resolver.mu.Unlock()

	f.backend.mirror = f.mirror

	// A read reaches the mirror: the counting fake upstream gets consulted, which
	// only the mirror is capable of -- BuildCache has no Fetcher to consult. The
	// STATUS is not the discriminator in this fixture: its blob.Service is read-only
	// (Tx: nil), so the mirror's fetch-through cannot persist what it fetched and
	// the response degrades to a miss. The upstream counters cannot lie either way.
	f.do(http.MethodGet, bcBuildkitPfx+"app/manifests/latest", bearer(bcWriteKey), nil)

	resolves, manifests, _, _ := f.upstream.counts()
	if resolves+manifests == 0 {
		t.Error("the mirror's upstream was never consulted: the fallback did not reach the mirror")
	}

	// A write never falls through: the mirror has no push API, and an unconfigured
	// backend never mounts one.
	if w := f.do(http.MethodPost, bcBuildkitPfx+"app/blobs/uploads/", bearer(bcWriteKey), nil); w.Code != http.StatusNotFound {
		t.Errorf("POST without a registry backend = %d, want 404", w.Code)
	}

	// With the registry backend present the shadow applies: same path, same token,
	// answered by the writable namespace's own clean miss -- never the mirror, never
	// an upstream.
	f2 := newBCFixture(t)
	f2.backend.mirror = f2.mirror

	if w := f2.do(http.MethodGet, bcBuildkitPfx+"app/manifests/latest", bearer(bcWriteKey), nil); w.Code != http.StatusNotFound {
		t.Errorf("shadowed GET = %d, want the buildcache 404", w.Code)
	}

	f2.assertNoUpstream(t)
}
