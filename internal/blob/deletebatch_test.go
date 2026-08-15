package blob

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// fakeGCRunID stands in for the gc_runs row id in the fake-backed tests. It is only
// ever a query PARAMETER there -- the write barrier it drives is a property of the SQL,
// asserted against a real Postgres in TestGCWriteBarrierSparesAConcurrentBuild
// (internal/db) and its per-stage replicas. What these tests assert is that
// DeleteBatch carries it, unmodified, into the statement.
const fakeGCRunID int64 = 4242

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

	n, err := svc.DeleteBatch(t.Context(), fakeGCRunID, deleteRefs([]string{sstateKey(1)}, [][]byte{digestOf(1)}))
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

// THE PRE-LOCK, THE ORDER, AND THE RUN ID -- the three things DeleteBatch adds to a
// bare DELETE, all of them invisible in the row count it returns.
//
// LockBlobDigests must run FIRST and in the SAME transaction: a lock taken after the
// DELETE has already fired its refcount trigger orders nothing, and a lock taken in
// another transaction is released before this one needs it. The digests go up in
// ascending order because that is the order the trigger takes its own paired blob locks
// (000006), and matching it is what makes the two writers unable to form a cycle. And
// the run id has to reach the statement, because it is the only thing the write barrier
// is derived from -- a batch that quietly passed zero would delete nothing, forever,
// and look like a cache with nothing to collect.
func TestDeleteBatchPreLocksDigestsInDigestOrder(t *testing.T) {
	repo := newFakeReader()

	// Descending digests, so caller order and digest order disagree.
	keys := []string{sstateKey(1), sstateKey(2), sstateKey(3)}
	digests := [][]byte{{0x03}, {0x01}, {0x02}}

	for i, k := range keys {
		repo.add(k, digests[i], 4096)
	}

	db := &fakeDBTX{execs: nil, errs: nil, reader: repo, onExec: nil}
	svc, _ := newToucherService(t, repo, db)

	if _, err := svc.DeleteBatch(t.Context(), fakeGCRunID, deleteRefs(keys, digests)); err != nil {
		t.Fatalf("DeleteBatch() error = %v", err)
	}

	locks := db.calls("LockBlobDigests")
	if len(locks) != 1 {
		t.Fatalf("LockBlobDigests statements = %d, want 1", len(locks))
	}

	deletes := db.calls("DeleteObjectsByKeys")
	if len(deletes) != 1 {
		t.Fatalf("DeleteObjectsByKeys statements = %d, want 1", len(deletes))
	}

	// Order within the transaction: the pre-lock is only a pre-lock if it is first.
	if db.execs[0].sql != locks[0].sql {
		t.Error("the first statement of the transaction is not LockBlobDigests -- " +
			"a lock taken after the DELETE orders nothing")
	}

	locked, _ := locks[0].args[0].([][]byte)
	if len(locked) != len(digests) {
		t.Fatalf("locked %d digests, want %d", len(locked), len(digests))
	}

	for i := 1; i < len(locked); i++ {
		if bytes.Compare(locked[i-1], locked[i]) > 0 {
			t.Errorf("digests passed to LockBlobDigests are not ascending: %x then %x",
				locked[i-1], locked[i])
		}
	}

	// The DELETE's own driving set stays in CALLER order (the SQL sorts it); what must
	// travel is the run id, in the parameter position sqlc gave it.
	if got, _ := deletes[0].args[4].(int64); got != fakeGCRunID {
		t.Errorf("DeleteObjectsByKeys run_id = %v, want %d -- the write barrier has nothing to read",
			deletes[0].args[4], fakeGCRunID)
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

			_, err := svc.DeleteBatch(t.Context(), fakeGCRunID, deleteRefs([]string{sstateKey(1)}, [][]byte{digestOf(1)}))

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

	if _, err := svc.DeleteBatch(t.Context(), fakeGCRunID, []DeleteRef{a, b}); !errors.Is(err, ErrMixedDeleteBatch) {
		t.Fatalf("DeleteBatch() error = %v, want ErrMixedDeleteBatch", err)
	}

	if got := len(db.calls("DeleteObjectsByKeys")); got != 0 {
		t.Errorf("a rejected batch still issued %d DELETEs", got)
	}
}

// startGCRun opens a real gc_runs row and returns its id. DeleteObjectsByKeys reads
// started_at and snapshot BACK OUT OF THAT ROW (never passed from Go), so a batch
// delete without one deletes nothing at all -- correctly: the barrier fails closed.
// It is called AFTER the objects are committed, which is what puts them behind the
// barrier rather than in front of it.
func (f *fixture) startGCRun(t *testing.T) int64 {
	t.Helper()

	run, err := f.store.StartGCRun(t.Context(), repository.StartGCRunParams{
		GracePeriod: pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true},
		Trigger:     "interval",
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("StartGCRun() error = %v", err)
	}

	return run.ID
}

// deleteAttempts reads bakery_db_queries_total{query="DeleteObjectsByKeys"} off the
// Service's own pre-resolved counter child -- the one deleteBatchTx increments ONCE PER
// ATTEMPT, inside the transaction. So it is the statement count and the retry count at
// the same time, read from production instrumentation rather than from a test hook.
func deleteAttempts(t *testing.T, svc *Service) float64 {
	t.Helper()

	c, ok := svc.qDeleteByKeys.(prometheus.Metric)
	if !ok {
		t.Fatalf("qDeleteByKeys is %T, which cannot be read back", svc.qDeleteByKeys)
	}

	var m dto.Metric

	if err := c.Write(&m); err != nil {
		t.Fatalf("read bakery_db_queries_total: %v", err)
	}

	return m.GetCounter().GetValue()
}

// THE DEADLOCK REGRESSION, against a REAL Postgres because the property under test is
// the DATABASE's: two concurrent batch deletes over the SAME deduped digests, driven in
// opposite key order, must not deadlock. The refcount trigger takes its paired blob
// locks in ascending digest order precisely to kill an ABBA; LockBlobDigests pre-locks
// this transaction's blobs rows in that same order before the DELETE touches anything,
// which is what makes the two writers' lock orders agree by construction rather than by
// planner accident.
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

	// Every object is committed before the run starts, so all of them are behind both
	// halves of the write barrier and eligible for deletion.
	runID := f.startGCRun(t)

	attemptsBefore := deleteAttempts(t, f.svc)

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2)
	)

	for _, batch := range [][]DeleteRef{left, right} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := f.svc.DeleteBatch(ctx, runID, batch); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent DeleteBatch: %v", err)
	}

	// ZERO RETRIES, not merely "no error returned". The bounded 40P01 retry is BELT for
	// a concurrent /ac overwrite taking its own two locks on its own schedule -- it is
	// NOT the mechanism that makes two batch deletes safe against each other. If these
	// two ever deadlock, the explicit digest-ordered pre-lock has stopped working and
	// the retry is the only thing hiding it: this assertion is what stops a real
	// ordering regression from passing as green after a pause and a re-run, at a
	// thousand digests per chunk in production instead of forty here.
	if got := deleteAttempts(t, f.svc) - attemptsBefore; got != 2 {
		t.Errorf("DeleteObjectsByKeys statements = %v, want exactly 2 (one per batch) -- "+
			"a deadlock was detected and retried to green", got)
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

// THE WRITE BARRIER, BEHAVIORALLY, ON THE DELETE ITSELF (verify pass N4): the
// ...SparesAConcurrentBuild replica for DeleteObjectsByKeys. The scan happens, the
// row is OVERWRITTEN (an /ac PUT: PutObjectOverwritable refreshes created_at and
// live_xid), and only then does the delete run -- keyed on exactly that (backend,
// namespace, key). Without the barrier re-derived AT DELETE TIME the key match
// deletes the freshly-written ActionResult on the scan's stale evidence; with it,
// the overwritten row fails both halves and survives.
//
// Real Postgres, because the property under test is the statement's, not the
// caller's.
func TestDeleteBatchSparesAConcurrentOverwrite(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	acRef := f.ref("f00d" + strings.Repeat("0", 60))
	acRef.Namespace = "ac"

	old, err := f.svc.Put(ctx, acRef, bytes.NewReader([]byte("the scanned ActionResult")),
		PutOptions{Overwrite: true, Verify: NoVerify()})
	if err != nil {
		t.Fatalf("seed Put() error = %v", err)
	}

	// The "scan": the GC froze its run AFTER the seed existed, so the seed row is
	// squarely behind the barrier -- the vacuity guard below proves it.
	runID := f.startGCRun(t)

	var selectable bool
	if err := f.pool.QueryRow(ctx,
		`SELECT o.created_at < g.started_at
		        AND pg_visible_in_snapshot(o.live_xid::text::xid8, g.snapshot::pg_snapshot)
		   FROM cache_objects o, gc_runs g
		  WHERE o.backend_id = $1 AND o.namespace = 'ac' AND o.key = $2 AND g.id = $3`,
		acRef.BackendID, acRef.Key, runID,
	).Scan(&selectable); err != nil {
		t.Fatalf("vacuity probe: %v", err)
	}

	if !selectable {
		t.Fatal("vacuity guard: the seeded row is not behind the run's barrier, so this " +
			"test cannot distinguish the barrier from the key match")
	}

	// The concurrent build: Bazel UpdateActionResults the same key mid-sweep.
	fresh, err := f.svc.Put(ctx, acRef, bytesReaderOf("the freshly-written ActionResult"),
		PutOptions{Overwrite: true, Verify: NoVerify()})
	if err != nil {
		t.Fatalf("overwrite Put() error = %v", err)
	}

	n, err := f.svc.DeleteBatch(ctx, runID, []DeleteRef{{Ref: acRef, Digest: old.Digest}})
	if err != nil {
		t.Fatalf("DeleteBatch() error = %v", err)
	}

	if n != 0 {
		t.Fatalf("DeleteBatch() deleted %d row(s); the overwritten row fails both barrier "+
			"halves and must survive", n)
	}

	meta, err := f.svc.StatUncached(ctx, acRef)
	if err != nil || !meta.Exists {
		t.Fatalf("StatUncached() after the spared delete = (%+v, %v); want the row present", meta, err)
	}

	if meta.Digest != fresh.Digest {
		t.Errorf("surviving row's digest = %s, want the overwrite's %s", meta.Digest, fresh.Digest)
	}
}

// bytesReaderOf keeps the test above readable; bytes.NewReader wants []byte.
func bytesReaderOf(s string) *bytes.Reader { return bytes.NewReader([]byte(s)) }
