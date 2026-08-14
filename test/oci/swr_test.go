package ociconf

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// swrTTL is swrTagTTL parsed. The waits below are the TTL plus a margin; a margin that
// is too tight makes a flaky gate, and a gate that flakes gets disabled.
const swrTTL = 2 * time.Second

// swrSlack is added to every wait that must cross the TTL boundary.
const swrSlack = 750 * time.Millisecond

// TestStaleWhileRevalidate proves the tag freshness machine end to end, including the
// half the design memo got wrong.
//
// # What Bakery does, and why it is not what the ecosystem does
//
// registry:2's pull-through proxy resolves EVERY tag pull against the remote first,
// synchronously, falling back to local only when that fails -- so every pull of a cached
// image pays a full upstream round trip, and its "TTL" is a seven-day content eviction
// timer rather than a freshness horizon. That is the wrong trade for a build cache: the
// point of a mirror is to take the upstream off the hot path.
//
// Here a tag is FRESH for tag_ttl with zero upstream contact; once stale it is still
// served IMMEDIATELY from cache and revalidated in the background. The client never
// waits for the upstream.
//
// # The three things asserted, in order
//
//  1. STALE IS SERVED, NOT WITHHELD. After the TTL passes and the upstream tag has
//     moved, the very next request still gets the OLD digest -- immediately, from cache.
//     Serving nothing (or blocking on the upstream) would put a third party's latency,
//     and its outages, on every pull.
//
//  2. THE REPOINT LANDS. The background revalidation notices the tag moved and repoints
//     it. A broken refresh here serves an ancient `:latest` forever with perfectly
//     correct digests -- silent, and indistinguishable from a stable tag.
//
//  3. FRESHNESS IS RESTORED WHEN NOTHING CHANGED. This is the one the design memo
//     concluded by assumption and got wrong. A stable tag -- the overwhelmingly common
//     case -- revalidates to the SAME digest, which writes no bytes and repoints
//     nothing. Without an explicit touch, updated_at never advances: every tag is
//     permanently stale after its first TTL, every response is a stale response forever,
//     every request past the first kicks another upstream HEAD, and a real upstream
//     outage becomes indistinguishable from steady state. The assertion is a request
//     COUNT, because that is the only place the bug is visible.
func TestStaleWhileRevalidate(t *testing.T) {
	e := newEnv(t)

	url := e.family1(projSWR, manifestPath(repoMoving, tagMoving))
	auth := bearer(e.key(projSWR))

	first := request(t, http.MethodGet, url, auth)
	if first.status != http.StatusOK {
		t.Fatalf("cold GET %s = %d; body %s", url, first.status, first.body)
	}

	before := digestOf(first.body)

	// ---- 1. the upstream tag moves, and Bakery keeps serving the old digest.

	moved := e.up.seedImageFresh(t, repoMoving, tagMoving)
	if moved == before {
		t.Fatal("the repointed image has the same digest as the original -- the fixture is degenerate")
	}

	// Inside the TTL: still fresh, still the old digest, and no upstream contact at all.
	e.up.reset()

	if got := digestOf(request(t, http.MethodGet, url, auth).body); got != before {
		t.Errorf("a FRESH tag served %s, want the cached %s -- the TTL is not being honored", got, before)
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("a fresh tag made %d upstream request(s): %v", n, e.up.requests())
	}

	time.Sleep(swrTTL + swrSlack)

	stale := request(t, http.MethodGet, url, auth)
	if stale.status != http.StatusOK {
		t.Fatalf("stale GET %s = %d; body %s", url, stale.status, stale.body)
	}

	if got := digestOf(stale.body); got != before {
		t.Errorf("the first STALE response served %s, want the cached %s served immediately. "+
			"Blocking on the upstream, or withholding the stale copy, puts a third party's "+
			"latency and outages on every pull.", got, before)
	}

	// ---- 2. the background revalidation repoints the tag.

	if got := pollDigest(t, url, auth, moved); got != moved {
		t.Fatalf("the tag never repointed: still serving %s after the revalidation window, want %s",
			got, moved)
	}

	// ---- 3. an UNCHANGED revalidation restores freshness.

	time.Sleep(swrTTL + swrSlack)

	before3 := e.up.headCount()

	if got := digestOf(request(t, http.MethodGet, url, auth).body); got != moved {
		t.Fatalf("stale-again GET served %s, want %s", got, moved)
	}

	// Wait for that revalidation to reach the upstream and finish writing.
	waitFor(t, 10*time.Second, func() bool { return e.up.headCount() > before3 })
	time.Sleep(500 * time.Millisecond)

	settled := e.up.headCount()

	// Three requests, back to back, well inside the TTL of the touch that just happened.
	// If the unchanged branch had not bumped updated_at, all three would still be stale
	// and all three would kick another upstream HEAD -- the refresh path has no
	// singleflight of its own, so the count would climb by three.
	for range 3 {
		if got := request(t, http.MethodGet, url, auth); got.status != http.StatusOK {
			t.Fatalf("post-revalidation GET = %d; body %s", got.status, got.body)
		}
	}

	time.Sleep(250 * time.Millisecond)

	if got := e.up.headCount(); got != settled {
		t.Errorf("three requests after an UNCHANGED revalidation drove the upstream HEAD count "+
			"from %d to %d, want it unchanged.\nThe revalidation confirmed the tag had not "+
			"moved but did not restore its freshness, so the tag is now permanently stale: "+
			"every response is a stale response and every request revalidates, forever.",
			settled, got)
	}
}

// pollDigest re-requests a tag until it serves the wanted digest, or gives up. The
// refresh it is waiting on is asynchronous BY DESIGN -- the request that noticed the
// staleness was answered from cache and did not wait for it -- so polling is the honest
// way to observe it, and the bound is what turns "never repointed" into a failure rather
// than a hang.
func pollDigest(t *testing.T, url string, headers map[string]string, want string) string {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	got := ""

	for time.Now().Before(deadline) {
		res := request(t, http.MethodGet, url, headers)
		if res.status != http.StatusOK {
			t.Fatalf("GET %s = %d while polling; body %s", url, res.status, res.body)
		}

		got = digestOf(res.body)
		if got == want {
			return got
		}

		time.Sleep(100 * time.Millisecond)
	}

	return got
}

// waitFor blocks until cond holds, or fails the test.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("condition never held within %s", limit)
}

// headCount is how many manifest HEADs the upstream has served. A tag revalidation is a
// HEAD first -- free against Docker Hub's pull limit, verified live -- so this is the
// counter that makes "did it revalidate?" observable.
func (r *fakeRegistry) headCount() int {
	n := 0

	for _, req := range r.requests() {
		if strings.HasPrefix(req, http.MethodHead+" ") && strings.Contains(req, "/manifests/") {
			n++
		}
	}

	return n
}
