package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// dbtest.Main is MANDATORY in every package that uses dbtest: TestMain is Go's only
// after-the-last-test hook, so it is the only correct place to stop the Postgres
// container the package's tests share.
func TestMain(m *testing.M) { dbtest.Main(m) }

// fixture is a Service over a REAL Postgres and a REAL local store. The refcount
// protocol is about what a SECOND, CONCURRENT transaction observes and when it
// blocks; a fake cannot express that, so nothing in this file uses one.
type fixture struct {
	svc       *Service
	store     *db.Store
	pool      *pgxpool.Pool
	bytes     storage.Store
	backendID int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	m := metrics.New()
	bytesStore := storage.NewInstrumented(local, m, metrics.DriverLocal)

	svc, err := New(Config{
		Reader:    store,
		Tx:        store,
		Storage:   bytesStore,
		Metrics:   m,
		CacheSize: 4096,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := t.Context()

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

	backend, err := store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        project.ID,
		Kind:             repository.BackendKindSstate,
		Enabled:          true,
		ReadAuthRequired: true,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend() error = %v", err)
	}

	return &fixture{svc: svc, store: store, pool: pool, bytes: bytesStore, backendID: backend.ID}
}

func (f *fixture) ref(key string) Ref {
	return Ref{
		BackendID: f.backendID,
		Org:       "acme",
		Project:   "widget",
		Backend:   metrics.BackendSstate,
		Kind:      "object",
		Namespace: "",
		Key:       key,
	}
}

func (f *fixture) put(t *testing.T, key string, content []byte, opts PutOptions) PutResult {
	t.Helper()

	res, err := f.svc.Put(t.Context(), f.ref(key), bytes.NewReader(content), opts)
	if err != nil {
		t.Fatalf("Put(%q) error = %v", key, err)
	}

	return res
}

// blobRow reads the raw blob row. The refcount is maintained by a TRIGGER, so the
// tests assert on what the database actually holds, never on what Go thinks it wrote.
func (f *fixture) blobRow(t *testing.T, d Digest) (refcount int64, state string, ok bool) {
	t.Helper()

	err := f.pool.QueryRow(t.Context(),
		`SELECT refcount, state::text FROM blobs WHERE digest = $1`, d.Bytes(),
	).Scan(&refcount, &state)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false
	}

	if err != nil {
		t.Fatalf("read blob row: %v", err)
	}

	return refcount, state, true
}

// mark runs the GC's MARK phase: a gc_runs row (which freezes started_at and the
// snapshot -- the write barrier) plus MarkBlobsPendingDelete. The MARK is the GC's
// (M6); the UNLINK is blob.ReapDigest's, and this is how they compose.
func (f *fixture) mark(t *testing.T, grace time.Duration) []Digest {
	t.Helper()

	ctx := t.Context()

	run, err := f.store.StartGCRun(ctx, pgtype.Interval{
		Microseconds: grace.Microseconds(), Days: 0, Months: 0, Valid: true,
	})
	if err != nil {
		t.Fatalf("StartGCRun() error = %v", err)
	}

	rows, err := f.store.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{
		ID: run.ID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("MarkBlobsPendingDelete() error = %v", err)
	}

	err = f.store.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID: run.ID, Status: repository.GcRunStatusSucceeded,
		Error:          pgtype.Text{String: "", Valid: false},
		ObjectsDeleted: 0, BlobsMarked: int64(len(rows)), BlobsDeleted: 0, BytesReclaimed: 0,
	})
	if err != nil {
		t.Fatalf("FinishGCRun() error = %v", err)
	}

	out := make([]Digest, 0, len(rows))

	for _, r := range rows {
		d, err := storage.KeyFromBytes(r.Digest)
		if err != nil {
			t.Fatalf("KeyFromBytes() error = %v", err)
		}

		out = append(out, d)
	}

	return out
}

func TestPut_RoundTrip(t *testing.T) {
	f := newFixture(t)
	content := []byte("sstate tarball")

	res := f.put(t, "a/b/sstate:busybox", content, PutOptions{Overwrite: false, Verify: NoVerify()})
	if res.Deduped {
		t.Error("first Put deduped against an empty store")
	}

	if !res.Created {
		t.Error("first Put did not create the object row")
	}

	if res.Digest != storage.KeyOf(content) {
		t.Errorf("digest = %s, want %s", res.Digest, storage.KeyOf(content))
	}

	ok, err := f.svc.Exists(t.Context(), f.ref("a/b/sstate:busybox"))
	if err != nil || !ok {
		t.Fatalf("Exists() = %v, %v; want true, nil", ok, err)
	}

	meta, rc, err := f.svc.Get(t.Context(), f.ref("a/b/sstate:busybox"))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("Get() = %q, want %q", got, content)
	}

	if meta.Size != int64(len(content)) {
		t.Errorf("Meta.Size = %d, want %d", meta.Size, len(content))
	}

	refcount, state, ok := f.blobRow(t, res.Digest)
	if !ok {
		t.Fatal("no blob row after Put")
	}

	if refcount != 1 || state != "live" {
		t.Errorf("blob row = (refcount %d, state %s), want (1, live)", refcount, state)
	}
}

// Dedup: two different KEYS, identical CONTENT. The second write is staged, hashed,
// and discarded -- and the refcount, which only the trigger ever touches, is 2.
func TestPut_DedupElidesTheWrite(t *testing.T) {
	f := newFixture(t)
	content := []byte("identical content")

	first := f.put(t, "key-one", content, PutOptions{Overwrite: false, Verify: NoVerify()})
	second := f.put(t, "key-two", content, PutOptions{Overwrite: false, Verify: NoVerify()})

	if first.Deduped {
		t.Error("first Put reported dedup")
	}

	if !second.Deduped {
		t.Error("second Put of identical content did NOT dedup -- the bytes were written twice")
	}

	if first.Digest != second.Digest {
		t.Fatalf("digests differ: %s vs %s", first.Digest, second.Digest)
	}

	refcount, state, ok := f.blobRow(t, first.Digest)
	if !ok {
		t.Fatal("no blob row")
	}

	if refcount != 2 || state != "live" {
		t.Errorf("blob row = (refcount %d, state %s), want (2, live)", refcount, state)
	}

	// Both keys still serve the bytes.
	for _, key := range []string{"key-one", "key-two"} {
		_, rc, err := f.svc.Get(t.Context(), f.ref(key))
		if err != nil {
			t.Fatalf("Get(%q) error = %v", key, err)
		}

		_ = rc.Close()
	}
}

// Verification is a PER-CALL flag with NO SAFE DEFAULT: /cas/ and OCI verify
// key == sha256(body); sstate, downloads and /ac/ must not. A zero Verify is an
// error, not a default -- getting this backwards breaks three clients at once.
func TestPut_VerificationPolicy(t *testing.T) {
	f := newFixture(t)
	content := []byte("cas payload")
	right := storage.KeyOf(content)
	wrong := storage.KeyOf([]byte("something else"))

	tests := []struct {
		name    string
		key     string
		verify  Verify
		wantErr error
	}{
		{name: "unspecified is refused", key: "k1", verify: Verify{}, wantErr: ErrVerificationUnspecified},
		{name: "verified and correct", key: "k2", verify: VerifyDigest(right), wantErr: nil},
		{name: "verified and wrong", key: "k3", verify: VerifyDigest(wrong), wantErr: ErrDigestMismatch},
		// /ac/, sstate, downloads: the key is opaque and is NOT the digest.
		{name: "opaque key accepted", key: "sstate:busybox", verify: NoVerify(), wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Put(t.Context(), f.ref(tt.key), bytes.NewReader(content),
				PutOptions{Overwrite: false, Verify: tt.verify})

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Put() error = %v, want %v", err, tt.wantErr)
			}

			ok, existsErr := f.svc.Exists(t.Context(), f.ref(tt.key))
			if existsErr != nil {
				t.Fatalf("Exists() error = %v", existsErr)
			}

			if want := tt.wantErr == nil; ok != want {
				t.Errorf("Exists() = %v, want %v -- a rejected write left metadata behind", ok, want)
			}
		})
	}
}

// /ac/ is the ONE mutable namespace (ccache, sccache, moon-over-HTTP). Repointing a
// key must decrement the old blob and increment the new one ATOMICALLY -- the single
// easiest refcount bug to write in Go, which is exactly why Go does not write it: the
// trigger does.
func TestPut_OverwriteMovesTheRefcount(t *testing.T) {
	f := newFixture(t)

	ref := f.ref("0123456789abcdef")
	ref.Namespace = "ac"
	ref.Kind = "ac"
	ref.Backend = metrics.BackendBazel

	old := []byte("ccache result v1")
	fresh := []byte("ccache result v2")

	if _, err := f.svc.Put(t.Context(), ref, bytes.NewReader(old),
		PutOptions{Overwrite: true, Verify: NoVerify()}); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	if _, err := f.svc.Put(t.Context(), ref, bytes.NewReader(fresh),
		PutOptions{Overwrite: true, Verify: NoVerify()}); err != nil {
		t.Fatalf("overwrite Put() error = %v", err)
	}

	oldCount, oldState, ok := f.blobRow(t, storage.KeyOf(old))
	if !ok {
		t.Fatal("old blob row vanished; only the GC may delete blob rows")
	}

	if oldCount != 0 || oldState != "live" {
		t.Errorf("old blob = (refcount %d, state %s), want (0, live)", oldCount, oldState)
	}

	newCount, _, ok := f.blobRow(t, storage.KeyOf(fresh))
	if !ok {
		t.Fatal("no blob row for the new content")
	}

	if newCount != 1 {
		t.Errorf("new blob refcount = %d, want 1", newCount)
	}

	// The key now serves the NEW bytes.
	_, rc, err := f.svc.Get(t.Context(), ref)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if !bytes.Equal(got, fresh) {
		t.Errorf("Get() = %q, want %q -- the overwrite did not repoint the key", got, fresh)
	}
}

// [METADATA FIRST.] Delete removes the object row and NOTHING ELSE. The bytes stay --
// another project may reference the same blob, and cache_objects_blob_fk is ON DELETE
// RESTRICT, so the database itself refuses to lose them while any object names them.
func TestDelete_RemovesMetadataOnlyAndLeavesTheBytes(t *testing.T) {
	f := newFixture(t)
	content := []byte("shared bytes")

	res := f.put(t, "key-one", content, PutOptions{Overwrite: false, Verify: NoVerify()})
	f.put(t, "key-two", content, PutOptions{Overwrite: false, Verify: NoVerify()})

	deleted, err := f.svc.Delete(t.Context(), f.ref("key-one"))
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if !deleted {
		t.Error("Delete() reported no row deleted")
	}

	// The negative entry must be served from the LRU, not re-read from Postgres.
	ok, err := f.svc.Exists(t.Context(), f.ref("key-one"))
	if err != nil || ok {
		t.Errorf("Exists(deleted) = %v, %v; want false, nil", ok, err)
	}

	refcount, state, present := f.blobRow(t, res.Digest)
	if !present {
		t.Fatal("Delete removed the blob row -- only the GC may do that")
	}

	if refcount != 1 || state != "live" {
		t.Errorf("blob row = (refcount %d, state %s), want (1, live)", refcount, state)
	}

	if ok, err := f.bytes.Exists(t.Context(), res.Digest); !ok || err != nil {
		t.Errorf("Delete unlinked the bytes: Exists() = %v, %v", ok, err)
	}

	// ...and the surviving key still serves them.
	_, rc, err := f.svc.Get(t.Context(), f.ref("key-two"))
	if err != nil {
		t.Fatalf("Get(key-two) error = %v", err)
	}

	_ = rc.Close()
}

// The GRACE PERIOD, which is the part everyone believes is the whole mechanism. It is
// not -- but it does have to work: a blob unreferenced one second ago is not sweepable
// under an hour's grace, and IS sweepable under none.
func TestGracePeriodSparesARecentlyUnreferencedBlob(t *testing.T) {
	f := newFixture(t)
	content := []byte("just unreferenced")

	res := f.put(t, "gone", content, PutOptions{Overwrite: false, Verify: NoVerify()})

	if _, err := f.svc.Delete(t.Context(), f.ref("gone")); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if marked := f.mark(t, time.Hour); len(marked) != 0 {
		t.Errorf("an hour's grace marked %d blobs, want 0", len(marked))
	}

	if _, state, _ := f.blobRow(t, res.Digest); state != "live" {
		t.Errorf("state = %s, want live -- the grace period did not spare the blob", state)
	}

	marked := f.mark(t, 0)
	if len(marked) != 1 || marked[0] != res.Digest {
		t.Fatalf("zero grace marked %v, want [%s]", marked, res.Digest)
	}

	reaped, err := f.svc.ReapDigest(t.Context(), res.Digest)
	if err != nil {
		t.Fatalf("ReapDigest() error = %v", err)
	}

	if !reaped {
		t.Error("ReapDigest() = false, want true")
	}

	if _, _, present := f.blobRow(t, res.Digest); present {
		t.Error("blob row survived the reap")
	}

	if ok, err := f.bytes.Exists(t.Context(), res.Digest); ok || err != nil {
		t.Errorf("bytes survived the reap: Exists() = %v, %v", ok, err)
	}
}

// THE RESURRECTION RACE, deterministically.
//
// A blob is marked pending_delete. Before the GC unlinks it, a PUT arrives for the
// SAME digest. The PUT must see the tombstone, refuse to trust the bytes, and
// re-upload; the GC's recheck must then find zero rows and NOT unlink.
//
// This is why 'pending_delete' is a persisted state and not an in-memory work queue,
// and why the tombstone must outlive the bytes.
func TestReapDigest_RefusesToUnlinkARevivedBlob(t *testing.T) {
	f := newFixture(t)
	content := []byte("resurrect me")
	digest := storage.KeyOf(content)

	f.put(t, "first", content, PutOptions{Overwrite: false, Verify: NoVerify()})

	if _, err := f.svc.Delete(t.Context(), f.ref("first")); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if marked := f.mark(t, 0); len(marked) != 1 {
		t.Fatalf("mark returned %d blobs, want 1", len(marked))
	}

	// The PUT arrives while the tombstone stands. It must NOT dedup: the bytes may
	// already be gone.
	res := f.put(t, "second", content, PutOptions{Overwrite: false, Verify: NoVerify()})
	if res.Deduped {
		t.Error("a PUT deduped onto a pending_delete blob -- it trusted bytes the GC may have unlinked")
	}

	refcount, state, ok := f.blobRow(t, digest)
	if !ok || refcount != 1 || state != "live" {
		t.Fatalf("after revival blob = (refcount %d, state %s, present %v), want (1, live, true)",
			refcount, state, ok)
	}

	// The GC now tries to unlink. The refcount = 0 recheck must stop it dead.
	reaped, err := f.svc.ReapDigest(t.Context(), digest)
	if err != nil {
		t.Fatalf("ReapDigest() error = %v", err)
	}

	if reaped {
		t.Fatal("ReapDigest() unlinked a revived blob")
	}

	if ok, err := f.bytes.Exists(t.Context(), digest); !ok || err != nil {
		t.Fatalf("the revived blob's bytes are gone: Exists() = %v, %v -- this is dangling metadata", ok, err)
	}

	_, rc, err := f.svc.Get(t.Context(), f.ref("second"))
	if err != nil {
		t.Fatalf("Get() after revival error = %v", err)
	}

	_ = rc.Close()
}

// THE RACE. This is the test the whole package exists to pass.
//
// Concurrently, against a REAL Postgres, under -race:
//
//	INCREF   8 putters PUT objects that all reference ONE hot digest (the dedup path)
//	DECREF   the same putters immediately DELETE them again, so the refcount
//	         repeatedly falls to ZERO while other putters are still arriving
//	DELETE   a GC loop marks (refcount = 0 + grace + the write barrier, FOR UPDATE
//	         SKIP LOCKED) and then unlinks through blob.ReapDigest (digest advisory
//	         lock + a refcount = 0 recheck FOR UPDATE + storage.Delete INSIDE the
//	         transaction)
//
// with a ZERO grace period, so the GC is as adversarial as it can be: a blob is
// sweepable the instant its last reference goes, and it will be reaped out from under
// a PUT that is deduping onto it unless the protocol actually holds.
//
// THE ASSERTION THAT MATTERS is the Get immediately after each Put: the bytes MUST be
// there the instant the metadata is. That is dangling metadata -- a permanent 500 --
// and it is the one failure the ordering invariant exists to prevent.
//
// The test refuses to pass vacuously: if the GC never actually reaped anything, the
// storm never met the collector and the test proves nothing, so that is a failure too.
// (It was: an earlier draft anchored the digest with a permanent reference, which kept
// refcount >= 1 forever and meant the mark predicate never matched a single row.)
func TestRefcountRace_IncrefDecrefDelete(t *testing.T) {
	const (
		putters    = 8
		iterations = 25
	)

	f := newFixture(t)
	ctx := t.Context()

	// Two shapes of contention at once, alternating:
	//   even iterations -> ONE hot digest shared by every putter, so the digest
	//     advisory lock is contended and the dedup path is hammered;
	//   odd iterations  -> a digest private to each putter, whose refcount therefore
	//     cycles 0 <-> 1 constantly, which is what actually gives the GC something to
	//     reap mid-storm. Sharing one digest across 8 putters keeps it referenced
	//     almost continuously and the mark predicate almost never matches.
	shared := []byte("the hot blob every bitbake thread wants")
	private := func(p int) []byte { return fmt.Appendf(nil, "putter %d's own blob", p) }

	var (
		writers sync.WaitGroup // the putters
		gc      sync.WaitGroup // the collector, which runs until it is told to stop
		reaps   atomic.Int64   // bytes actually unlinked -- the anti-vacuity counter
		errs    = make(chan error, putters*iterations*2)
		stop    = make(chan struct{})
	)

	// INCREF / DECREF.
	for p := range putters {
		writers.Add(1)

		go func() {
			defer writers.Done()

			for i := range iterations {
				key := fmt.Sprintf("p%d-i%d", p, i)
				ref := f.ref(key)

				content := shared
				if i%2 == 1 {
					content = private(p)
				}

				if _, err := f.svc.Put(ctx, ref, bytes.NewReader(content),
					PutOptions{Overwrite: false, Verify: NoVerify()}); err != nil {
					errs <- fmt.Errorf("put %s: %w", key, err)

					return
				}

				// THE BYTES MUST BE THERE THE INSTANT THE METADATA IS.
				meta, rc, err := f.svc.Get(ctx, ref)
				if err != nil {
					errs <- fmt.Errorf("get %s: %w", key, err)

					return
				}

				n, err := io.Copy(io.Discard, rc)
				_ = rc.Close()

				if err != nil {
					errs <- fmt.Errorf("read %s: %w", key, err)

					return
				}

				if n != meta.Size {
					errs <- fmt.Errorf("read %s: %d bytes, want %d", key, n, meta.Size)

					return
				}

				if _, err := f.svc.Delete(ctx, ref); err != nil {
					errs <- fmt.Errorf("delete %s: %w", key, err)

					return
				}

				// A window for the collector to actually WIN: without it the digest is
				// referenced by somebody almost continuously, the mark predicate never
				// matches, and the storm never meets the GC at all.
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// DELETE: the GC, running flat out with zero grace.
	gc.Add(1)

	go func() {
		defer gc.Done()

		for {
			select {
			case <-stop:
				return
			default:
			}

			for _, d := range gcMark(ctx, f.store, errs) {
				reaped, err := f.svc.ReapDigest(ctx, d)
				if err != nil {
					errs <- fmt.Errorf("reap %s: %w", d, err)

					return
				}

				if reaped {
					reaps.Add(1)
				}
			}
		}
	}()

	// The putters finish; only THEN is the GC told to stop.
	writers.Wait()
	close(stop)
	gc.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("race: %v", err)
	}

	if got := reaps.Load(); got == 0 {
		t.Fatal("the GC reaped nothing: the storm never met the collector and this test proved nothing")
	} else {
		t.Logf("the GC unlinked %d blobs mid-storm", got)
	}

	// THE GLOBAL INVARIANT, checked against every blob row the storm left behind:
	//
	//   1. refcount == the number of objects that actually name the digest. The trigger
	//      is the only thing that ever wrote it; if it drifted, dedup would eventually
	//      unlink bytes a live object names.
	//   2. a 'live' row's BYTES ARE IN STORAGE. That is what 'live' MEANS. A live row
	//      whose bytes are gone is dangling metadata -- a permanent 500 -- and it is
	//      the single failure this entire protocol exists to make unreachable.
	rows, err := f.pool.Query(ctx, `SELECT digest, refcount, state::text FROM blobs`)
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}

	type blobState struct {
		digest   Digest
		refcount int64
		state    string
	}

	var blobs []blobState

	for rows.Next() {
		var (
			raw      []byte
			refcount int64
			state    string
		)

		if err := rows.Scan(&raw, &refcount, &state); err != nil {
			t.Fatalf("scan blob: %v", err)
		}

		d, err := storage.KeyFromBytes(raw)
		if err != nil {
			t.Fatalf("KeyFromBytes() error = %v", err)
		}

		blobs = append(blobs, blobState{digest: d, refcount: refcount, state: state})
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		t.Fatalf("read blobs: %v", err)
	}

	for _, b := range blobs {
		var objects int64

		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM cache_objects WHERE digest = $1`, b.digest.Bytes(),
		).Scan(&objects); err != nil {
			t.Fatalf("count objects: %v", err)
		}

		if b.refcount != objects {
			t.Errorf("%s: refcount = %d but %d objects name it -- the refcount drifted",
				b.digest, b.refcount, objects)
		}

		if b.state != "live" {
			continue
		}

		if ok, err := f.bytes.Exists(ctx, b.digest); !ok || err != nil {
			t.Errorf("%s: a LIVE blob row names bytes that are not in storage (Exists() = %v, %v) -- dangling metadata",
				b.digest, ok, err)
		}
	}
}

// gcMark is the GC's MARK phase, inline, so the race test drives the REAL queries --
// the grace period, both forms of the write barrier, and FOR UPDATE SKIP LOCKED --
// rather than a simplification of them. The mark is the GC's job (M6); the unlink is
// blob.ReapDigest's, and this is how the two compose.
func gcMark(ctx context.Context, store *db.Store, errs chan<- error) []Digest {
	run, err := store.StartGCRun(ctx, pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true})
	if err != nil {
		errs <- fmt.Errorf("start gc run: %w", err)

		return nil
	}

	rows, err := store.MarkBlobsPendingDelete(ctx, repository.MarkBlobsPendingDeleteParams{ID: run.ID, Limit: 50})
	if err != nil {
		errs <- fmt.Errorf("mark: %w", err)
	}

	if ferr := store.FinishGCRun(ctx, repository.FinishGCRunParams{
		ID: run.ID, Status: repository.GcRunStatusSucceeded,
		Error:          pgtype.Text{String: "", Valid: false},
		ObjectsDeleted: 0, BlobsMarked: int64(len(rows)), BlobsDeleted: 0, BytesReclaimed: 0,
	}); ferr != nil {
		errs <- fmt.Errorf("finish gc run: %w", ferr)
	}

	out := make([]Digest, 0, len(rows))

	for _, r := range rows {
		d, err := storage.KeyFromBytes(r.Digest)
		if err != nil {
			errs <- fmt.Errorf("digest: %w", err)

			continue
		}

		out = append(out, d)
	}

	return out
}

func TestPut_InvalidKey(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		// The cache_objects btree cannot hold an entry this long. Caught in Go so it
		// is a clean 400, not a runtime index failure discovered in production.
		{name: "too long", key: strings.Repeat("x", maxKeyLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Put(t.Context(), f.ref(tt.key), bytes.NewReader([]byte("x")),
				PutOptions{Overwrite: false, Verify: NoVerify()})
			if !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put() error = %v, want ErrInvalidKey", err)
			}
		})
	}
}

// The REAPI empty blob (e3b0c442..., size 0) MUST always report as present. A store
// that treats zero bytes as absent breaks every Bazel client at once, and the schema
// went out of its way to allow it (size_bytes >= 0, not > 0).
func TestPut_EmptyBlob(t *testing.T) {
	f := newFixture(t)

	res := f.put(t, "empty", []byte{}, PutOptions{Overwrite: false, Verify: NoVerify()})
	if res.Size != 0 {
		t.Errorf("size = %d, want 0", res.Size)
	}

	ok, err := f.svc.Exists(t.Context(), f.ref("empty"))
	if err != nil || !ok {
		t.Errorf("Exists(empty blob) = %v, %v; want true, nil", ok, err)
	}
}

// Dangling metadata is a 500, NOT a 404. A 404 would let a build silently rebuild
// from a corrupted cache forever; a 500 is loud, and the whole ordering invariant
// exists so that this error is never reachable in production.
func TestGet_DanglingMetadataIsAnError(t *testing.T) {
	f := newFixture(t)

	res := f.put(t, "orphan", []byte("bytes about to vanish"), PutOptions{Overwrite: false, Verify: NoVerify()})

	// Reach behind blob.Service and unlink the bytes: exactly what a GC bug, or a
	// half-applied ordering, would do.
	if err := f.bytes.Delete(t.Context(), res.Digest); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	_, _, err := f.svc.Get(t.Context(), f.ref("orphan"))
	if !errors.Is(err, ErrDanglingMetadata) {
		t.Errorf("Get() error = %v, want ErrDanglingMetadata", err)
	}

	if errors.Is(err, ErrNotFound) {
		t.Error("dangling metadata was reported as a cache MISS -- it must be a 500, not a 404")
	}
}
