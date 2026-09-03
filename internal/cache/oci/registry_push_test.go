package oci

import (
	"bytes"
	"context"
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
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// The PUSH path cannot be honestly faked. blob.Service.Put's dedup, the refcount
// trigger's arithmetic, and the tag repoint's ON CONFLICT DO UPDATE all live in
// Postgres, and a fake would assert our beliefs about the schema rather than the
// schema. So these run against a real migrated database, exactly like ingest_test.go.
// (TestMain lives there; there is one per package.)

// bcPushFixture is the full stack: the writable backend and the read-only mirror on
// one mux, a real blob.Service over a real migrated Postgres and a real local byte
// store, and the mirror's counting upstream fake as the anti-regression witness.
type bcPushFixture struct {
	mux      *http.ServeMux
	backend  *BuildCache
	upstream *fakeUpstream
	store    *db.Store
	route    cache.Route
}

func newBCPushFixture(t *testing.T) *bcPushFixture {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	m := metrics.New()

	svc, err := blob.New(blob.Config{
		Reader: store, Tx: store,
		Storage: storage.NewInstrumented(local, m, metrics.DriverLocal),
		Metrics: m, CacheSize: 0,
	})
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

	routes := &fakeKindResolver{mu: sync.Mutex{}, routes: map[repository.BackendKind]cache.Route{}}

	for _, kind := range []repository.BackendKind{
		repository.BackendKindRegistry, repository.BackendKindOci,
	} {
		row, berr := store.CreateBackend(ctx, repository.CreateBackendParams{
			ProjectID: project.ID, Kind: kind,
			Enabled: true, ReadAuthRequired: false, Config: []byte(`{}`),
		})
		if berr != nil {
			t.Fatalf("CreateBackend(%s): %v", kind, berr)
		}

		routes.set(kind, cache.Route{
			OrgID: org.ID, ProjectID: project.ID, Org: "acme", Project: "widget",
			BackendID: row.ID, Kind: kind,
			Enabled: true, ReadAuthRequired: false, Config: []byte(`{}`),
		})
	}

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}

	up := newFakeUpstream(testIndex(t), testIndexType)
	cfg := Config{ExternalURL: "https://bakery.example.com", UpstreamAuth: nil}
	registryRoute, _ := routes.Resolve(ctx, "acme", "widget", repository.BackendKindRegistry)

	f := &bcPushFixture{
		mux:      http.NewServeMux(),
		backend:  NewBuildCache(deps, routes, newKeyring(), cfg, nil),
		upstream: up,
		store:    store,
		route:    registryRoute,
	}

	New(deps, routes, newKeyring(), up, cfg).Register(f.mux)
	f.backend.Register(f.mux)

	return f
}

// do issues one request against the mounted mux, authenticated with token unless it is
// empty.
func (f *bcPushFixture) do(
	method, target, token string, headers map[string]string, body []byte,
) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}

	r := httptest.NewRequest(method, target, rdr)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}

	for k, v := range headers {
		r.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)

	return w
}

// keysIn lists the keys present in one of the writable backend's namespaces.
func (f *bcPushFixture) keysIn(ctx context.Context, t *testing.T, namespace string) []string {
	t.Helper()

	rows, err := f.store.Pool().Query(ctx,
		`SELECT key FROM cache_objects WHERE backend_id = $1 AND namespace = $2 ORDER BY key`,
		f.route.BackendID, namespace)
	if err != nil {
		t.Fatalf("query keys: %v", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan key: %v", err)
		}

		out = append(out, k)
	}

	return out
}

// assertNoUpstream is spec §8.4's anti-regression: zero upstream requests during the
// whole gate. The mirror's counting fake is mounted on the SAME project, so a
// buildcache request that leaked into it would be visible here.
func (f *bcPushFixture) assertNoUpstream(t *testing.T) {
	t.Helper()

	resolves, manifests, stats, gets := f.upstream.counts()
	if resolves+manifests+stats+gets != 0 {
		t.Errorf("the writable namespace reached an upstream (resolve=%d manifest=%d stat=%d get=%d)",
			resolves, manifests, stats, gets)
	}
}

// pushBlob runs the two-step blob upload and returns the digest hex.
func (f *bcPushFixture) pushBlob(t *testing.T, prefix, repo string, body []byte) string {
	t.Helper()

	start := f.do(http.MethodPost, prefix+repo+"/blobs/uploads/", bcWriteKey, nil, nil)
	if start.Code != http.StatusAccepted {
		t.Fatalf("POST uploads = %d, want 202 (body %q)", start.Code, start.Body.String())
	}

	location := start.Header().Get("Location")
	if location == "" {
		t.Fatal("POST uploads returned no Location; containerd has nothing to PUT to")
	}

	hex := storage.KeyOf(body).String()

	done := f.do(http.MethodPut, location+"?digest=sha256:"+hex, bcWriteKey, nil, body)

	// 202 IS THE FAILURE MODE. containerd accepts 200/201/204 from the final PUT and
	// hard-rejects 202 (pusher.go:329) -- with ignore-error=true on --cache-to, which
	// the snippet always carries, that failure is silent and permanent.
	if done.Code != http.StatusCreated {
		t.Fatalf("PUT blob = %d, want 201 (never 202) (body %q)", done.Code, done.Body.String())
	}

	if got := done.Header().Get("Docker-Content-Digest"); got != "sha256:"+hex {
		t.Errorf("PUT blob Docker-Content-Digest = %q, want sha256:%s", got, hex)
	}

	return hex
}

// TestBuildCachePushRoundTrip is the milestone in one test: BuildKit's export, then
// its import, in both manifest shapes and both route families.
//
// The manifest bytes must come back BYTE-IDENTICAL with the pushed Content-Type: the
// digest is the sha256 of the exact bytes, so a json.Marshal round trip anywhere in
// the path would change it and break Docker-Content-Digest for every client at once --
// and containerd assigns the response Content-Type straight into the descriptor's
// MediaType.
func TestBuildCachePushRoundTrip(t *testing.T) {
	for _, fam := range bcFamilies() {
		for _, shape := range bcManifestShapes() {
			t.Run(fam.name+"/"+shape.name, func(t *testing.T) {
				f := newBCPushFixture(t)

				const repo = "impulse"

				layer := []byte("a buildkit cache layer, gzipped in real life")
				layerHex := f.pushBlob(t, fam.prefix, repo, layer)

				raw := []byte(shape.body)
				digest := storage.KeyOf(raw)

				put := f.do(http.MethodPut, fam.prefix+repo+"/manifests/main", bcWriteKey,
					map[string]string{"Content-Type": shape.mediaType}, raw)
				if put.Code != http.StatusCreated {
					t.Fatalf("PUT manifest = %d, want 201 (body %q)", put.Code, put.Body.String())
				}

				if got, want := put.Header().Get("Docker-Content-Digest"),
					"sha256:"+digest.String(); got != want {
					t.Errorf("PUT manifest Docker-Content-Digest = %q, want the SELF-COMPUTED %q", got, want)
				}

				if got := put.Header().Get("Location"); got != fam.prefix+repo+"/manifests/sha256:"+digest.String() {
					t.Errorf("PUT manifest Location = %q", got)
				}

				// THE HEAD THAT MAKES A REPEAT EXPORT FREE: containerd skips the PUT
				// entirely when this digest matches the descriptor it holds.
				head := f.do(http.MethodHead, fam.prefix+repo+"/manifests/main", bcWriteKey, nil, nil)
				if head.Code != http.StatusOK {
					t.Fatalf("HEAD manifest = %d, want 200", head.Code)
				}

				if got, want := head.Header().Get("Docker-Content-Digest"),
					"sha256:"+digest.String(); got != want {
					t.Errorf("HEAD Docker-Content-Digest = %q, want %q", got, want)
				}

				if got, want := head.Header().Get("Content-Length"), strconv.Itoa(len(raw)); got != want {
					t.Errorf("HEAD Content-Length = %q, want %q", got, want)
				}

				if got := head.Header().Get("Content-Type"); got != shape.mediaType {
					t.Errorf("HEAD Content-Type = %q, want the stored %q", got, shape.mediaType)
				}

				// The import leg: manifest bytes verbatim, then the config/layer blob.
				get := f.do(http.MethodGet, fam.prefix+repo+"/manifests/main", bcWriteKey, nil, nil)
				if get.Code != http.StatusOK {
					t.Fatalf("GET manifest = %d, want 200", get.Code)
				}

				if !bytes.Equal(get.Body.Bytes(), raw) {
					t.Errorf("served manifest is not byte-identical to the pushed bytes;\n got %q\nwant %q",
						get.Body.String(), shape.body)
				}

				if got := get.Header().Get("Content-Type"); got != shape.mediaType {
					t.Errorf("GET Content-Type = %q, want %q", got, shape.mediaType)
				}

				byDigest := f.do(http.MethodGet,
					fam.prefix+repo+"/manifests/sha256:"+digest.String(), bcWriteKey, nil, nil)
				if byDigest.Code != http.StatusOK || !bytes.Equal(byDigest.Body.Bytes(), raw) {
					t.Errorf("GET manifest by digest = %d", byDigest.Code)
				}

				blobGet := f.do(http.MethodGet, fam.prefix+repo+"/blobs/sha256:"+layerHex, bcWriteKey, nil, nil)
				if blobGet.Code != http.StatusOK || !bytes.Equal(blobGet.Body.Bytes(), layer) {
					t.Fatalf("GET blob = %d", blobGet.Code)
				}

				// One tag row, one manifest row, one blob row: the tag Put re-presented
				// bytes already stored, so dedup elided the byte write.
				if keys := f.keysIn(t.Context(), t, nsTags); len(keys) != 1 ||
					keys[0] != buildCacheTagKey(repo, "main") {
					t.Errorf("tags namespace holds %v, want [%s]", keys, buildCacheTagKey(repo, "main"))
				}

				if keys := f.keysIn(t.Context(), t, nsManifests); len(keys) != 1 || keys[0] != digest.String() {
					t.Errorf("manifests namespace holds %v, want [%s]", keys, digest.String())
				}

				if keys := f.keysIn(t.Context(), t, nsBlobs); len(keys) != 1 || keys[0] != layerHex {
					t.Errorf("blobs namespace holds %v, want [%s]", keys, layerHex)
				}

				f.assertNoUpstream(t)
			})
		}
	}
}

// TestBuildCacheRePushIsIdempotent. containerd HEADs before it pushes, but a race
// between two exporters, or a client that skips the probe, re-PUTs content we already
// hold. That must be a 201 -- the client's question is "is this in the registry now" --
// and it must not mint a second row or swap any content.
func TestBuildCacheRePushIsIdempotent(t *testing.T) {
	f := newBCPushFixture(t)

	const repo = "impulse"

	layer := []byte("a layer pushed twice")
	raw := []byte(bcIndexManifest)

	for range 2 {
		f.pushBlob(t, bcBuildkitPfx, repo, layer)

		put := f.do(http.MethodPut, bcBuildkitPfx+repo+"/manifests/main", bcWriteKey,
			map[string]string{"Content-Type": bcIndexType}, raw)
		if put.Code != http.StatusCreated {
			t.Fatalf("re-PUT manifest = %d, want 201", put.Code)
		}
	}

	for _, ns := range []string{nsBlobs, nsManifests, nsTags} {
		if keys := f.keysIn(t.Context(), t, ns); len(keys) != 1 {
			t.Errorf("%s namespace holds %v after a re-push, want exactly one row", ns, keys)
		}
	}

	get := f.do(http.MethodGet, bcBuildkitPfx+repo+"/manifests/main", bcWriteKey, nil, nil)
	if !bytes.Equal(get.Body.Bytes(), raw) {
		t.Error("a re-push swapped the stored content")
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheTagOverwriteRepoints. Every export overwrites the tag; the previous
// manifest row becomes untagged and ages out on the GC ladder rather than being
// mutated in place. The refcount trigger does the decrement-old / increment-new
// arithmetic; Go never does.
func TestBuildCacheTagOverwriteRepoints(t *testing.T) {
	f := newBCPushFixture(t)

	const repo = "impulse"

	first := []byte(bcIndexManifest)
	second := []byte(bcImageManifest)

	for _, m := range []struct {
		raw       []byte
		mediaType string
	}{
		{raw: first, mediaType: bcIndexType},
		{raw: second, mediaType: bcImageType},
	} {
		put := f.do(http.MethodPut, bcBuildkitPfx+repo+"/manifests/main", bcWriteKey,
			map[string]string{"Content-Type": m.mediaType}, m.raw)
		if put.Code != http.StatusCreated {
			t.Fatalf("PUT manifest = %d, want 201", put.Code)
		}
	}

	head := f.do(http.MethodHead, bcBuildkitPfx+repo+"/manifests/main", bcWriteKey, nil, nil)
	if got, want := head.Header().Get("Docker-Content-Digest"),
		"sha256:"+storage.KeyOf(second).String(); got != want {
		t.Errorf("after the second export the tag reports %q, want %q", got, want)
	}

	if got := head.Header().Get("Content-Type"); got != bcImageType {
		t.Errorf("Content-Type = %q, want the second push's %q -- the tag row's media type "+
			"must be repointed too", got, bcImageType)
	}

	get := f.do(http.MethodGet, bcBuildkitPfx+repo+"/manifests/main", bcWriteKey, nil, nil)
	if !bytes.Equal(get.Body.Bytes(), second) {
		t.Error("the tag did not repoint at the second manifest")
	}

	// BOTH manifests survive under their own digests: a repoint changes the tag's
	// target, it does not mutate an immutable manifest row.
	if keys := f.keysIn(t.Context(), t, nsManifests); len(keys) != 2 {
		t.Errorf("manifests namespace holds %v, want 2 (old and new)", keys)
	}

	if keys := f.keysIn(t.Context(), t, nsTags); len(keys) != 1 {
		t.Errorf("tags namespace holds %v, want 1 -- a repoint must not mint a new tag row", keys)
	}

	// The displaced manifest is still fetchable by digest, which is what an in-flight
	// import holding the old descriptor needs.
	old := f.do(http.MethodGet, bcBuildkitPfx+repo+"/manifests/sha256:"+storage.KeyOf(first).String(),
		bcWriteKey, nil, nil)
	if old.Code != http.StatusOK || !bytes.Equal(old.Body.Bytes(), first) {
		t.Errorf("the displaced manifest = %d, want 200 with its own bytes", old.Code)
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheRejectsBadPushBodies covers every 400 on the push path.
//
// A blob is addressed by its OWN content, so bytes that do not hash to the ?digest=
// must never be stored under it -- the OCI trap in its pure form. And nothing may be
// stored on a rejection: the assertion is on the namespace being empty afterwards, not
// only on the status.
func TestBuildCacheRejectsBadPushBodies(t *testing.T) {
	honest := []byte("the bytes that were actually declared")

	tests := []struct {
		name      string
		method    string
		tail      string
		mediaType string
		body      []byte
		wantCode  string
		namespace string
	}{
		{
			name:   "blob content does not match the digest",
			method: http.MethodPut,
			tail:   "impulse/blobs/uploads/u?digest=sha256:" + storage.KeyOf(honest).String(),
			body:   []byte("something else entirely"), wantCode: codeDigestInvalid, namespace: nsBlobs,
		},
		{
			name:   "blob PUT with no digest parameter",
			method: http.MethodPut, tail: "impulse/blobs/uploads/u",
			body: honest, wantCode: codeDigestInvalid, namespace: nsBlobs,
		},
		{
			name:   "blob PUT with a non-sha256 digest",
			method: http.MethodPut,
			tail:   "impulse/blobs/uploads/u?digest=sha512:" + strings.Repeat("a", 64),
			body:   honest, wantCode: codeDigestInvalid, namespace: nsBlobs,
		},
		{
			name:   "blob PUT with an uppercase digest",
			method: http.MethodPut,
			tail:   "impulse/blobs/uploads/u?digest=sha256:" + strings.ToUpper(storage.KeyOf(honest).String()),
			body:   honest, wantCode: codeDigestInvalid, namespace: nsBlobs,
		},
		{
			name:   "empty manifest",
			method: http.MethodPut, tail: "impulse/manifests/main", mediaType: bcIndexType,
			body: []byte{}, wantCode: codeManifestInvalid, namespace: nsManifests,
		},
		{
			name:   "manifest pushed by a digest it does not hash to",
			method: http.MethodPut,
			tail:   "impulse/manifests/sha256:" + strings.Repeat("f", 64), mediaType: bcIndexType,
			body: []byte(bcIndexManifest), wantCode: codeDigestInvalid, namespace: nsManifests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBCPushFixture(t)

			headers := map[string]string{}
			if tt.mediaType != "" {
				headers["Content-Type"] = tt.mediaType
			}

			w := f.do(tt.method, bcBuildkitPfx+tt.tail, bcWriteKey, headers, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("= %d, want 400 (body %q)", w.Code, w.Body.String())
			}

			assertOCIError(t, w.Body.Bytes(), tt.wantCode)

			if keys := f.keysIn(t.Context(), t, tt.namespace); len(keys) != 0 {
				t.Errorf("%s namespace holds %v after a rejected push; nothing may be stored",
					tt.namespace, keys)
			}

			f.assertNoUpstream(t)
		})
	}
}

// TestBuildCacheStoresTheEmptyBlob. §3 carves out exactly one empty body: the empty
// blob e3b0c442... with a matching ?digest= is legal and stores normally. Only a
// manifest body may not be empty.
func TestBuildCacheStoresTheEmptyBlob(t *testing.T) {
	f := newBCPushFixture(t)

	hex := storage.KeyOf(nil).String()

	w := f.do(http.MethodPut, bcBuildkitPfx+"impulse/blobs/uploads/u?digest=sha256:"+hex,
		bcWriteKey, nil, []byte{})
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT the empty blob = %d, want 201 (body %q)", w.Code, w.Body.String())
	}

	get := f.do(http.MethodGet, bcBuildkitPfx+"impulse/blobs/sha256:"+hex, bcWriteKey, nil, nil)
	if get.Code != http.StatusOK || get.Body.Len() != 0 {
		t.Errorf("GET the empty blob = %d with %d bytes, want 200 and 0", get.Code, get.Body.Len())
	}

	f.assertNoUpstream(t)
}

// TestBuildCacheManifestMediaTypeIsAllowlisted pins the stored-XSS gate. The manifest
// BYTES round-trip verbatim whatever the pusher sent; the Content-Type echoed to
// readers comes only from the closed manifest-type set. An attacker-chosen text/html
// would otherwise render on Bakery's own origin -- the console's session cookie lives
// there -- from any open-read backend. Off-list types store NULL and read back as
// defaultManifestType; nosniff rides the response as defense in depth.
func TestBuildCacheManifestMediaTypeIsAllowlisted(t *testing.T) {
	f := newBCPushFixture(t)

	const pfx = "/v2/acme/widget/buildcache/"

	body := []byte(`<script>fetch('/api/v1/me')</script>`)

	put := f.do(http.MethodPut, pfx+"pwn/manifests/latest", bcWriteKey,
		map[string]string{"Content-Type": "text/html"}, body)
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: the bytes are welcome, only the type is not", put.Code)
	}

	get := f.do(http.MethodGet, pfx+"pwn/manifests/latest", bcWriteKey, nil, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (body %q)", get.Code, get.Body.String())
	}

	if got := get.Header().Get("Content-Type"); got != defaultManifestType {
		t.Errorf("Content-Type = %q, want the %q fallback -- echoing the pusher's text/html is stored XSS",
			got, defaultManifestType)
	}

	if got := get.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}

	if got := get.Body.String(); got != string(body) {
		t.Errorf("manifest bytes were not served verbatim: %q", got)
	}

	// The closed set still echoes: the type containerd dispatches on survives the gate.
	const idxType = "application/vnd.oci.image.index.v1+json"

	idx := []byte(`{"schemaVersion":2,"manifests":[]}`)
	if put := f.do(http.MethodPut, pfx+"ok/manifests/latest", bcWriteKey,
		map[string]string{"Content-Type": idxType}, idx); put.Code != http.StatusCreated {
		t.Fatalf("PUT allowlisted = %d, want 201", put.Code)
	}

	if got := f.do(http.MethodGet, pfx+"ok/manifests/latest", bcWriteKey, nil, nil).
		Header().Get("Content-Type"); got != idxType {
		t.Errorf("allowlisted Content-Type = %q, want %q echoed", got, idxType)
	}
}

// TestBuildCacheImportReadTouchesTheTag pins the GC-liveness touch: a tag that is
// still IMPORTED must not age out just because exports stopped. Reading the tag over
// the wire must advance the row's updated_at (Touch), because `tags` bypasses the LRU
// and nothing else can record the read; without it, a cache imported on every PR
// build but exported by a quieted branch dies at W and takes its blobs at 2W.
func TestBuildCacheImportReadTouchesTheTag(t *testing.T) {
	f := newBCPushFixture(t)

	const pfx = "/v2/acme/widget/buildcache/"

	raw := []byte(`{"schemaVersion":2,"manifests":[]}`)
	if put := f.do(http.MethodPut, pfx+"impulse/manifests/main", bcWriteKey,
		map[string]string{"Content-Type": "application/vnd.oci.image.index.v1+json"}, raw); put.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", put.Code)
	}

	tagRef := f.route.Ref(nsTags, kindTag, buildCacheTagKey("impulse", "main"))

	before, err := f.backend.deps.Blobs.StatUncached(t.Context(), tagRef)
	if err != nil || !before.Exists {
		t.Fatalf("StatUncached before: exists=%v err=%v", before.Exists, err)
	}

	// Touch runs in its own transaction; now() is transaction start time, so two
	// back-to-back transactions could in principle share a microsecond. The sleep
	// makes strictly-after assertable rather than flaky.
	time.Sleep(10 * time.Millisecond)

	if w := f.do(http.MethodGet, pfx+"impulse/manifests/main", bcWriteKey, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}

	after, err := f.backend.deps.Blobs.StatUncached(t.Context(), tagRef)
	if err != nil || !after.Exists {
		t.Fatalf("StatUncached after: exists=%v err=%v", after.Exists, err)
	}

	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not advance on an import read (%v -> %v); the tag has no GC liveness",
			before.UpdatedAt, after.UpdatedAt)
	}
}
