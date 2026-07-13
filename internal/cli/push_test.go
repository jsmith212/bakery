package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jsmith212/bakery/internal/config"
)

// fakeCache is an httptest handler that stands in for the sstate/downloads backend at
// the wire level a push cares about: HEAD is a hit (200) for a key it holds and a miss
// (404) otherwise; PUT records the body and is 201. It counts PUTs per key so a test can
// assert an already-present object is never re-uploaded.
type fakeCache struct {
	mu       sync.Mutex
	present  map[string]bool   // key -> already there (HEAD 200, PUT would be a no-op)
	stored   map[string][]byte // key -> bytes received on a PUT
	putCount map[string]int    // key -> number of PUTs seen

	// status overrides for the auth/abort tests: if authFail is set, every request
	// answers with it before touching the store.
	authFail int
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		present:  map[string]bool{},
		stored:   map[string][]byte{},
		putCount: map[string]int{},
	}
}

// key extracts the decoded cache key from /cache/{org}/{project}/{kind}/{key...}.
func cacheKeyFromPath(p string) string {
	// p is like /cache/acme/widget/sstate/universal/aa/bb/sstate:zlib.tar.zst
	const prefix = "/cache/"
	rest := p[len(prefix):]

	// drop org/project/kind (three segments)
	for range 3 {
		i := indexByte(rest, '/')
		if i < 0 {
			return ""
		}

		rest = rest[i+1:]
	}

	return rest
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}

	return -1
}

func (fc *fakeCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if fc.authFail != 0 {
		w.WriteHeader(fc.authFail)

		return
	}

	key := cacheKeyFromPath(r.URL.Path)

	fc.mu.Lock()
	defer fc.mu.Unlock()

	switch r.Method {
	case http.MethodHead:
		if fc.present[key] || fc.stored[key] != nil {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusNotFound)

	case http.MethodPut:
		fc.putCount[key]++

		// An already-present immutable key is an idempotent no-op: 200, no swap.
		if fc.present[key] || fc.stored[key] != nil {
			w.WriteHeader(http.StatusOK)

			return
		}

		body, _ := io.ReadAll(r.Body)
		fc.stored[key] = body
		w.WriteHeader(http.StatusCreated)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// testClient builds a real Client pointed at srv. The cache path uses --key (Basic), so
// no token store or OIDC is needed.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()

	c, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return c
}

// writeFile writes body to dir/rel, creating parents.
func writeFile(t *testing.T, dir, rel string, body []byte) {
	t.Helper()

	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestSstatePushUploadsMissesAndSkipsPresent is the headline CLI contract: walk a temp
// SSTATE_DIR, HEAD every object, PUT only the misses, skip what is already there, and
// never re-PUT a present object. A .done donestamp must never be uploaded.
func TestSstatePushUploadsMissesAndSkipsPresent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	miss := "universal/aa/bb/sstate:zlib:x86_64.tar.zst"
	missSig := "universal/aa/bb/sstate:zlib:x86_64.tar.zst.siginfo"
	present := "universal/cc/dd/sstate:busybox:aarch64.tar.zst"
	done := "universal/aa/bb/sstate:zlib:x86_64.tar.zst.done" // must be skipped

	writeFile(t, dir, miss, []byte("zlib object bytes"))
	writeFile(t, dir, missSig, []byte("zlib siginfo"))
	writeFile(t, dir, present, []byte("busybox object bytes"))
	writeFile(t, dir, done, []byte("donestamp -- client only"))

	fc := newFakeCache()
	fc.present[present] = true // server already has this one

	srv := httptest.NewServer(fc)
	defer srv.Close()

	c := testClient(t, srv)

	summary, fatal := pushMirror(context.Background(), c, kindSstate, "acme", "widget",
		mustWalkSstate(t, dir), pushOpts{concurrency: 4, cred: cacheCredential{Key: "bkry_testkey"}})

	if fatal != nil {
		t.Fatalf("unexpected fatal: %v", fatal)
	}

	// Three served objects (the .done is excluded by the walk).
	if summary.Scanned != 3 {
		t.Errorf("scanned = %d, want 3 (the .done must be excluded)", summary.Scanned)
	}

	if summary.Uploaded != 2 {
		t.Errorf("uploaded = %d, want 2 (the two misses)", summary.Uploaded)
	}

	if summary.AlreadyPresent != 1 {
		t.Errorf("already present = %d, want 1", summary.AlreadyPresent)
	}

	if summary.Failed != 0 {
		t.Errorf("failed = %d, want 0 (%v)", summary.Failed, summary.Failures)
	}

	// The present object was HEADed and must NOT have been PUT.
	if n := fc.putCount[present]; n != 0 {
		t.Errorf("present object was PUT %d time(s); it must be skipped", n)
	}

	// The .done was never a candidate, so it was neither HEADed nor stored.
	if _, ok := fc.stored[done]; ok {
		t.Error("the .done donestamp was uploaded; it must never be")
	}

	// The two misses landed with their exact bytes.
	if got := string(fc.stored[miss]); got != "zlib object bytes" {
		t.Errorf("stored %q = %q", miss, got)
	}

	if got := string(fc.stored[missSig]); got != "zlib siginfo" {
		t.Errorf("stored %q = %q", missSig, got)
	}
}

// TestSstatePushDryRun reports the misses but PUTs nothing.
func TestSstatePushDryRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "aa/bb/sstate:one.tar.zst", []byte("one"))
	writeFile(t, dir, "aa/bb/sstate:two.tar.zst", []byte("two"))

	fc := newFakeCache()
	srv := httptest.NewServer(fc)
	defer srv.Close()

	c := testClient(t, srv)

	summary, fatal := pushMirror(context.Background(), c, kindSstate, "acme", "widget",
		mustWalkSstate(t, dir), pushOpts{concurrency: 2, dryRun: true, cred: cacheCredential{Key: "bkry_k"}})

	if fatal != nil {
		t.Fatalf("unexpected fatal: %v", fatal)
	}

	if summary.Uploaded != 2 {
		t.Errorf("would-upload = %d, want 2", summary.Uploaded)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if len(fc.putCount) != 0 {
		t.Errorf("dry run issued %d PUT(s); it must issue none", len(fc.putCount))
	}
}

// TestSstatePushFatalAbort: a 403 on the first request aborts the whole push -- every
// subsequent object would fail identically, so continuing is noise.
func TestSstatePushFatalAbort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 5 {
		writeFile(t, dir, filepath.Join("aa", "bb", "sstate:obj"+string(rune('0'+i))+".tar.zst"), []byte("x"))
	}

	fc := newFakeCache()
	fc.authFail = http.StatusForbidden

	srv := httptest.NewServer(fc)
	defer srv.Close()

	c := testClient(t, srv)

	_, fatal := pushMirror(context.Background(), c, kindSstate, "acme", "widget",
		mustWalkSstate(t, dir), pushOpts{concurrency: 1, cred: cacheCredential{Key: "bkry_readonly"}})

	if fatal == nil {
		t.Fatal("expected a fatal abort on 403, got nil")
	}

	// runPush maps this to the actionable sentence.
	err := fatalPushError(fatal, kindSstate, "acme", "widget", cacheCredential{Key: "bkry_readonly"})
	if err == nil || !contains(err.Error(), "write access") {
		t.Errorf("fatal message = %v, want one about write access", err)
	}
}

// TestDownloadsWalkSkipsDirsAndControlFiles: the flat downloads walk uploads top-level
// files and skips subdirectories (VCS mirror trees) and .done/.lock control files.
func TestDownloadsWalkSkipsDirsAndControlFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "zlib-1.3.tar.gz", []byte("tarball"))
	writeFile(t, dir, "zlib-1.3.tar.gz.done", []byte("donestamp"))  // skip
	writeFile(t, dir, "busybox.tar.bz2.lock", []byte("lock"))       // skip
	writeFile(t, dir, "git2/github.com.repo", []byte("vcs mirror")) // subdir -> skip

	entries, err := walkDownloads(dir)
	if err != nil {
		t.Fatalf("walkDownloads: %v", err)
	}

	if len(entries) != 1 || entries[0].key != "zlib-1.3.tar.gz" {
		t.Fatalf("entries = %+v, want exactly [zlib-1.3.tar.gz]", entries)
	}
}

func mustWalkSstate(t *testing.T, dir string) []entry {
	t.Helper()

	entries, err := walkSstate(dir)
	if err != nil {
		t.Fatalf("walkSstate: %v", err)
	}

	return entries
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}

// ensure the command handlers compile against the config types (they are the real
// dispatch targets).
var (
	_ = func(ctx context.Context, c *Client, r renderer, cmd config.SstatePushCmd) error {
		return sstatePush(ctx, c, r, cmd)
	}
	_ = func(ctx context.Context, c *Client, r renderer, cmd config.DownloadsPushCmd) error {
		return downloadsPush(ctx, c, r, cmd)
	}
)
