package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Directory layout under the root:
//
//	objects/aa/bb/aabbcc...ff   the object, named by its full hex digest
//	staging/                    partial writes, never visible under objects/
//
// TWO LEVELS of fan-out, 256 wide each: 65,536 leaf directories, so a million
// objects averages ~15 files per directory. One level (256 dirs) puts thousands of
// entries in a directory and turns readdir into the GC's bottleneck; three levels
// costs an extra inode lookup per open for headroom nothing needs.
//
// The full digest is repeated in the filename rather than truncated to the
// remaining 60 characters. It costs 4 bytes per name and it means an object found
// by `find` names itself -- which is exactly the position a human is in when they
// are reconciling orphaned bytes against Postgres after a crash.
const (
	objectsDir = "objects"
	stagingDir = "staging"

	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o640
)

// Local is a Store over a local directory tree.
//
// Writes are atomic: staged in staging/, fsynced, then renamed into place.
// rename(2) within a filesystem is atomic, so a reader either sees no object or
// sees the whole object. A torn file is not observable -- not after a crash, not
// under a concurrent read, not ever.
type Local struct {
	root string
}

// Compile-time proof of the contract.
var _ Store = (*Local)(nil)

// NewLocal prepares root and returns a Store over it. The directories are created
// if absent.
func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, errors.New("storage: local root is empty")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root %q: %w", root, err)
	}

	for _, dir := range []string{filepath.Join(abs, objectsDir), filepath.Join(abs, stagingDir)} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, fmt.Errorf("create storage directory %q: %w", dir, err)
		}
	}

	return &Local{root: abs}, nil
}

// Root is the absolute path of the tree. For diagnostics and tests.
func (l *Local) Root() string { return l.root }

// path is the object's location. It is a pure FUNCTION of the digest, which is why
// no storage_path column exists in the schema: a stored path is a second copy of a
// fact that can then disagree with the first.
func (l *Local) path(k Key) string {
	h := k.String()

	return filepath.Join(l.root, objectsDir, h[0:2], h[2:4], h)
}

// Create stages an object in staging/. Nothing under objects/ changes until Commit.
func (l *Local) Create(_ context.Context) (Writer, error) {
	f, err := os.CreateTemp(filepath.Join(l.root, stagingDir), "put-*")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}

	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())

		return nil, fmt.Errorf("chmod staging file: %w", err)
	}

	return &localWriter{store: l, f: f, h: sha256.New(), n: 0, done: false}, nil
}

func (l *Local) Get(_ context.Context, k Key) (io.ReadCloser, error) {
	f, err := os.Open(l.path(k))
	if err != nil {
		return nil, l.mapErr("open", k, err)
	}

	return f, nil
}

func (l *Local) Stat(_ context.Context, k Key) (Info, error) {
	fi, err := os.Stat(l.path(k))
	if err != nil {
		return Info{}, l.mapErr("stat", k, err)
	}

	return Info{Key: k, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (l *Local) Exists(_ context.Context, k Key) (bool, error) {
	_, err := os.Stat(l.path(k))
	if err == nil {
		return true, nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return false, l.mapErr("stat", k, err)
}

// Delete unlinks the bytes. Absent is success: the GC re-drives its queue after a
// crash and the second attempt must not be an error.
//
// The now-empty fan-out directories are deliberately NOT removed. Two GCs racing
// an rmdir against a concurrent write's MkdirAll is a lost object for a few
// hundred bytes of inode.
func (l *Local) Delete(_ context.Context, k Key) error {
	if err := os.Remove(l.path(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return l.mapErr("remove", k, err)
	}

	return nil
}

// mapErr turns a syscall error into a sentinel. Every path error carries the key,
// never the raw path -- the path is derivable and the key is what Postgres knows.
func (l *Local) mapErr(op string, k Key, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s %s: %w", op, k, ErrNotFound)
	}

	return fmt.Errorf("%s %s: %w", op, k, err)
}

// localWriter stages bytes and hashes them in one pass. It never buffers the
// object: an sstate tarball is multi-GB.
type localWriter struct {
	store *Local
	f     *os.File
	h     hash.Hash
	n     int64
	done  bool
}

func (w *localWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrCommitted
	}

	n, err := w.f.Write(p)
	w.n += int64(n)

	// Hash exactly what reached the file, so a short write cannot produce a digest
	// that disagrees with the bytes.
	_, _ = w.h.Write(p[:n])

	if err != nil {
		return n, fmt.Errorf("write staging file: %w", err)
	}

	return n, nil
}

func (w *localWriter) Digest() (Key, int64) {
	var k Key

	w.h.Sum(k[:0])

	return k, w.n
}

// Commit fsyncs the data, renames it into place, and fsyncs the containing
// directory. All three are required:
//
//   - without the file fsync, a crash can leave the renamed name pointing at a
//     file whose data blocks were never written -- a torn object with a valid name;
//   - without the rename, a reader can see a partial file;
//   - without the DIRECTORY fsync, the rename itself is not durable, and a crash
//     leaves us having told Postgres the bytes are live when the name is gone.
//     That is dangling metadata, i.e. a permanent 500.
func (w *localWriter) Commit(_ context.Context) (Info, error) {
	if w.done {
		return Info{}, ErrCommitted
	}

	k, n := w.Digest()

	// From here on the staging file is either renamed away or removed by Abort.
	if err := w.f.Sync(); err != nil {
		return Info{}, fmt.Errorf("fsync staging file: %w", err)
	}

	if err := w.f.Close(); err != nil {
		return Info{}, fmt.Errorf("close staging file: %w", err)
	}

	dst := w.store.path(k)

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return Info{}, fmt.Errorf("create fan-out directory: %w", err)
	}

	if err := os.Rename(w.f.Name(), dst); err != nil {
		return Info{}, fmt.Errorf("rename staged object into place: %w", err)
	}

	w.done = true

	if err := fsyncDir(dir); err != nil {
		return Info{}, err
	}

	fi, err := os.Stat(dst)
	if err != nil {
		return Info{}, fmt.Errorf("stat committed object %s: %w", k, err)
	}

	return Info{Key: k, Size: n, ModTime: fi.ModTime()}, nil
}

// Abort discards the staged bytes. Safe to call after Commit (no-op) and safe to
// call twice, so `defer w.Abort()` is unconditionally correct.
func (w *localWriter) Abort() error {
	if w.done {
		return nil
	}

	w.done = true

	_ = w.f.Close()

	if err := os.Remove(w.f.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove staging file: %w", err)
	}

	return nil
}

// fsyncDir makes a rename durable. Opening a directory read-only and fsyncing it is
// the portable POSIX way; on Linux it is exactly what the kernel wants.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for fsync: %w", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync directory %q: %w", dir, err)
	}

	return nil
}
