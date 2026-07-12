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
// S3 IS DEFERRED. The interface is deliberately free of any filesystem concept
// (no paths, no modes, no directories) so an S3 implementation is a new type, not a
// new interface.
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
	// Commit (durable) or Abort (discarded). Nothing is observable at the object's
	// key until Commit returns.
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

	// Commit makes the staged bytes durable at their content address and returns
	// their Info. On return the bytes are fsynced and the rename is fsynced: a
	// reader can never observe a torn or partial object, and a crash cannot
	// resurrect one.
	//
	// Committing content that is already present is a no-op that succeeds -- the
	// bytes are identical by construction.
	//
	// A second Commit returns ErrCommitted.
	Commit(ctx context.Context) (Info, error)

	// Abort discards the staged bytes. Idempotent, and a no-op after a successful
	// Commit, so `defer w.Abort()` is always correct.
	Abort() error
}
