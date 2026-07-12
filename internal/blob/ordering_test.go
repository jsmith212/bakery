package blob

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/storage"
)

// THE NEGATIVE-CACHE ORDERING BUG, deterministically.
//
// A lookup that read "absent" from Postgres must NOT be able to land its negative
// entry on top of a concurrent Put's positive one. If it can, the LRU serves the
// stale "absent" answer with zero further queries until eviction -- a permanent 404
// for an object that exists, which is exactly what a `bakery sstate push` landing
// during a BB_NUMBER_THREADS HEAD storm would trigger.
//
// The gate holds the probe open at precisely the moment its finding needs: statDB has
// already read the shard generation and issued its query (which will miss), and only
// THEN does an authoritative Put publish the positive entry. The generation guard must
// make the late negative fill a no-op.
func TestStat_StaleNegativeFillCannotClobberAConcurrentPut(t *testing.T) {
	const key = "universal/sstate:busybox-during-a-push"

	repo := newFakeReader() // empty: the probe will read ErrNoRows (a miss)
	repo.entered = make(chan struct{}, 1)
	repo.gate = make(chan struct{})

	svc := newTestService(t, repo, 1024)
	ref := testRef(key)

	// A: a HEAD that misses the LRU, reads the generation, then stalls inside the
	// (missing) probe.
	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		// Its own answer is a legitimate miss -- it genuinely was absent in this
		// probe's snapshot. What must NOT happen is that miss overwriting the Put below.
		if ok, err := svc.Exists(context.Background(), ref); err != nil || ok {
			t.Errorf("racing Exists() = %v, %v; want false, nil", ok, err)
		}
	}()

	<-repo.entered // A has read the generation and is now inside the probe.

	// A committed Put publishes the positive entry, exactly as blob.put does after its
	// transaction commits. This bumps the shard generation.
	digest := storage.KeyOf([]byte("the object that a concurrent push just landed"))
	svc.cache(ref, digest, 4096, true)

	close(repo.gate) // let A's probe return ErrNoRows and attempt its (stale) fill.
	wg.Wait()

	// THE ASSERTION: the positive entry stands, and it is served from the LRU with no
	// further query. With the unguarded `s.lru.put`, A's negative fill lands last and
	// this reads false.
	before := repo.queries.Load()

	ok, err := svc.Exists(context.Background(), ref)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}

	if !ok {
		t.Fatal("Exists() = false: a stale negative fill clobbered a concurrent Put -- permanent 404 for an object that exists")
	}

	if got := repo.queries.Load() - before; got != 0 {
		t.Errorf("post-race Exists() issued %d queries, want 0 -- the positive entry was not held in the LRU", got)
	}
}

// THE SINGLEFLIGHT CANCELLATION BUG, deterministically.
//
// A HEAD storm collapses 64 callers onto ONE probe. If that probe rides the leader's
// request context, the leader disconnecting (Ctrl-C, ingress idle timeout, HTTP/2
// RST_STREAM) cancels the probe and singleflight hands context.Canceled to every other
// caller -- whose own contexts are perfectly alive. On the sstate miss path that
// renders as a 500, and a 500 there breaks the build.
//
// Here the leader enters the flight, a batch of waiters with LIVE contexts join it,
// and only then is the leader's context cancelled. The waiters must still get their
// answer; only the leader sees its own cancellation.
func TestExists_LeaderCancellationDoesNotFailTheWaiters(t *testing.T) {
	const (
		key     = "universal/sstate:the-hot-setscene-object"
		waiters = 16
	)

	repo := newFakeReader()
	repo.add(key, digestOf(1), 4096)
	repo.entered = make(chan struct{}, 1)
	repo.gate = make(chan struct{})

	svc := newTestService(t, repo, 1024)
	ref := testRef(key)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	leaderErr := make(chan error, 1)

	go func() {
		_, err := svc.Exists(leaderCtx, ref)
		leaderErr <- err
	}()

	<-repo.entered // the leader is now the in-flight probe.

	// The waiters join the flight. Their contexts are alive and stay alive.
	var (
		wg       sync.WaitGroup
		okCount  = make(chan bool, waiters)
		errCount = make(chan error, waiters)
	)

	for range waiters {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ok, err := svc.Exists(context.Background(), ref)
			okCount <- ok
			errCount <- err
		}()
	}

	// Give the waiters time to join the still-open flight (it is blocked on the gate).
	time.Sleep(100 * time.Millisecond)

	// The leader disconnects. On the old code this cancels the shared probe.
	cancelLeader()

	// Let the (detached) probe finish. On the fixed code it never saw the leader's
	// cancellation; on the old code it already returned context.Canceled to everyone.
	close(repo.gate)

	wg.Wait()
	close(okCount)
	close(errCount)

	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("leader Exists() error = %v, want context.Canceled -- the leader must honour its own cancellation", err)
	}

	for err := range errCount {
		if err != nil {
			t.Errorf("a waiter with a live context got error %v -- one caller's disconnect failed the whole storm", err)
		}
	}

	for ok := range okCount {
		if !ok {
			t.Error("a waiter got Exists() = false for a present key -- the collapsed probe failed for a live caller")
		}
	}
}

// A per-caller deadline is still honoured against the detached flight: a caller whose
// context is already cancelled must not block on the shared probe.
func TestExists_CallerDeadlineIsHonouredAgainstTheFlight(t *testing.T) {
	const key = "universal/sstate:slow-backend"

	repo := newFakeReader()
	repo.add(key, digestOf(2), 128)
	repo.entered = make(chan struct{}, 1)
	repo.gate = make(chan struct{}) // never closed: the probe stays wedged

	svc := newTestService(t, repo, 1024)
	ref := testRef(key)

	// Start the flight and leave it wedged.
	go func() { _, _ = svc.Exists(context.Background(), ref) }()
	<-repo.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := svc.Exists(ctx, ref)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Exists() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exists() did not return when its own deadline expired -- it blocked on the shared flight")
	}
}
