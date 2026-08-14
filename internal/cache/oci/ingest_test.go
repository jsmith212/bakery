package oci

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// dbtest.Main is MANDATORY in every package that uses dbtest: it is the only hook that
// runs after the last test and can stop the shared container.
//
// The INGEST path cannot be honestly faked. Put's refcount arithmetic lives in a
// Postgres trigger, the tag repoint is an ON CONFLICT DO UPDATE whose trigger branch
// swaps two blobs' refcounts atomically, and Touch is an UPDATE whose whole point is
// which columns it does NOT move. A fake would assert our beliefs about the schema
// rather than the schema.
func TestMain(m *testing.M) { dbtest.Main(m) }

// ingestFixture is the full stack: a real blob.Service over a real migrated Postgres
// and a real local byte store, the real handler, and a fake upstream.
type ingestFixture struct {
	mux      *http.ServeMux
	backend  *Backend
	upstream *fakeUpstream
	store    *db.Store
	route    cache.Route
	metrics  *metrics.Metrics

	clock time.Time

	// refreshes receives the result of every completed background tag refresh, so a
	// test JOINS the goroutine instead of sleeping and hoping.
	refreshes chan string
}

func newIngestFixture(t *testing.T, cfgJSON string) *ingestFixture {
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

	backendRow, err := store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID: project.ID, Kind: repository.BackendKindOci,
		Enabled: true, ReadAuthRequired: false, Config: []byte(cfgJSON),
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	route := cache.Route{
		OrgID: org.ID, ProjectID: project.ID, Org: "acme", Project: "widget",
		BackendID: backendRow.ID, Kind: repository.BackendKindOci,
		Enabled: true, ReadAuthRequired: false, Config: []byte(cfgJSON),
	}

	up := newFakeUpstream(testIndex(t), testIndexType)

	f := &ingestFixture{
		mux: http.NewServeMux(), backend: nil, upstream: up, store: store,
		route: route, metrics: m,
		clock: time.Now().UTC(), refreshes: make(chan string, 8),
	}

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}

	authn := &fakeAuthenticator{
		mu: sync.Mutex{}, principal: fakePrincipal{canRead: true, canWrite: true}, err: nil, seen: nil,
	}

	f.backend = New(deps, &fakeResolver{route: route, found: true}, authn, up,
		Config{ExternalURL: "https://bakery.example.com", UpstreamAuth: nil})
	f.backend.now = func() time.Time { return f.clock }
	f.backend.refreshHook = func(result string) { f.refreshes <- result }
	f.backend.Register(f.mux)

	return f
}

// pull issues an authenticated tag pull. A credential is required for anything that
// reaches the upstream: no principal, no upstream.
func (f *ingestFixture) pull(target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer bkry_a_valid_key")

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)

	return w
}

// awaitRefresh joins the background revalidation and returns its result.
func (f *ingestFixture) awaitRefresh(t *testing.T) string {
	t.Helper()

	select {
	case result := <-f.refreshes:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("background tag refresh never completed")

		return ""
	}
}

// keysIn lists the keys present in a namespace. It goes through the pool directly
// because there is no sqlc query for "enumerate a namespace" -- and there should not
// be one in production code, which never needs it.
func (f *ingestFixture) keysIn(ctx context.Context, t *testing.T, namespace string) []string {
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

const testConfig = `{"default_upstream":"docker.io","upstreams":["docker.io","ghcr.io"],"tag_ttl":"10m"}`

// TestStoredDigestIsSelfComputed is the single most important ingest assertion.
//
// go-containerregistry does NOT verify a tag-fetched manifest against the upstream's
// Docker-Content-Digest -- its own source says so, because too many registries get the
// header wrong. So the header is an unverified string from a third party. If it were
// used as the storage key, one broken or malicious upstream response would store bytes
// under a digest they do not hash to, and every client (all of which verify manifest
// bytes against the digest they asked for) would hard-fail on every pull of that image,
// permanently.
//
// Here the upstream LIES: it serves the real alpine index while claiming a completely
// different digest. The bytes must land under the digest WE computed, and the lie must
// name nothing at all.
func TestStoredDigestIsSelfComputed(t *testing.T) {
	f := newIngestFixture(t, testConfig)

	const lie = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	f.upstream.claimDigest = lie

	w := f.pull(tenantPrefix + "library/alpine/manifests/3.20")
	if w.Code != http.StatusOK {
		t.Fatalf("pull = %d, want 200 (body %q)", w.Code, w.Body.String())
	}

	// The response advertises OUR digest, which is the one the registry itself reported
	// for these bytes.
	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:"+testIndexDigest {
		t.Errorf("Docker-Content-Digest = %q, want the SELF-COMPUTED sha256:%s", got, testIndexDigest)
	}

	keys := f.keysIn(t.Context(), t, nsManifests)
	if len(keys) != 1 || keys[0] != testIndexDigest {
		t.Fatalf("manifests namespace holds %v, want exactly [%s]", keys, testIndexDigest)
	}

	// And the lie names nothing: a client that trusted the upstream's header and asked
	// for that digest gets a clean miss, not somebody else's bytes.
	if got := f.pull(tenantPrefix + "library/alpine/manifests/" + lie); got.Code != http.StatusNotFound {
		t.Errorf("pull by the LIED digest = %d, want 404", got.Code)
	}
}

// TestManifestBytesSurviveByteForByte is the no-re-serialization gate, and it uses a
// REAL MULTI-ARCH INDEX because that is the only payload that catches the bug: a
// json.Marshal round trip reorders keys and rewrites whitespace, which changes the
// digest, and it only shows up on an index in production.
func TestManifestBytesSurviveByteForByte(t *testing.T) {
	f := newIngestFixture(t, testConfig)
	raw := testIndex(t)

	w := f.pull(tenantPrefix + "library/alpine/manifests/3.20")
	if w.Code != http.StatusOK {
		t.Fatalf("pull = %d, want 200", w.Code)
	}

	if !bytes.Equal(w.Body.Bytes(), raw) {
		t.Error("served manifest is not byte-identical to the upstream's; something re-serialized it")
	}

	if got := storage.KeyOf(w.Body.Bytes()).String(); got != testIndexDigest {
		t.Errorf("served bytes hash to %s, want the registry's own %s", got, testIndexDigest)
	}

	if got := w.Header().Get("Content-Type"); got != testIndexType {
		t.Errorf("Content-Type = %q, want the upstream's %q verbatim -- containerd assigns "+
			"it straight into the descriptor's MediaType", got, testIndexType)
	}
}

// TestTagRefreshTouchRestoresFreshness is the freshness half of stale-while-revalidate.
//
// A revalidation that finds the tag UNCHANGED -- the overwhelmingly common case for a
// stable tag -- writes no bytes and repoints no row. Without an explicit updated_at
// bump the row's freshness never advances, so every tag is PERMANENTLY STALE after its
// first TTL: an upstream HEAD per window per tag forever, ResultStale on every
// response, and a real outage made indistinguishable from steady state.
//
// IT RUNS ON THE REAL CLOCK, deliberately. Every other test here injects one, but
// updated_at is written by Postgres' now(), so an injected Go clock and the database
// disagree by construction and the assertion would be about the fixture rather than
// about the code. A 300ms TTL keeps the test fast while letting both clocks be the same
// wall clock, exactly as they are in production.
func TestTagRefreshTouchRestoresFreshness(t *testing.T) {
	f := newIngestFixture(t, `{"default_upstream":"docker.io","tag_ttl":"300ms"}`)
	f.backend.now = time.Now

	target := tenantPrefix + "library/alpine/manifests/3.20"

	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("first pull = %d, want 200", w.Code)
	}

	// Still inside the TTL: no revalidation at all.
	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("fresh pull = %d, want 200", w.Code)
	}

	if resolves, _, _, _ := f.upstream.counts(); resolves != 0 {
		t.Fatalf("a FRESH tag revalidated %d times; it must contact nobody", resolves)
	}

	time.Sleep(400 * time.Millisecond) // past the TTL

	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("stale pull = %d, want 200 served immediately from cache", w.Code)
	}

	if got := f.awaitRefresh(t); got != metrics.OCIRefreshUnchanged {
		t.Fatalf("refresh result = %q, want %q", got, metrics.OCIRefreshUnchanged)
	}

	resolvesAfter, _, _, _ := f.upstream.counts()

	// THE ASSERTION. The touch must have restored freshness, so this pull -- issued
	// immediately, well inside the TTL measured from the touch -- revalidates nothing.
	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("post-refresh pull = %d, want 200", w.Code)
	}

	select {
	case result := <-f.refreshes:
		t.Fatalf("the tag revalidated AGAIN (%q) immediately after an unchanged refresh: "+
			"the freshness touch is not restoring updated_at, so every tag in the system "+
			"is permanently stale after its first TTL", result)
	case <-time.After(150 * time.Millisecond):
	}

	if resolves, _, _, _ := f.upstream.counts(); resolves != resolvesAfter {
		t.Errorf("upstream resolves went %d -> %d after a touch; freshness was not restored",
			resolvesAfter, resolves)
	}
}

// TestTouchDoesNotResetTheGCWriteBarrier. created_at and live_xid ARE the write
// barrier, and the barrier marks ROW CREATION. A revalidation that confirmed nothing
// changed created nothing; resetting the barrier would make an unchanged, ancient,
// never-pulled tag immortal simply because something revalidated it.
func TestTouchDoesNotResetTheGCWriteBarrier(t *testing.T) {
	// The injected clock is used ONLY to force staleness. The assertion is on the two
	// Postgres-written timestamps, which are compared against each other, so the Go
	// clock never enters it.
	f := newIngestFixture(t, testConfig)
	target := tenantPrefix + "library/alpine/manifests/3.20"

	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("first pull = %d, want 200", w.Code)
	}

	key := tagKey("docker.io", "library/alpine", "3.20")
	beforeCreated, beforeUpdated := f.tagTimestamps(t, key)

	f.clock = f.clock.Add(11 * time.Minute)

	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("stale pull = %d, want 200", w.Code)
	}

	if got := f.awaitRefresh(t); got != metrics.OCIRefreshUnchanged {
		t.Fatalf("refresh result = %q, want unchanged", got)
	}

	afterCreated, afterUpdated := f.tagTimestamps(t, key)

	if !afterCreated.Equal(beforeCreated) {
		t.Errorf("created_at moved %v -> %v: a touch must not reset the GC write barrier",
			beforeCreated, afterCreated)
	}

	if !afterUpdated.After(beforeUpdated) {
		t.Errorf("updated_at did not advance (%v -> %v): the touch did nothing",
			beforeUpdated, afterUpdated)
	}
}

// tagTimestamps reads the tag row's barrier and freshness columns.
func (f *ingestFixture) tagTimestamps(t *testing.T, key string) (created, updated time.Time) {
	t.Helper()

	row := f.store.Pool().QueryRow(t.Context(),
		`SELECT created_at, updated_at FROM cache_objects
		  WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		f.route.BackendID, nsTags, key)

	if err := row.Scan(&created, &updated); err != nil {
		t.Fatalf("read tag timestamps: %v", err)
	}

	return created, updated
}

// TestStaleTagServesImmediatelyAndRepoints. The client never waits for the upstream:
// a stale tag is answered from cache at local speed, and the repoint lands in the
// background and is visible on the next pull.
func TestStaleTagServesImmediatelyAndRepoints(t *testing.T) {
	f := newIngestFixture(t, testConfig)
	target := tenantPrefix + "library/alpine/manifests/3.20"

	old := testIndex(t)
	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("first pull = %d, want 200", w.Code)
	}

	// The tag moves upstream.
	updated := append(append([]byte(nil), old...), ' ')
	f.upstream.setRaw(updated)

	f.clock = f.clock.Add(11 * time.Minute)

	w := f.pull(target)
	if w.Code != http.StatusOK {
		t.Fatalf("stale pull = %d, want 200", w.Code)
	}

	// SERVED IMMEDIATELY, from cache, with the OLD bytes. The refresh has not
	// necessarily even started; the client does not wait for it.
	if !bytes.Equal(w.Body.Bytes(), old) {
		t.Error("a stale pull did not serve the cached bytes immediately")
	}

	if got := f.awaitRefresh(t); got != metrics.OCIRefreshRepointed {
		t.Fatalf("refresh result = %q, want %q", got, metrics.OCIRefreshRepointed)
	}

	after := f.pull(target)
	if after.Code != http.StatusOK {
		t.Fatalf("post-repoint pull = %d, want 200", after.Code)
	}

	if !bytes.Equal(after.Body.Bytes(), updated) {
		t.Error("the tag did not repoint at the new manifest")
	}

	if got, want := after.Header().Get("Docker-Content-Digest"),
		"sha256:"+storage.KeyOf(updated).String(); got != want {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, want)
	}

	// BOTH manifests are still stored under their own digests: the repoint changed the
	// tag's target, it did not mutate an immutable manifest row.
	if n := len(f.keysIn(t.Context(), t, nsManifests)); n != 2 {
		t.Errorf("manifests namespace holds %d rows, want 2 (old and new)", n)
	}

	if n := len(f.keysIn(t.Context(), t, nsTags)); n != 1 {
		t.Errorf("tags namespace holds %d rows, want 1 -- a repoint must not mint a new tag row", n)
	}
}

// TestUpstreamOutageServesStaleUnbounded. A build cache that fails closed when Docker
// Hub is down is not doing its job. The outage is visible ONLY in
// bakery_oci_tag_refresh_total{result="error"} -- no client will ever report it,
// because every client silently falls back to the real registry.
func TestUpstreamOutageServesStaleUnbounded(t *testing.T) {
	f := newIngestFixture(t, testConfig)
	target := tenantPrefix + "library/alpine/manifests/3.20"

	raw := testIndex(t)
	if w := f.pull(target); w.Code != http.StatusOK {
		t.Fatalf("first pull = %d, want 200", w.Code)
	}

	f.upstream.setErr(errors.New("dial tcp: connection refused"))

	// Long past the TTL, and then some. Serving stale is unbounded on purpose.
	for _, elapsed := range []time.Duration{11 * time.Minute, 24 * time.Hour, 30 * 24 * time.Hour} {
		f.clock = f.clock.Add(elapsed)

		w := f.pull(target)
		if w.Code != http.StatusOK {
			t.Fatalf("pull %v into an outage = %d, want 200 from cache", elapsed, w.Code)
		}

		if !bytes.Equal(w.Body.Bytes(), raw) {
			t.Errorf("pull %v into an outage served the wrong bytes", elapsed)
		}

		if got := f.awaitRefresh(t); got != metrics.OCIRefreshError {
			t.Errorf("refresh result = %q, want %q -- the outage must be loud in the metrics",
				got, metrics.OCIRefreshError)
		}
	}
}

// TestUpstreamHostNormalizationIsOneRow. containerd sends ns=docker.io, the registry
// answering is registry-1.docker.io, and Docker's config files say index.docker.io.
// Unnormalized, one upstream tag becomes several rows with independent TTLs that can
// disagree about which digest :latest is.
func TestUpstreamHostNormalizationIsOneRow(t *testing.T) {
	f := newIngestFixture(t,
		`{"default_upstream":"docker.io","upstreams":["docker.io","index.docker.io","registry-1.docker.io"]}`)

	for _, ns := range []string{"", "?ns=docker.io", "?ns=registry-1.docker.io", "?ns=index.docker.io"} {
		if w := f.pull(tenantPrefix + "library/alpine/manifests/3.20" + ns); w.Code != http.StatusOK {
			t.Fatalf("pull with %q = %d, want 200", ns, w.Code)
		}
	}

	keys := f.keysIn(t.Context(), t, nsTags)
	if len(keys) != 1 {
		t.Fatalf("tags namespace holds %v, want ONE row -- the upstream host is not normalized", keys)
	}

	if want := tagKey("docker.io", "library/alpine", "3.20"); keys[0] != want {
		t.Errorf("tag key = %q, want %q", keys[0], want)
	}
}

// TestBlobIngestRejectsMismatchedBytes. A blob is addressed by its OWN content, so this
// is the OCI trap in its pure form: bytes that do not hash to the requested digest must
// never be stored under it.
func TestBlobIngestRejectsMismatchedBytes(t *testing.T) {
	f := newIngestFixture(t, testConfig)

	honest := []byte("the bytes that were actually asked for")
	wanted := "sha256:" + storage.KeyOf(honest).String()

	// The upstream serves DIFFERENT bytes under the requested digest.
	f.upstream.blobs[wanted] = []byte("something else entirely")

	w := f.pull(tenantPrefix + "library/alpine/blobs/" + wanted)
	if w.Code != http.StatusNotFound {
		t.Fatalf("blob pull with mismatched upstream bytes = %d, want 404", w.Code)
	}

	if keys := f.keysIn(t.Context(), t, nsBlobs); len(keys) != 0 {
		t.Errorf("blobs namespace holds %v after a digest mismatch; nothing may be stored", keys)
	}
}

// TestBlobIngestStoresAndServes -- the honest path, so the rejection above is not
// simply "blob ingest never works".
func TestBlobIngestStoresAndServes(t *testing.T) {
	f := newIngestFixture(t, testConfig)

	body := []byte("a small but genuine layer")
	digest := "sha256:" + storage.KeyOf(body).String()
	f.upstream.blobs[digest] = body

	w := f.pull(tenantPrefix + "library/alpine/blobs/" + digest)
	if w.Code != http.StatusOK {
		t.Fatalf("blob pull = %d, want 200 (body %q)", w.Code, w.Body.String())
	}

	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Error("served blob bytes differ from the upstream's")
	}

	// A second pull is a HIT: it must contact nobody.
	before := func() int { _, _, _, gets := f.upstream.counts(); return gets }()

	if again := f.pull(tenantPrefix + "library/alpine/blobs/" + digest); again.Code != http.StatusOK {
		t.Fatalf("second blob pull = %d, want 200", again.Code)
	}

	if after := func() int { _, _, _, gets := f.upstream.counts(); return gets }(); after != before {
		t.Errorf("a cached blob was fetched from upstream again (%d -> %d)", before, after)
	}
}
