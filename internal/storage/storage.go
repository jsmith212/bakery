// Package storage owns the BYTES, and nothing else.
//
// A Store is a content-addressed byte store: the key of an object is the sha256 of
// its content, and there is no other kind of key. It holds no metadata, no
// refcounts, no tenancy and no policy -- those live in blob.Service and in
// Postgres. This split is the whole reason the ordering invariant is expressible:
//
//	on create: bytes first, then metadata
//	on delete: metadata first, then bytes
//
// THE API IS SHAPED TO MAKE THE WRONG ORDER AWKWARD TO WRITE.
//
//   - There is no Put(key, reader). You cannot name an object before you have
//     hashed it, so a write is staged (Create) and then made durable (Commit), and
//     Commit is what yields the Key. Metadata is keyed by digest -- so the only way
//     to obtain the thing you need in order to write metadata is to have already
//     made the bytes durable. "Bytes first" stops being a convention you remember
//     and starts being the only order that compiles.
//
//   - Delete takes a Key and returns nothing useful. It cannot know whether an
//     object still references those bytes, so it must never be called by anything
//     that has not already proved -- in Postgres, under the digest advisory lock,
//     with a refcount = 0 recheck -- that nothing does. blob.Service.ReapDigest is
//     that proof, and it is the only caller.
//
// TWO DRIVERS: Local (local.go) and S3 (s3.go). The interface is deliberately
// free of any filesystem concept (no paths, no modes, no directories), which is
// why S3 landed as a new TYPE and not as a new interface. Everything either
// driver owes a caller is proven once, against both, by the shared suite in
// conformance_test.go -- a driver-specific test file may add its own stronger
// guarantees but may never weaken those.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// Sentinel errors. Callers branch on these; implementations MUST wrap them with %w
// rather than inventing their own.
var (
	// ErrNotFound means the bytes are not in the store. A cache miss renders this
	// as 404 -- NEVER 403, and never a 200 with an empty body: BitBake retries a
	// 403 as a full-body GET.
	ErrNotFound = errors.New("storage: object not found")

	// ErrInvalidKey means the key is not 32 raw bytes / 64 hex characters.
	ErrInvalidKey = errors.New("storage: invalid key")

	// ErrCommitted is returned by a second Commit on the same Writer.
	ErrCommitted = errors.New("storage: writer already committed")
)

// KeySize is the length of a Key in bytes.
const KeySize = sha256.Size

// Key is the content address of an object: the sha256 WE computed over the bytes,
// never anything a client told us.
//
// It is 32 raw bytes rather than 64 hex characters because that is what the blobs
// table stores (bytea, memcmp-comparable, half the index entry) and what Go's
// sha256 hands back.
type Key [KeySize]byte

// KeyOf returns the Key of b. Only useful for small payloads and tests -- the
// streaming write path hashes as it copies.
func KeyOf(b []byte) Key { return sha256.Sum256(b) }

// ParseKey parses a 64-character lowercase hex digest.
func ParseKey(s string) (Key, error) {
	var k Key

	if len(s) != hex.EncodedLen(KeySize) {
		return k, fmt.Errorf("%w: want %d hex chars, got %d", ErrInvalidKey, hex.EncodedLen(KeySize), len(s))
	}

	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return k, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}

	return k, nil
}

// KeyFromBytes adopts 32 raw bytes -- the shape Postgres hands back for a bytea
// digest column.
func KeyFromBytes(b []byte) (Key, error) {
	var k Key

	if len(b) != KeySize {
		return k, fmt.Errorf("%w: want %d bytes, got %d", ErrInvalidKey, KeySize, len(b))
	}

	copy(k[:], b)

	return k, nil
}

// String is the lowercase hex form. This is the ONLY place a Key becomes a string
// for storage purposes; never build a path by hand.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// Bytes is a copy of the raw digest, for binding to a bytea parameter.
func (k Key) Bytes() []byte { return k[:] }

// Info is what a Store knows about an object: nothing but its identity, size and
// when the bytes landed. Deliberately not "metadata" -- metadata is Postgres'.
type Info struct {
	Key     Key
	Size    int64
	ModTime time.Time
}

// Store is the byte store.
//
// Every method streams. sstate tarballs are multi-GB; an implementation that
// buffers an object in memory is a bug, not a tuning choice.
type Store interface {
	// Create stages a new object. Write the bytes to the returned Writer, then
	// Commit (durable) or Abort (discarded). Nothing PARTIAL is ever observable
	// at the object's key -- see Writer for exactly when a driver publishes.
	Create(ctx context.Context) (Writer, error)

	// Get streams an object's bytes. The caller MUST Close the reader.
	// Returns ErrNotFound if the bytes are absent -- which, if Postgres says the
	// object exists, is dangling metadata and a 500.
	Get(ctx context.Context, k Key) (io.ReadCloser, error)

	// Stat reports an object's size without opening it.
	Stat(ctx context.Context, k Key) (Info, error)

	// Exists is Stat without the size. It exists because S3 can answer it with a
	// cheaper request than a full HEAD-equivalent, and because the sstate hot path
	// asks nothing else.
	Exists(ctx context.Context, k Key) (bool, error)

	// Delete removes the bytes. IDEMPOTENT: deleting an absent object is nil, not
	// ErrNotFound, because the GC re-drives its work queue after a crash and the
	// second attempt must not be an error.
	//
	// This is the dangerous end of the ordering invariant. See the package doc:
	// only blob.Service.ReapDigest may call it.
	Delete(ctx context.Context, k Key) error
}

// Writer is a staged object.
//
// The Key is not a parameter, it is a RESULT: you get it from Digest (after
// writing) or from Commit. That is what makes "bytes before metadata" structural
// rather than aspirational.
//
// THE PUBLICATION CONTRACT, and it is DRIVER-AWARE -- the load-bearing property
// is not "Commit publishes", it is:
//
//	Bytes are durable at their content address STRICTLY BEFORE any metadata row
//	names them. Local publishes at Commit; S3 publishes at Sync and RE-ASSERTS at
//	Commit under the caller's digest advisory lock. Neither ever publishes a torn
//	or partial object, and no reader reaches storage except through a live
//	metadata row.
//
// The re-assertion is not ceremony. A driver that publishes before the lock is
// taken can have its object reaped by a concurrent ReapDigest in the window
// between publication and the lock; the PUT would then write a live metadata row
// naming bytes that are gone, which is dangling metadata and a permanent 500 --
// the forbidden side of the ordering invariant. Any such driver MUST re-check
// (and if necessary re-publish) inside Commit, using CONSTANT-TIME calls on the
// common path.
type Writer interface {
	io.Writer

	// Digest returns the sha256 and byte count of everything written so far,
	// WITHOUT making the bytes durable.
	//
	// This is the seam the PUT protocol needs: blob.Service must know the digest to
	// take the digest advisory lock and to ask Postgres whether the bytes are
	// already live -- and only then decide whether to Commit these bytes or Abort
	// them and dedup onto the copy that is already there.
	Digest() (Key, int64)

	// Sync makes the staged bytes DURABLE. Local fsyncs the staged object without
	// renaming it into place, so nothing is observable at the content address yet;
	// S3 finishes the upload and server-side-copies to the content address, so
	// there the object IS observable from here on. Both satisfy the publication
	// contract above.
	//
	// This is the EXPENSIVE step -- an fsync, or a server-side copy, of a possibly
	// multi-GB object -- and it is a separate call so blob.Service can pay it
	// BEFORE it opens the metadata transaction and takes the digest advisory lock,
	// rather than holding a pool connection and the lock across it while every
	// other Postgres user starves. Commit is then constant-time on the common
	// path.
	//
	// Bytes-first is preserved: the data is durable after Sync, before any metadata
	// row. Sync is OPTIONAL -- Commit does the whole job itself if Sync was not
	// called, so a caller that skips it still gets a durable object. Idempotent,
	// and a no-op after Commit.
	Sync() error

	// Commit makes the staged bytes durable at their content address and returns
	// their Info. On return, a reader can never observe a torn or partial object,
	// and a crash cannot resurrect one.
	//
	// IT RUNS INSIDE THE CALLER'S TRANSACTION, HOLDING THE DIGEST ADVISORY LOCK.
	// That is its whole cost budget: on the common path an implementation may make
	// only CONSTANT-TIME calls here (Local: a rename plus a directory fsync; S3: a
	// HeadObject plus a staging DELETE). Size-proportional work belongs in Sync.
	// It is also the only window in which the driver and a concurrent physical
	// delete are mutually excluded, which is why a driver that published in Sync
	// must re-assert presence here -- see the publication contract above.
	//
	// Committing content that is already present is a no-op that succeeds -- the
	// bytes are identical by construction.
	//
	// A second Commit returns ErrCommitted.
	Commit(ctx context.Context) (Info, error)

	// Abort discards the staged bytes -- the STAGED ones only, NEVER the object at
	// the content address, which on the dedup path is live, referenced and
	// somebody else's. Idempotent, and a no-op after a successful Commit, so
	// `defer w.Abort()` is always correct.
	Abort() error
}
