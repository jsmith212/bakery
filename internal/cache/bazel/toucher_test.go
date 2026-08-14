package bazel

import (
	"bytes"
	"context"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// dbtest.Main is MANDATORY in every package that uses dbtest: it is Go's only
// after-the-last-test hook, so it is the only correct place to stop the shared
// Postgres container.
func TestMain(m *testing.M) { dbtest.Main(m) }

const acWriteToken = "bkry_wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww"

// acFixture is a Backend over a REAL Postgres and a REAL local byte store. The
// reachability touch (spec 6.3) is a property of blob.Service's accessed_at column
// and its in-memory pending set, and no fake reader/store can assert on either.
type acFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	blobs *blob.Service
	b     *Backend
	route cache.Route
}

func newACFixture(t *testing.T, readAuthRequired bool) *acFixture {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	ctx := t.Context()

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	mx := metrics.New()

	blobs, err := blob.New(blob.Config{
		Reader: store, Tx: store, Storage: local, Metrics: mx, CacheSize: 4096, Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("blob.New() error = %v", err)
	}

	org, err := store.CreateOrganization(ctx, repository.CreateOrganizationParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrganization() error = %v", err)
	}

	project, err := store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	cb, err := store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID: project.ID, Kind: repository.BackendKindBazel, Enabled: true,
		ReadAuthRequired: readAuthRequired, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend() error = %v", err)
	}

	route := cache.Route{
		OrgID: org.ID, ProjectID: project.ID, Org: "acme", Project: "widget",
		BackendID: cb.ID, Kind: repository.BackendKindBazel,
		Enabled: true, ReadAuthRequired: readAuthRequired,
	}

	deps := cache.Deps{Blobs: blobs, Metrics: mx, Logger: testLogger()}
	res := fakeResolver{org: "acme", project: "widget", route: route, ok: true}
	authn := fakeAuthn{byToken: map[string]fakePrincipal{acWriteToken: {read: true, write: true}}}

	b := &Backend{deps: deps, routes: res, authn: authn}

	return &acFixture{t: t, pool: pool, blobs: blobs, b: b, route: route}
}

// putCASBlob seeds a CAS object DIRECTLY through blob.Service.Put -- deliberately
// bypassing FindMissingBlobs/BatchUpdateBlobs, because the whole point of this test
// is what a GetActionResult HIT does BY ITSELF, with zero explicit reads of the
// outputs it names. It returns the hex digest a repb.Digest would carry.
func (f *acFixture) putCASBlob(content string) string {
	f.t.Helper()

	data := []byte(content)
	key := storage.KeyOf(data)
	ref := f.route.Ref(casNamespace, casKind, key.String())

	if _, err := f.blobs.Put(f.t.Context(), ref, bytes.NewReader(data), blob.PutOptions{
		Overwrite: false, Verify: blob.VerifyDigest(key),
	}); err != nil {
		f.t.Fatalf("seed CAS blob: Put() error = %v", err)
	}

	return key.String()
}

// pendingCAS reports the §6.2 veto's answer for one output digest.
func (f *acFixture) pendingCAS(hexDigest string) bool {
	f.t.Helper()

	return f.blobs.PendingTouch(f.route.BackendID, casNamespace, hexDigest)
}

// accessedAt reads back cache_objects.accessed_at for one CAS key. ok is false when
// no such row exists.
func (f *acFixture) accessedAt(hexDigest string) (ts pgtype.Timestamptz, ok bool) {
	f.t.Helper()

	err := f.pool.QueryRow(f.t.Context(),
		`SELECT accessed_at FROM cache_objects WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		f.route.BackendID, casNamespace, hexDigest).Scan(&ts)
	if err != nil {
		return pgtype.Timestamptz{}, false
	}

	return ts, true
}

// flushNow forces blob.Service's final (all=true, staleness-unguarded) flush by
// starting the toucher with an interval far longer than the test and then
// immediately cancelling -- the same technique blob/toucher_test.go's
// TestToucherFinalFlushOnShutdown uses, since flushAccess itself is unexported
// outside package blob.
func (f *acFixture) flushNow() {
	f.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		f.blobs.StartAccessToucher(ctx, time.Hour, func() time.Duration { return time.Hour })
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.t.Fatal("StartAccessToucher did not return after its context was cancelled")
	}
}

// THE REACHABILITY TOUCH (spec 6.3). An ActionResult HIT must keep its output
// digests' accessed_at fresh even though this test never reads them through any
// other RPC -- exactly the --remote_download_minimal steady state the spec section
// exists to cover.
func TestACHitTouchesItsOutputs(t *testing.T) {
	f := newACFixture(t, false) // ReadAuthRequired=false: GetActionResult needs no credential

	fileDigest := f.putCASBlob("output file contents")
	treeDigest := f.putCASBlob("encoded Tree proto contents")
	stdoutDigest := f.putCASBlob("stdout contents")
	untouchedDigest := f.putCASBlob("never named by any ActionResult")

	// Freshly Put blobs are unmarked: Put's LRU fill is UNSTAMPED (only a cold-read
	// FILL or an LRU HIT stamps markedAt). This is the test's own sanity check that
	// what follows is caused by GetActionResult, not by the seeding Put calls.
	for _, d := range []string{fileDigest, treeDigest, stdoutDigest} {
		if f.pendingCAS(d) {
			t.Fatalf("PendingTouch(%q) = true before any ActionResult was read", d)
		}
	}

	ar := &repb.ActionResult{
		ExitCode: 0,
		OutputFiles: []*repb.OutputFile{
			{Path: "out.bin", Digest: &repb.Digest{Hash: fileDigest, SizeBytes: 21}},
		},
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "outdir", TreeDigest: &repb.Digest{Hash: treeDigest, SizeBytes: 28}},
		},
		StdoutDigest: &repb.Digest{Hash: stdoutDigest, SizeBytes: 14},
	}

	if _, err := f.b.UpdateActionResult(bearerCtx(acWriteToken), &repb.UpdateActionResultRequest{
		InstanceName: "acme/widget",
		ActionDigest: &repb.Digest{Hash: storage.KeyOf([]byte("action-1")).String(), SizeBytes: 1},
		ActionResult: ar,
	}); err != nil {
		t.Fatalf("UpdateActionResult() error = %v", err)
	}

	// The HIT. Zero output reads: no BatchReadBlobs, no ByteStream.Read, on any of
	// fileDigest/treeDigest/stdoutDigest -- GetActionResult alone is what must mark
	// them.
	got, err := f.b.GetActionResult(context.Background(), &repb.GetActionResultRequest{
		InstanceName: "acme/widget",
		ActionDigest: &repb.Digest{Hash: storage.KeyOf([]byte("action-1")).String(), SizeBytes: 1},
	})
	if err != nil {
		t.Fatalf("GetActionResult() error = %v", err)
	}

	if got.GetExitCode() != 0 || len(got.GetOutputFiles()) != 1 {
		t.Fatalf("GetActionResult() returned an unexpected result: %+v", got)
	}

	// Marked IMMEDIATELY: the veto must answer true before any flush ever runs, or a
	// sweep racing this RPC would delete outputs a HIT just named.
	for _, d := range []string{fileDigest, treeDigest, stdoutDigest} {
		if !f.pendingCAS(d) {
			t.Errorf("PendingTouch(%q) = false after an ActionResult HIT named it", d)
		}
	}

	if f.pendingCAS(untouchedDigest) {
		t.Error("PendingTouch(untouched) = true -- a blob no ActionResult named must not be marked")
	}

	// FLUSH: accessed_at must land in the database, which is what makes a §3
	// stage-6 age sweep (coalesce(accessed_at, created_at) < now()-W_cas) spare
	// these rows instead of reaping them on created_at alone.
	f.flushNow()

	for _, d := range []string{fileDigest, treeDigest, stdoutDigest} {
		ts, ok := f.accessedAt(d)
		if !ok {
			t.Fatalf("accessed_at row for %q vanished", d)
		}

		if !ts.Valid {
			t.Errorf("accessed_at(%q) is still NULL after the flush", d)
		} else if time.Since(ts.Time) > time.Minute {
			t.Errorf("accessed_at(%q) = %v, not recent", d, ts.Time)
		}

		if f.pendingCAS(d) {
			t.Errorf("PendingTouch(%q) = true after a successful flush", d)
		}
	}

	if ts, ok := f.accessedAt(untouchedDigest); ok && ts.Valid {
		t.Error("accessed_at(untouched) was set -- the flush touched a blob it should not have")
	}
}

// THE MISS PATH MUST NEVER TOUCH. A NotFound (a genuine miss, or an unparseable
// stored value) never got past the unmarshal in GetActionResult, so touchOutputs is
// never reached -- this asserts the observable half of that: nothing is marked.
func TestACMissTouchesNothing(t *testing.T) {
	f := newACFixture(t, false)

	untouchedDigest := f.putCASBlob("would be an output if any ActionResult named it")

	_, err := f.b.GetActionResult(context.Background(), &repb.GetActionResultRequest{
		InstanceName: "acme/widget",
		ActionDigest: &repb.Digest{Hash: storage.KeyOf([]byte("no-such-action")).String(), SizeBytes: 1},
	})

	st, ok := grpcstatus.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Fatalf("GetActionResult() on a miss = %v, want NotFound", err)
	}

	if f.pendingCAS(untouchedDigest) {
		t.Error("PendingTouch() = true after a MISS -- the miss path must never mark")
	}
}
