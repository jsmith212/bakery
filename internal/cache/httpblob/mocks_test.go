package httpblob

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
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
// A REAL blob.Service, driven at its two honest seams: a fake metadata Reader
// (the cache_objects probe) and a real local byte store. The handler, blob.Service
// and http.ServeContent are all real -- only Postgres and the DB-row source are
// faked, exactly where blob.Reader exists to be faked.
// ---------------------------------------------------------------------------

// fakeReader is the metadata store. It maps an object KEY to its row, and returns
// pgx.ErrNoRows (a cache MISS) for anything unseeded.
type fakeReader struct {
	rows map[string]repository.StatObjectRow
}

func newFakeReader() *fakeReader { return &fakeReader{rows: map[string]repository.StatObjectRow{}} }

func (f *fakeReader) StatObject(
	_ context.Context, arg repository.StatObjectParams,
) (repository.StatObjectRow, error) {
	row, ok := f.rows[arg.Key]
	if !ok {
		return repository.StatObjectRow{}, pgx.ErrNoRows
	}

	return row, nil
}

func (f *fakeReader) add(key string, digest storage.Key, size int64) {
	f.rows[key] = repository.StatObjectRow{
		Digest:    digest.Bytes(),
		SizeBytes: size,
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), InfinityModifier: 0, Valid: true},
	}
}

// countingStore wraps a real storage.Store and counts Get calls. It is how a test
// asserts the HEAD hot path NEVER opens the object bytes: a HEAD that streamed a file
// would bump gets, and the design forbids exactly that on the BB_NUMBER_THREADS storm.
type countingStore struct {
	inner storage.Store
	gets  atomic.Int64
}

func (c *countingStore) Create(ctx context.Context) (storage.Writer, error) {
	return c.inner.Create(ctx)
}

func (c *countingStore) Get(ctx context.Context, k storage.Key) (io.ReadCloser, error) {
	c.gets.Add(1)

	return c.inner.Get(ctx, k)
}

func (c *countingStore) Stat(ctx context.Context, k storage.Key) (storage.Info, error) {
	return c.inner.Stat(ctx, k)
}

func (c *countingStore) Exists(ctx context.Context, k storage.Key) (bool, error) {
	return c.inner.Exists(ctx, k)
}

func (c *countingStore) Delete(ctx context.Context, k storage.Key) error {
	return c.inner.Delete(ctx, k)
}

// testBlobs is a real blob.Service plus the handles a test needs to seed and assert.
type testBlobs struct {
	svc     *blob.Service
	reader  *fakeReader
	store   *countingStore
	metrics *metrics.Metrics
}

// newTestBlobs stands up a real blob.Service over a fresh temp-dir local store.
func newTestBlobs(t *testing.T) *testBlobs {
	t.Helper()

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	store := &countingStore{inner: local}
	reader := newFakeReader()
	m := metrics.New()

	svc, err := blob.New(blob.Config{Reader: reader, Tx: nil, Storage: store, Metrics: m})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	return &testBlobs{svc: svc, reader: reader, store: store, metrics: m}
}

// seed writes body durably through the REAL store (so the bytes exist at their content
// address) and registers a matching metadata row under key. The handler then serves it
// end to end.
func (b *testBlobs) seed(t *testing.T, key string, body []byte) {
	t.Helper()

	w, err := b.store.Create(t.Context())
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

	b.reader.add(key, info.Key, info.Size)
}

func (b *testBlobs) deps() cache.Deps {
	return cache.Deps{
		Blobs:   b.svc,
		Metrics: b.metrics,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// ---------------------------------------------------------------------------
// Route + auth fakes.
// ---------------------------------------------------------------------------

// fakeResolver returns a fixed Route (or not-found). It records nothing the handler
// must not do -- the point of driving the real handler is that the fake supplies only
// the resolved route, and the handler makes every status/auth decision itself.
type fakeResolver struct {
	route cache.Route
	found bool
}

func (f fakeResolver) Resolve(
	_ context.Context, _, _ string, _ repository.BackendKind,
) (cache.Route, bool) {
	return f.route, f.found
}

// testRoute is a resolved, enabled sstate route. orgID/projectID are fixed nonzero
// UUIDs so the authorization checks have something concrete to compare.
func testRoute() cache.Route {
	return cache.Route{
		OrgID:            uuid(0x0a),
		ProjectID:        uuid(0x1a),
		Org:              "acme",
		Project:          "widget",
		BackendID:        7,
		Kind:             repository.BackendKindSstate,
		Enabled:          true,
		ReadAuthRequired: false,
	}
}

// fakePrincipal answers exactly the two capability questions the read/write handler
// asks. It is a legal Principal because Principal is the narrow consumer-side
// interface, not auth's sealed one.
type fakePrincipal struct {
	canRead  bool
	canWrite bool
}

func (p fakePrincipal) CanReadProject(_, _ pgtype.UUID) bool  { return p.canRead }
func (p fakePrincipal) CanWriteProject(_, _ pgtype.UUID) bool { return p.canWrite }

// fakeAuthenticator returns a fixed principal or error, and records whether it was
// called -- so a test can assert an OPEN backend never authenticates at all.
type fakeAuthenticator struct {
	principal Principal
	err       error
	calls     atomic.Int64
}

func (a *fakeAuthenticator) AuthenticateCache(_ context.Context, _ *http.Request) (Principal, error) {
	a.calls.Add(1)

	return a.principal, a.err
}

// uuid makes a deterministic valid pgtype.UUID from a byte.
func uuid(b byte) pgtype.UUID {
	var out pgtype.UUID

	out.Valid = true
	for i := range out.Bytes {
		out.Bytes[i] = b
	}

	return out
}
