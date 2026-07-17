package bazel

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/storage"
)

// Fixed route/tenant identity the tests resolve to. The UUIDs need only be stable and
// comparable -- the fakes key on the slug, and the principal answers per-project.
var (
	testOrgID     = pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	testProjectID = pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
)

func testRoute(readAuth bool) cache.Route {
	return cache.Route{
		OrgID:            testOrgID,
		ProjectID:        testProjectID,
		Org:              "acme",
		Project:          "widget",
		BackendID:        7,
		Kind:             repository.BackendKindBazel,
		Enabled:          true,
		ReadAuthRequired: readAuth,
	}
}

// fakeResolver resolves exactly one (org, project) -> route. Anything else is a miss,
// which is how the tests drive the NotFound-for-unconfigured path.
type fakeResolver struct {
	org, project string
	route        cache.Route
	ok           bool
}

func (r fakeResolver) Resolve(
	_ context.Context, org, project string, _ repository.BackendKind,
) (cache.Route, bool) {
	if !r.ok || org != r.org || project != r.project {
		return cache.Route{}, false
	}

	return r.route, true
}

// fakePrincipal answers the two capability questions from static booleans.
type fakePrincipal struct{ read, write bool }

func (p fakePrincipal) CanReadProject(_, _ pgtype.UUID) bool  { return p.read }
func (p fakePrincipal) CanWriteProject(_, _ pgtype.UUID) bool { return p.write }

// fakeAuthn maps a token string to a principal. An unknown token is an error, which
// authorize collapses to Unauthenticated.
type fakeAuthn struct{ byToken map[string]fakePrincipal }

func (a fakeAuthn) AuthenticateToken(_ context.Context, token string) (Principal, error) {
	p, ok := a.byToken[token]
	if !ok {
		return nil, errNoCredential
	}

	return p, nil
}

// fakeReader is the metadata half of a read-only blob.Service. It counts nothing the
// bazel tests assert on, so it is the minimal shape: a map keyed by object key.
type fakeReader struct {
	rows map[string]repository.StatObjectRow
}

func newFakeReader() *fakeReader {
	return &fakeReader{rows: map[string]repository.StatObjectRow{}}
}

func (f *fakeReader) StatObject(
	_ context.Context, arg repository.StatObjectParams,
) (repository.StatObjectRow, error) {
	row, ok := f.rows[arg.Key]
	if !ok {
		return repository.StatObjectRow{}, pgx.ErrNoRows
	}

	return row, nil
}

func (f *fakeReader) StatObjectsBatch(
	_ context.Context, arg repository.StatObjectsBatchParams,
) ([]repository.StatObjectsBatchRow, error) {
	out := make([]repository.StatObjectsBatchRow, 0, len(arg.Keys))

	for _, k := range arg.Keys {
		row, ok := f.rows[k]
		if !ok {
			continue
		}

		out = append(out, repository.StatObjectsBatchRow{
			Key: k, Digest: row.Digest, SizeBytes: row.SizeBytes, UpdatedAt: row.UpdatedAt,
		})
	}

	return out, nil
}

func (f *fakeReader) add(key string, digest []byte, size int64) {
	f.rows[key] = repository.StatObjectRow{
		Digest:    digest,
		SizeBytes: size,
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
	}
}

// fakeStore is the byte half: an in-memory content-addressed blob store. Only the read
// side (Get) is exercised; the write side returns an error, which is fine because the
// bazel read-path tests never Put.
type fakeStore struct {
	bytesByDigest map[storage.Key][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{bytesByDigest: map[storage.Key][]byte{}}
}

func (s *fakeStore) put(k storage.Key, data []byte) { s.bytesByDigest[k] = data }

func (s *fakeStore) Create(context.Context) (storage.Writer, error) {
	return nil, io.ErrClosedPipe
}

func (s *fakeStore) Get(_ context.Context, k storage.Key) (io.ReadCloser, error) {
	data, ok := s.bytesByDigest[k]
	if !ok {
		return nil, storage.ErrNotFound
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStore) Stat(_ context.Context, k storage.Key) (storage.Info, error) {
	data, ok := s.bytesByDigest[k]
	if !ok {
		return storage.Info{}, storage.ErrNotFound
	}

	return storage.Info{Key: k, Size: int64(len(data))}, nil
}

func (s *fakeStore) Exists(_ context.Context, k storage.Key) (bool, error) {
	_, ok := s.bytesByDigest[k]

	return ok, nil
}

func (s *fakeStore) Delete(context.Context, storage.Key) error { return nil }

// testBackend builds a Backend whose blob.Service is read-only (nil Txer): every read
// path works against the fakes, and any write path returns a "read-only" error rather
// than panicking. That is exactly the coverage boundary -- write happy-paths need a
// real Postgres and live in the conformance gate.
func testBackend(t *testing.T, res fakeResolver, authn fakeAuthn) (*Backend, *fakeReader, *fakeStore) {
	t.Helper()

	reader := newFakeReader()
	store := newFakeStore()

	svc, err := blob.New(blob.Config{
		Reader:  reader,
		Storage: store,
		Metrics: testMetrics(),
	})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	deps := cache.Deps{Blobs: svc, Metrics: testMetrics(), Logger: testLogger()}

	b := &Backend{deps: deps, routes: res, authn: authn}

	return b, reader, store
}

// seedCAS seeds one CAS object (key == hex(sha256(data))) into both fakes and returns
// its Digest message.
func seedCAS(r *fakeReader, s *fakeStore, data []byte) *repb.Digest {
	d := storage.KeyOf(data)
	r.add(d.String(), d.Bytes(), int64(len(data)))
	s.put(d, data)

	return &repb.Digest{Hash: d.String(), SizeBytes: int64(len(data))}
}

// --- fake ByteStream streams --------------------------------------------------

type fakeReadServer struct {
	grpc.ServerStream

	ctx  context.Context
	sent [][]byte
}

func (f *fakeReadServer) Context() context.Context { return f.ctx }

func (f *fakeReadServer) Send(r *bspb.ReadResponse) error {
	f.sent = append(f.sent, append([]byte(nil), r.GetData()...))

	return nil
}

func (f *fakeReadServer) body() []byte {
	var out []byte
	for _, chunk := range f.sent {
		out = append(out, chunk...)
	}

	return out
}

type fakeWriteServer struct {
	grpc.ServerStream

	ctx    context.Context
	frames []*bspb.WriteRequest
	idx    int
	resp   *bspb.WriteResponse
}

func (f *fakeWriteServer) Context() context.Context { return f.ctx }

func (f *fakeWriteServer) Recv() (*bspb.WriteRequest, error) {
	if f.idx >= len(f.frames) {
		return nil, io.EOF
	}

	r := f.frames[f.idx]
	f.idx++

	return r, nil
}

func (f *fakeWriteServer) SendAndClose(r *bspb.WriteResponse) error {
	f.resp = r

	return nil
}
