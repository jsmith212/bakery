package blob

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// deleteRefs builds a one-backend, one-namespace batch out of a fake repository's keys.
func deleteRefs(keys []string, digests [][]byte) []DeleteRef {
	out := make([]DeleteRef, 0, len(keys))

	for i, k := range keys {
		var d Digest
		copy(d[:], digests[i])

		out = append(out, DeleteRef{Ref: testRef(k), Digest: d})
	}

	return out
}

// THE INVALIDATION, and it is the entire reason DeleteBatch exists rather than the GC
// calling DeleteObjectsChunk.
//
// The LRU serves negative AND positive answers with zero database contact, so a delete
// that does not invalidate leaves a POSITIVE entry naming a digest whose row is gone --
// every subsequent HEAD answers "present" from memory, every GET then hits
// ErrDanglingMetadata once the bytes are reaped, and no query is issued that could ever
// correct it. The assertion is therefore on the query COUNT as much as on the answer:
// the next lookup has to reach Postgres.
func TestDeleteBatchInvalidatesTheLRU(t *testing.T) {
	repo := newFakeReader()
	repo.add(sstateKey(1), digestOf(1), 4096)

	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, _ := newToucherService(t, repo, db)

	if ok, err := svc.Exists(t.Context(), testRef(sstateKey(1))); err != nil || !ok {
		t.Fatalf("warm Exists() = %v, %v; want true, nil", ok, err)
	}

	warm := repo.queries.Load()

	n, err := svc.DeleteBatch(t.Context(), deleteRefs([]string{sstateKey(1)}, [][]byte{digestOf(1)}))
	if err != nil {
		t.Fatalf("DeleteBatch() error = %v", err)
	}

	if n != 1 {
		t.Errorf("DeleteBatch() = %d rows, want 1", n)
	}

	ok, err := svc.Exists(t.Context(), testRef(sstateKey(1)))
	if err != nil {
		t.Fatalf("Exists() after DeleteBatch error = %v", err)
	}

	if ok {
		t.Error("Exists() = true after DeleteBatch -- the LRU still serves the deleted object")
	}

	if got := repo.queries.Load() - warm; got != 1 {
		t.Errorf("%d queries after DeleteBatch, want 1 -- the lookup was answered from a stale cache entry", got)
	}
}

// 40P01 IS A NORMAL OUTCOME, NOT A FAILURE. The batch delete already sorts by digest so
// it cannot deadlock the refcount trigger's paired locks, but a concurrent /ac
// overwrite takes its own two locks on its own schedule, so the loser of a real race is
// aborted by Postgres and has nothing to unwind. Retrying is the correct response;
// surfacing it to the GC as an error would abandon a whole chunk over a contention
// event that resolves itself.
func TestDeleteBatchRetriesDeadlock(t *testing.T) {
	deadlock := &pgconn.PgError{Code: pgerrcode.DeadlockDetected, Message: "deadlock detected"}

	tests := []struct {
		name      string
		errs      []error
		wantCalls int
		wantErr   bool
	}{
		{name: "retried once", errs: []error{deadlock}, wantCalls: 2, wantErr: false},
		{name: "retried twice", errs: []error{deadlock, deadlock}, wantCalls: 3, wantErr: false},
		{
			name:      "gives up bounded",
			errs:      []error{deadlock, deadlock, deadlock},
			wantCalls: deleteBatchAttempts,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeReader()
			repo.add(sstateKey(1), digestOf(1), 4096)

			db := &fakeDBTX{execs: nil, errs: tc.errs, reader: repo, onExec: nil}
			svc, _ := newToucherService(t, repo, db)

			_, err := svc.DeleteBatch(t.Context(), deleteRefs([]string{sstateKey(1)}, [][]byte{digestOf(1)}))

			if tc.wantErr {
				if err == nil {
					t.Fatal("DeleteBatch() error = nil, want the deadlock to surface after the bounded retry")
				}

				if !isDeadlock(err) {
					t.Errorf("DeleteBatch() error = %v, want it to still carry the 40P01", err)
				}
			} else if err != nil {
				t.Fatalf("DeleteBatch() error = %v", err)
			}

			if got := len(db.calls("DeleteObjectsByKeys")); got != tc.wantCalls {
				t.Errorf("DELETE attempts = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// DeleteObjectsByKeys is scoped to ONE (backend, namespace) and every sweep stage
// already works one namespace at a time, so a mixed batch is a caller bug. It is caught
// BEFORE the transaction: a batch that silently deleted only its first namespace's
// share would leave the rest of the chunk in the LRU as valid positive entries.
func TestDeleteBatchRejectsAMixedBatch(t *testing.T) {
	repo := newFakeReader()
	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, _ := newToucherService(t, repo, db)

	a := DeleteRef{Ref: testRef(sstateKey(1)), Digest: Digest{}}

	b := a
	b.Namespace = "cas"

	if _, err := svc.DeleteBatch(t.Context(), []DeleteRef{a, b}); !errors.Is(err, ErrMixedDeleteBatch) {
		t.Fatalf("DeleteBatch() error = %v, want ErrMixedDeleteBatch", err)
	}

	if got := len(db.calls("DeleteObjectsByKeys")); got != 0 {
		t.Errorf("a rejected batch still issued %d DELETEs", got)
	}
}

// THE DEADLOCK REGRESSION, against a REAL Postgres because the property under test is
// the DATABASE's: two concurrent batch deletes over the SAME deduped digests, driven in
// opposite key order, must not deadlock. The refcount trigger takes its paired blob
// locks in ascending digest order precisely to kill an ABBA; DeleteObjectsByKeys orders
// its driving set by digest for the same reason, a thousand digests wide.
//
// Both batches name the same content through two different backends, which is what
// dedup produces in production and what makes them contend on the same blobs rows at
// all -- delete two disjoint digest sets and this test passes with the ORDER BY
// deleted.
func TestDeleteBatchConcurrentOverlapDoesNotDeadlock(t *testing.T) {
	const objects = 40

	f := newFixture(t)
	ctx := t.Context()

	var projectID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT project_id FROM cache_backends WHERE id = $1`, f.backendID,
	).Scan(&projectID); err != nil {
		t.Fatalf("read project id: %v", err)
	}

	second, err := f.store.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        projectID,
		Kind:             repository.BackendKindDownloads,
		Enabled:          true,
		ReadAuthRequired: true,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend() error = %v", err)
	}

	secondRef := func(key string) Ref {
		r := f.ref(key)
		r.BackendID = second.ID
		r.Backend = metrics.BackendDownloads

		return r
	}

	// The same bytes under both backends: one blob row per object, refcount 2.
	left := make([]DeleteRef, 0, objects)
	right := make([]DeleteRef, 0, objects)

	for i := range objects {
		key := fmt.Sprintf("object-%03d", i)
		content := []byte(fmt.Sprintf("content for %d", i))

		res := f.put(t, key, content, PutOptions{Overwrite: false, Verify: NoVerify(), ContentType: ""})

		if _, err := f.svc.Put(ctx, secondRef(key), bytes.NewReader(content), PutOptions{
			Overwrite: false, Verify: NoVerify(), ContentType: "",
		}); err != nil {
			t.Fatalf("Put(second backend, %q) error = %v", key, err)
		}

		left = append(left, DeleteRef{Ref: f.ref(key), Digest: res.Digest})
		right = append(right, DeleteRef{Ref: secondRef(key), Digest: res.Digest})
	}

	// Opposite caller order: if lock order followed the caller's slice, this is the
	// ABBA.
	for i, j := 0, len(right)-1; i < j; i, j = i+1, j-1 {
		right[i], right[j] = right[j], right[i]
	}

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2)
	)

	for _, batch := range [][]DeleteRef{left, right} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := f.svc.DeleteBatch(ctx, batch); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent DeleteBatch: %v", err)
	}

	var remaining int64
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM cache_objects WHERE backend_id = ANY($1::bigint[])`,
		[]int64{f.backendID, second.ID},
	).Scan(&remaining); err != nil {
		t.Fatalf("count objects: %v", err)
	}

	if remaining != 0 {
		t.Errorf("cache_objects rows remaining = %d, want 0", remaining)
	}

	// The refcount is the trigger's, never Go's: both deletes must have decremented it
	// to zero, which is what makes the blob reapable at all.
	var unreferenced int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount <> 0`).Scan(&unreferenced); err != nil {
		t.Fatalf("count blobs: %v", err)
	}

	if unreferenced != 0 {
		t.Errorf("%d blobs still carry a refcount after every naming object was deleted", unreferenced)
	}
}
