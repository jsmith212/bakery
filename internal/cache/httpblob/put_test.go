package httpblob

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// dbtest.Main is MANDATORY in every package that uses dbtest: it is the only place the
// shared Postgres container can be stopped after the last test. The read-path tests in
// this package do not need a DB, but the write path routes through blob.Service.Put,
// whose refcount protocol lives in Postgres triggers -- a fake cannot express it, so
// the PUT tests run against a real migrated database.
func TestMain(m *testing.M) { dbtest.Main(m) }

// putFixture is the shared write handler bound to a REAL blob.Service over a REAL
// migrated Postgres and a REAL local byte store, plus a resolved Route whose IDs match
// rows the fixture inserted (blob.Put's cache_objects insert has an FK to the backend).
type putFixture struct {
	handler http.Handler
	route   cache.Route
	svc     *blob.Service
}

func newPutFixture(t *testing.T, authn Authenticator) *putFixture {
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
		Kind:             repository.BackendKindSstate,
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
		Kind:             repository.BackendKindSstate,
		Enabled:          true,
		ReadAuthRequired: false,
	}

	deps := cache.Deps{
		Blobs:   svc,
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	NewSstate(deps, fakeResolver{route: route, found: true}, authn).Register(mux)

	return &putFixture{handler: mux, route: route, svc: svc}
}

// get reads an object straight from the backing service so a test can assert on the
// bytes the store actually holds, independent of the HTTP GET path.
func (f *putFixture) get(t *testing.T, key string) []byte {
	t.Helper()

	ref := f.route.Ref("", "object", key)

	_, rc, err := f.svc.Get(t.Context(), ref)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}

	return body
}

func put(t *testing.T, h http.Handler, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPut, "/cache/acme/widget/sstate/"+key, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	return rec
}

// TestPutCreatesThenIsIdempotent is the headline write contract:
//
//   - a first PUT with a WRITE-scoped key -> 201, and the bytes are retrievable;
//   - a second PUT of the SAME key -> 200 (idempotent no-op), NEVER 409;
//   - that second PUT sending DIFFERENT bytes does NOT swap the stored content
//     (Overwrite == false): the original bytes stand.
func TestPutCreatesThenIsIdempotent(t *testing.T) {
	t.Parallel()

	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true}}
	f := newPutFixture(t, authn)

	const key = "universal/aa/bb/sstate:zlib:x86_64.tar.zst"
	first := []byte("the original object bytes")

	if rec := put(t, f.handler, key, first); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT status = %d, want 201", rec.Code)
	}

	if got := f.get(t, key); !bytes.Equal(got, first) {
		t.Fatalf("stored bytes = %q, want %q", got, first)
	}

	// A second PUT of the same key with DIFFERENT bytes: idempotent 200, and the
	// original content must stand -- no swap, no 409.
	second := []byte("COMPLETELY different bytes that must NOT overwrite")

	rec := put(t, f.handler, key, second)
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d, want 200 (idempotent, never 409)", rec.Code)
	}

	if got := f.get(t, key); !bytes.Equal(got, first) {
		t.Errorf("after re-PUT stored bytes = %q, want the ORIGINAL %q (no content swap)", got, first)
	}
}

// TestPutAlwaysRequiresAKey is the cache-poisoning defense: even on an open-read backend
// (ReadAuthRequired == false), a write demands a valid WRITE-scoped credential.
//
//   - anonymous / no credential -> 401 + WWW-Authenticate;
//   - a valid READ-scoped key (cannot write) -> 403.
func TestPutAlwaysRequiresAKey(t *testing.T) {
	t.Parallel()

	const key = "universal/aa/bb/sstate:zlib:x86_64.tar.zst"

	t.Run("anonymous PUT on an open-read backend is 401", func(t *testing.T) {
		t.Parallel()

		// The backend row has ReadAuthRequired == false, yet an anonymous write is
		// rejected: authenticate returns an error, so the handler 401s.
		authn := &fakeAuthenticator{err: errUnauth{}}
		f := newPutFixture(t, authn)

		rec := put(t, f.handler, key, []byte("poison"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous PUT status = %d, want 401", rec.Code)
		}

		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="bakery"` {
			t.Errorf("WWW-Authenticate = %q, want Basic realm=\"bakery\"", got)
		}
	})

	t.Run("valid read-scoped key PUT is 403", func(t *testing.T) {
		t.Parallel()

		// Authenticates fine, but the principal can read and not write this project.
		authn := &fakeAuthenticator{principal: fakePrincipal{canRead: true, canWrite: false}}
		f := newPutFixture(t, authn)

		rec := put(t, f.handler, key, []byte("poison"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-scoped PUT status = %d, want 403", rec.Code)
		}
	})
}

// TestPutStreamsLargeBody proves the body is streamed through blob.Service.Put (which
// io.Copy's r.Body into a staging file) rather than buffered whole: a multi-megabyte
// body round-trips byte-for-byte. The handler passes r.Body straight to Put; there is
// no io.ReadAll and no small MaxBytesReader cap on the sstate path.
func TestPutStreamsLargeBody(t *testing.T) {
	t.Parallel()

	authn := &fakeAuthenticator{principal: fakePrincipal{canWrite: true}}
	f := newPutFixture(t, authn)

	// 8 MiB of random bytes -- far larger than any incidental buffer, and random so a
	// truncated or re-chunked copy is caught by the exact-bytes comparison.
	big := make([]byte, 8<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("rand: %v", err)
	}

	const key = "universal/cc/dd/sstate:big:x86_64.tar.zst"

	if rec := put(t, f.handler, key, big); rec.Code != http.StatusCreated {
		t.Fatalf("large PUT status = %d, want 201", rec.Code)
	}

	if got := f.get(t, key); !bytes.Equal(got, big) {
		t.Errorf("large object round-trip mismatch: got %d bytes, want %d", len(got), len(big))
	}
}
