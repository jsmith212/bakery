package oci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// ---------------------------------------------------------------------------
// Fixtures. Hand-written, stdlib only -- no testify, no gomock.
//
// Two flavours, and the split is deliberate. The READ tests drive a real
// blob.Service over a fake metadata Reader and a real local byte store: the
// handler, blob.Service and http.ServeContent are all real, and only Postgres is
// faked, exactly at the seam blob.Reader exists to be faked. The INGEST tests
// (ingest_test.go) need Put, Touch and the refcount triggers, which live in
// Postgres and cannot be faked honestly -- those run against a real migrated
// database.
// ---------------------------------------------------------------------------

// testIndex is a REAL multi-arch OCI index, captured from Docker Hub
// (library/alpine:3.20). It is the one payload that catches a re-serialization bug:
// its digest is asserted against the value the registry itself reported, so any
// marshal/unmarshal round trip anywhere in the ingest path fails the test.
func testIndex(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "alpine-index.json"))
	if err != nil {
		t.Fatalf("read testdata index: %v", err)
	}

	return raw
}

// testIndexDigest is the digest registry-1.docker.io reported for the captured index,
// verified independently with sha256sum. Hardcoded on purpose: computing it from the
// bytes would make the assertion tautological.
const testIndexDigest = "d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// testIndexType is the Content-Type the registry sent with it.
const testIndexType = "application/vnd.oci.image.index.v1+json"

// fakeReader is the cache_objects metadata store: key -> row, pgx.ErrNoRows for a miss.
type fakeReader struct {
	mu   sync.Mutex
	rows map[string]repository.StatObjectRow
}

func newFakeReader() *fakeReader {
	return &fakeReader{mu: sync.Mutex{}, rows: map[string]repository.StatObjectRow{}}
}

func (f *fakeReader) StatObject(
	_ context.Context, arg repository.StatObjectParams,
) (repository.StatObjectRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	row, ok := f.rows[arg.Namespace+"\x00"+arg.Key]
	if !ok {
		return repository.StatObjectRow{}, pgx.ErrNoRows
	}

	return row, nil
}

func (f *fakeReader) StatObjectsBatch(
	_ context.Context, _ repository.StatObjectsBatchParams,
) ([]repository.StatObjectsBatchRow, error) {
	return nil, errors.New("oci does not use ExistsBatch")
}

// ListObjectKeysByPrefix mirrors the SQL: sorted keys under a LIKE prefix pattern,
// within one namespace. The pattern is unescaped back to a literal prefix here; the
// escaping itself is asserted against real Postgres in internal/blob.
func (f *fakeReader) ListObjectKeysByPrefix(
	_ context.Context, arg repository.ListObjectKeysByPrefixParams,
) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	prefix := arg.Namespace + "\x00" + unLikePattern(arg.Prefix)

	var out []string

	for k := range f.rows {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, arg.Namespace+"\x00"))
		}
	}

	sort.Strings(out)

	return out, nil
}

// unLikePattern strips the trailing % and undoes the LIKE metacharacter escapes, in
// one left-to-right pass.
func unLikePattern(pattern string) string {
	prefix := strings.TrimSuffix(pattern, "%")

	var b strings.Builder

	for i := 0; i < len(prefix); i++ {
		if prefix[i] == '\\' && i+1 < len(prefix) {
			i++
		}

		b.WriteByte(prefix[i])
	}

	return b.String()
}

func (f *fakeReader) add(namespace, key string, digest storage.Key, size int64, ct string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rows[namespace+"\x00"+key] = repository.StatObjectRow{
		Digest:      digest.Bytes(),
		SizeBytes:   size,
		UpdatedAt:   pgtype.Timestamptz{Time: updated, InfinityModifier: 0, Valid: true},
		ContentType: pgtype.Text{String: ct, Valid: ct != ""},
	}
}

// fakeResolver returns a fixed Route. The handler makes every status and auth decision
// itself; the fake supplies only the resolved route.
type fakeResolver struct {
	route cache.Route
	found bool
}

func (f fakeResolver) Resolve(
	_ context.Context, _, _ string, _ repository.BackendKind,
) (cache.Route, bool) {
	return f.route, f.found
}

// testRoute is a resolved, enabled, OPEN oci route.
func testRoute() cache.Route {
	return cache.Route{
		OrgID:            testUUID(0x0a),
		ProjectID:        testUUID(0x1a),
		Org:              "acme",
		Project:          "widget",
		BackendID:        7,
		Kind:             repository.BackendKindOci,
		Enabled:          true,
		ReadAuthRequired: false,
		Config:           []byte(`{"default_upstream":"docker.io","upstreams":["docker.io","ghcr.io"]}`),
	}
}

func testUUID(b byte) pgtype.UUID {
	var out pgtype.UUID

	out.Valid = true
	for i := range out.Bytes {
		out.Bytes[i] = b
	}

	return out
}

// fakePrincipal answers the two capability questions and nothing else -- Principal is
// the narrow consumer-side interface, not auth's sealed identity.
type fakePrincipal struct{ canRead, canWrite bool }

func (p fakePrincipal) CanReadProject(_, _ pgtype.UUID) bool  { return p.canRead }
func (p fakePrincipal) CanWriteProject(_, _ pgtype.UUID) bool { return p.canWrite }

// fakeAuthenticator records EVERY token it is handed. That record is the assertion in
// the "a forwarded Docker Hub credential is never validated and never logged" test:
// the shape gate in credentialToken must mean this authenticator is never called at
// all for a foreign credential.
type fakeAuthenticator struct {
	mu        sync.Mutex
	principal Principal
	err       error
	seen      []string
}

func (a *fakeAuthenticator) AuthenticateToken(_ context.Context, token string) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.seen = append(a.seen, token)

	return a.principal, a.err
}

func (a *fakeAuthenticator) tokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.seen...)
}

// fakeUpstream is the hand-written Fetcher. It counts calls (so a test can prove a
// cache hit contacted nobody), can fail on demand (the outage test), and -- crucially
// -- can LIE about a manifest's digest.
type fakeUpstream struct {
	mu sync.Mutex

	raw       []byte
	mediaType string

	// claimDigest overrides the digest reported by Resolve and Manifest. Setting it to
	// something the bytes do not hash to is the whole point: an upstream that lies must
	// not be able to get bytes stored under the digest it claimed.
	claimDigest string

	blobs map[string][]byte

	err error

	resolves  int
	manifests int
	statBlobs int
	blobGets  int

	// requireNonNilPrincipal records a nil principal reaching the fetcher, which must
	// be impossible.
	sawNilPrincipal bool
}

func newFakeUpstream(raw []byte, mediaType string) *fakeUpstream {
	return &fakeUpstream{
		mu: sync.Mutex{}, raw: raw, mediaType: mediaType, claimDigest: "",
		blobs: map[string][]byte{}, err: nil,
		resolves: 0, manifests: 0, statBlobs: 0, blobGets: 0, sawNilPrincipal: false,
	}
}

// digest returns what the fake CLAIMS, which is the real digest unless a test lied.
func (u *fakeUpstream) digest() string {
	if u.claimDigest != "" {
		return u.claimDigest
	}

	return "sha256:" + storage.KeyOf(u.raw).String()
}

func (u *fakeUpstream) gate(p Principal) error {
	if p == nil {
		u.sawNilPrincipal = true

		return ErrNoPrincipal
	}

	return u.err
}

func (u *fakeUpstream) Resolve(
	_ context.Context, p Principal, _ UpstreamRef, _ string,
) (Manifest, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.resolves++

	if err := u.gate(p); err != nil {
		return Manifest{}, err
	}

	return Manifest{
		Raw: nil, MediaType: u.mediaType, Digest: u.digest(), Size: int64(len(u.raw)),
	}, nil
}

func (u *fakeUpstream) Manifest(
	_ context.Context, p Principal, _ UpstreamRef, _ string,
) (Manifest, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.manifests++

	if err := u.gate(p); err != nil {
		return Manifest{}, err
	}

	return Manifest{
		Raw: u.raw, MediaType: u.mediaType, Digest: u.digest(), Size: int64(len(u.raw)),
	}, nil
}

func (u *fakeUpstream) StatBlob(
	_ context.Context, p Principal, _ UpstreamRef, digest string,
) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.statBlobs++

	if err := u.gate(p); err != nil {
		return 0, err
	}

	body, ok := u.blobs[digest]
	if !ok {
		return 0, ErrUpstreamNotFound
	}

	return int64(len(body)), nil
}

func (u *fakeUpstream) Blob(
	_ context.Context, p Principal, _ UpstreamRef, digest string,
) (io.ReadCloser, int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.blobGets++

	if err := u.gate(p); err != nil {
		return nil, 0, err
	}

	body, ok := u.blobs[digest]
	if !ok {
		return nil, 0, ErrUpstreamNotFound
	}

	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func (u *fakeUpstream) counts() (resolves, manifests, statBlobs, blobGets int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.resolves, u.manifests, u.statBlobs, u.blobGets
}

// setRaw replaces the manifest the upstream serves -- how a test repoints a tag.
func (u *fakeUpstream) setRaw(raw []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.raw = raw
}

func (u *fakeUpstream) setErr(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.err = err
}

// fixture is a Backend wired to fakes, plus the handles a test asserts on.
type fixture struct {
	backend  *Backend
	mux      *http.ServeMux
	reader   *fakeReader
	store    storage.Store
	upstream *fakeUpstream
	authn    *fakeAuthenticator
	resolver *fakeResolver
	logs     *bytes.Buffer
	metrics  *metrics.Metrics

	// clock is what b.now() reads, so a test advances past a TTL without sleeping.
	clock time.Time
}

// newFixture builds the read-path fixture: a real blob.Service with NO Txer (writes
// are the ingest tests' job) over a real local store and a fake metadata reader.
func newFixture(t *testing.T) *fixture {
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

	logs := &bytes.Buffer{}
	up := newFakeUpstream(testIndex(t), testIndexType)
	authn := &fakeAuthenticator{mu: sync.Mutex{}, principal: nil, err: errors.New("no such key"), seen: nil}
	resolver := &fakeResolver{route: testRoute(), found: true}

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	f := &fixture{
		backend: nil, mux: http.NewServeMux(), reader: reader, store: local,
		upstream: up, authn: authn, resolver: resolver, logs: logs, metrics: m,
		clock: time.Unix(1_700_000_000, 0).UTC(),
	}

	f.backend = New(deps, resolver, authn, up, Config{
		ExternalURL:  "https://bakery.example.com",
		UpstreamAuth: nil,
	})
	f.backend.now = func() time.Time { return f.clock }
	f.backend.Register(f.mux)

	return f
}

// seed writes bytes durably at their content address and registers the matching
// metadata row, so the read path serves them end to end.
func (f *fixture) seed(t *testing.T, namespace, key string, body []byte, ct string) storage.Key {
	t.Helper()

	w, err := f.store.Create(t.Context())
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	if _, err := w.Write(body); err != nil {
		t.Fatalf("write staged bytes: %v", err)
	}

	info, err := w.Commit(t.Context())
	if err != nil {
		t.Fatalf("commit staged bytes: %v", err)
	}

	f.reader.add(namespace, key, info.Key, info.Size, ct, f.clock)

	return info.Key
}

// do issues one request against the mounted mux.
func (f *fixture) do(method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)

	return w
}

// doForm issues a request with an optional urlencoded body -- the OAuth2 POST shape.
func (f *fixture) doForm(
	method, target string, headers map[string]string, form url.Values,
) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	for k, v := range headers {
		r.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)

	return w
}

// seedTag lands a manifest the way a completed ingest would: the bytes once, a
// `manifests` row keyed on the digest WE compute, and a `tags` row keyed on
// "<normalized-host>/<name>:<tag>" naming the same blob.
func (f *fixture) seedTag(t *testing.T, upstream, name, tag string, raw []byte) storage.Key {
	t.Helper()

	digest := storage.KeyOf(raw)

	f.seed(t, nsManifests, digest.String(), raw, testIndexType)
	f.seed(t, nsTags, tagKey(NormalizeUpstream(upstream), name, tag), raw, testIndexType)

	return digest
}

// seedBlob lands one blob under its own digest.
func (f *fixture) seedBlob(t *testing.T, body []byte) storage.Key {
	t.Helper()

	return f.seed(t, nsBlobs, storage.KeyOf(body).String(), body, "")
}

// hashOf is the digest hex of some bytes, for building an upstream fixture.
func hashOf(body []byte) string { return storage.KeyOf(body).String() }
