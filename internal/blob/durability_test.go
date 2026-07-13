package blob

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// --- fsync-before-the-transaction -------------------------------------------
//
// The multi-GB object fsync must happen BEFORE blob.put opens the metadata
// transaction, not inside w.Commit under the digest advisory lock. Otherwise a
// handful of large concurrent PUTs pin every pool connection (there are 16) and the
// digest lock across seconds-long fsyncs, and every other Postgres user -- /readyz's
// ping, the next build's HEAD storm, every /api/v1 call -- starves. The recorder below
// watches the order of the two events and fails if the transaction opens before the
// staged bytes were synced.

type durabilityOrder struct {
	synced             atomic.Bool
	txOpened           atomic.Bool
	txOpenedBeforeSync atomic.Bool
}

// recordingStore wraps a real Store and notes when the staged bytes are Sync'd.
type recordingStore struct {
	storage.Store

	rec *durabilityOrder
}

func (s recordingStore) Create(ctx context.Context) (storage.Writer, error) {
	w, err := s.Store.Create(ctx)
	if err != nil {
		return nil, err
	}

	return recordingWriter{Writer: w, rec: s.rec}, nil
}

type recordingWriter struct {
	storage.Writer

	rec *durabilityOrder
}

func (w recordingWriter) Sync() error {
	if err := w.Writer.Sync(); err != nil {
		return err
	}

	w.rec.synced.Store(true)

	return nil
}

// recordingTxer wraps a real Txer and, at the instant a transaction opens, records
// whether the staged bytes were already durable.
type recordingTxer struct {
	inner Txer

	rec *durabilityOrder
}

func (t recordingTxer) Tx(ctx context.Context, fn func(*repository.Queries) error) error {
	t.rec.txOpened.Store(true)

	if !t.rec.synced.Load() {
		t.rec.txOpenedBeforeSync.Store(true)
	}

	return t.inner.Tx(ctx, fn)
}

func TestPut_FsyncHappensBeforeTheTransaction(t *testing.T) {
	f := newFixture(t)

	rec := &durabilityOrder{}

	svc, err := New(Config{
		Reader:    f.store,
		Tx:        recordingTxer{inner: f.store, rec: rec},
		Storage:   recordingStore{Store: f.bytes, rec: rec},
		Metrics:   metrics.New(),
		CacheSize: 64,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	res, err := svc.Put(t.Context(), f.ref("large-sstate-tarball"),
		bytes.NewReader([]byte("pretend this is multiple gigabytes")),
		PutOptions{Overwrite: false, Verify: NoVerify()})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	if !res.Created {
		t.Fatal("Put did not create the object row")
	}

	if !rec.txOpened.Load() {
		t.Fatal("the transaction never opened -- the test proved nothing")
	}

	if !rec.synced.Load() {
		t.Fatal("the staged bytes were never fsynced")
	}

	if rec.txOpenedBeforeSync.Load() {
		t.Fatal("the metadata transaction opened BEFORE the staged bytes were fsynced -- " +
			"the multi-GB fsync is running inside the tx under the digest advisory lock")
	}
}

// --- reap unlinks inside the lock-holding transaction -----------------------
//
// ReapDigest must run storage.Delete INSIDE its transaction, while the digest advisory
// lock is held. Committing before the unlink ("optimising" the unlink out of the tx)
// reopens the resurrection race: the reaper drops the blob row and releases the lock,
// a concurrent PUT of the same content takes the lock, sees no row, re-uploads and
// commits a live object -- and the reaper's late unlink then deletes bytes the live
// object names. That is dangling metadata, a permanent 500.
//
// This test forces the window deterministically: it blocks the reaper's unlink and,
// while it is blocked, proves a concurrent PUT of the same digest is STILL BLOCKED
// (which can only be true if the unlink is inside the lock-holding transaction).

type blockingDeleteStore struct {
	storage.Store

	entered chan struct{}
	release chan struct{}
}

func (s *blockingDeleteStore) Delete(ctx context.Context, k storage.Key) error {
	s.entered <- struct{}{}
	<-s.release

	return s.Store.Delete(ctx, k)
}

func TestReapDigest_UnlinksInsideTheLockHoldingTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	content := []byte("reap me while a PUT of the same content races")
	digest := storage.KeyOf(content)

	bstore := &blockingDeleteStore{
		Store:   f.bytes,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	svc, err := New(Config{
		Reader:    f.store,
		Tx:        f.store,
		Storage:   bstore,
		Metrics:   metrics.New(),
		CacheSize: 64,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Land the blob, unreference it, and mark it pending_delete so a reap is legal.
	if _, err := svc.Put(ctx, f.ref("first"), bytes.NewReader(content),
		PutOptions{Overwrite: false, Verify: NoVerify()}); err != nil {
		t.Fatalf("seed Put() error = %v", err)
	}

	if _, err := svc.Delete(ctx, f.ref("first")); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if marked := f.mark(t, 0); len(marked) != 1 || marked[0] != digest {
		t.Fatalf("mark returned %v, want [%s]", marked, digest)
	}

	// The reaper runs; it will lock the digest, recheck, then block inside the unlink.
	type reapResult struct {
		reaped bool
		err    error
	}

	reapDone := make(chan reapResult, 1)

	go func() {
		reaped, rerr := svc.ReapDigest(ctx, digest)
		reapDone <- reapResult{reaped: reaped, err: rerr}
	}()

	<-bstore.entered // the reaper is now inside storage.Delete, holding the digest lock.

	// A concurrent PUT of the SAME digest. It must block on the advisory lock the
	// reaper holds -- which is only true if the unlink is inside that transaction.
	putDone := make(chan error, 1)

	go func() {
		_, perr := svc.Put(ctx, f.ref("second"), bytes.NewReader(content),
			PutOptions{Overwrite: false, Verify: NoVerify()})
		putDone <- perr
	}()

	select {
	case err := <-putDone:
		t.Fatalf("a concurrent PUT of the same digest completed (err=%v) while the reaper was mid-unlink -- "+
			"the unlink is NOT inside the lock-holding transaction; commit-then-unlink reopens the resurrection race", err)
	case <-time.After(300 * time.Millisecond):
		// Correct: the PUT is blocked on the lock the reaper's transaction holds.
	}

	close(bstore.release) // let the reaper finish its unlink and commit.

	got := <-reapDone
	if got.err != nil {
		t.Fatalf("ReapDigest() error = %v", got.err)
	}

	if !got.reaped {
		t.Fatal("ReapDigest() = false, want true")
	}

	// The blocked PUT now proceeds: it sees the reaped row is gone and re-uploads.
	if err := <-putDone; err != nil {
		t.Fatalf("the revived PUT error = %v", err)
	}

	// THE GLOBAL INVARIANT: the blob is live and its bytes are in storage. If the
	// unlink had raced outside the lock, the live row would name deleted bytes.
	refcount, state, ok := f.blobRow(t, digest)
	if !ok || state != "live" || refcount != 1 {
		t.Fatalf("after the race blob = (refcount %d, state %s, present %v), want (1, live, true)",
			refcount, state, ok)
	}

	if present, err := f.bytes.Exists(ctx, digest); !present || err != nil {
		t.Fatalf("a live blob's bytes are missing (Exists() = %v, %v) -- dangling metadata", present, err)
	}
}
