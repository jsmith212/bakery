package auth

import (
	"context"
	"encoding/binary"
	"hash/maphash"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The principal cache: a user's LIVE role set, keyed by (user_id, authz_epoch).
//
// # Why a cache exists at all
//
// A personal access token authorizes against the holder's live roles, and reading
// them is three queries (users, org_memberships, project_memberships). That is
// fine on the console, which is cold. It is not fine on the cache plane, which is
// a BB_NUMBER_THREADS-parallel sstate HEAD storm: three queries per HEAD would
// triple the database load of a build for a value that changes about once a
// quarter.
//
// # Why the key carries the epoch, and why that is the whole design
//
// The obvious cache is keyed on user_id and invalidated by calling Evict() from
// every path that mutates a role. That is a WHOLE-PROGRAM CLAIM maintained by
// hand: org grant, org revoke, org delete, project grant, project revoke, the
// reconciler's delete, the reconciler's upsert, the site-admin grant, the
// site-admin revoke, the FK cascades that fire without any Go code running at
// all. Miss one and a demoted user keeps their old authority for a TTL, silently,
// with no failing test -- and the cascades cannot be covered by an Evict call in
// the first place, because no Go statement executes for them.
//
// So the key carries `users.authz_epoch`, which the DATABASE bumps by trigger
// (migration 000015) on every membership insert/update/delete and every site-role
// change. A stale entry is never evicted; it becomes UNREACHABLE, because the next
// token probe reads a different epoch and therefore looks up a different key.
// There is no call site to forget. It also survives --allow-multi-instance, where
// an in-process Evict is not merely incomplete but wrong.
//
// # TTL is garbage collection, NOT the invalidation mechanism
//
// Nothing depends on the TTL for correctness. It exists so that entries for users
// who stopped using their token, and for epochs nobody will ever ask for again,
// do not accumulate forever. Deleting the TTL would leak memory; it would not
// make a single authorization decision wrong.
//
// # Sharded and negative-capable
//
// Sharded because a single-mutex map gets SLOWER as parallelism rises -- measured
// 170ns/op at -cpu 8 degrading to 246ns/op at -cpu 64 in the blob LRU work, which
// is exactly backwards under the storm this cache exists for.
//
// Negative-capable because "this user id has no row" is an answer, and re-asking
// the database for it on every request is precisely the miss storm the positive
// half is designed to avoid. It is reachable: a token whose owner was deleted
// between the probe's JOIN and this lookup.
const (
	// principalCacheShards. 16, not the blob LRU's 64: the working set here is
	// "humans with an active personal access token", which is orders of magnitude
	// smaller than "objects in the sstate cache", and each shard carries a map plus
	// a mutex whether or not it is used.
	principalCacheShards = 16

	// principalCacheTTL is the GC horizon. See the type doc: correctness does not
	// depend on it.
	principalCacheTTL = 30 * time.Second

	// principalCacheEntriesPerShard bounds memory. On overflow the shard drops its
	// expired entries first and, only if that is not enough, drops arbitrary live
	// ones -- a dropped entry costs three queries once, never a wrong answer.
	principalCacheEntriesPerShard = 512
)

// authzKey is the cache key. Both halves are comparable, so this is a legal map
// key and no string formatting happens on an authorization decision.
type authzKey struct {
	userID pgtype.UUID
	epoch  int64
}

type principalCache struct {
	shards [principalCacheShards]principalShard
	seed   maphash.Seed

	// ttl and now are injectable so the tests can drive expiry without sleeping.
	ttl time.Duration
	now func() time.Time
}

type principalShard struct {
	mu sync.Mutex
	m  map[authzKey]principalEntry
}

type principalEntry struct {
	// authz is nil for a NEGATIVE entry: "we asked, and there is no such user".
	authz   *userAuthz
	expires time.Time
}

func newPrincipalCache() *principalCache {
	c := &principalCache{
		shards: [principalCacheShards]principalShard{},
		seed:   maphash.MakeSeed(),
		ttl:    principalCacheTTL,
		now:    time.Now,
	}

	for i := range c.shards {
		c.shards[i].m = make(map[authzKey]principalEntry)
	}

	return c
}

// shardFor hashes the whole key, epoch included. Hashing the user id alone would
// pile every epoch of one very active user into one shard; including the epoch
// costs nothing and spreads them.
func (c *principalCache) shardFor(k authzKey) *principalShard {
	var buf [24]byte

	copy(buf[:16], k.userID.Bytes[:])
	binary.LittleEndian.PutUint64(buf[16:], uint64(k.epoch))

	return &c.shards[maphash.Bytes(c.seed, buf[:])%principalCacheShards]
}

// load returns the role set for (userID, epoch), reading through `fill` on a miss.
//
// A nil *userAuthz with a nil error means the user does not exist -- the caller
// must treat that as a refused credential, never as an empty role set.
//
// There is deliberately no singleflight here. The blob LRU has one because a
// thundering herd on a cold cache is its normal case; this cache's herd is
// bounded by the number of DISTINCT users pushing at once, which is small, and
// the duplicate work is three cheap indexed reads. A singleflight would add a
// second synchronization point on the hot path to save it.
func (c *principalCache) load(
	ctx context.Context,
	userID pgtype.UUID,
	epoch int64,
	fill func(ctx context.Context, userID pgtype.UUID) (*userAuthz, error),
) (*userAuthz, error) {
	key := authzKey{userID: userID, epoch: epoch}

	if authz, ok := c.get(key); ok {
		return authz, nil
	}

	authz, err := fill(ctx, userID)
	if err != nil {
		if isNoRows(err) {
			// A NEGATIVE entry, not a propagated error: the token's JOIN said this user
			// existed a moment ago, so the row was deleted underneath us. The credential
			// is dead either way, and caching that fact stops a revoked-by-deletion
			// token from re-asking the database on every request of a build.
			c.put(key, nil)

			return nil, nil //nolint:nilnil // nil,nil is the documented "no such user" answer.
		}

		return nil, err
	}

	c.put(key, authz)

	return authz, nil
}

func (c *principalCache) get(key authzKey) (*userAuthz, bool) {
	shard := c.shardFor(key)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	entry, ok := shard.m[key]
	if !ok {
		return nil, false
	}

	if c.now().After(entry.expires) {
		delete(shard.m, key)

		return nil, false
	}

	return entry.authz, true
}

func (c *principalCache) put(key authzKey, authz *userAuthz) {
	shard := c.shardFor(key)
	now := c.now()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if len(shard.m) >= principalCacheEntriesPerShard {
		shard.evictLocked(now)
	}

	shard.m[key] = principalEntry{authz: authz, expires: now.Add(c.ttl)}
}

// evictLocked drops expired entries, then -- only if the shard is still full --
// arbitrary ones. Map iteration order is randomized, so "arbitrary" really is,
// which is the property that matters: no user can arrange to be the one who is
// never evicted.
func (s *principalShard) evictLocked(now time.Time) {
	for key, entry := range s.m {
		if now.After(entry.expires) {
			delete(s.m, key)
		}
	}

	for key := range s.m {
		if len(s.m) < principalCacheEntriesPerShard {
			return
		}

		delete(s.m, key)
	}
}

// len reports how many entries the whole cache holds. Test-only today; it is the
// assertion that the epoch key really does strand old entries rather than
// updating them in place.
func (c *principalCache) len() int {
	total := 0

	for i := range c.shards {
		c.shards[i].mu.Lock()
		total += len(c.shards[i].m)
		c.shards[i].mu.Unlock()
	}

	return total
}
