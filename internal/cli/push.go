package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jsmith212/bakery/internal/config"
)

// The write path -- our invention. BitBake has no push; `bakery sstate push` walks a
// local SSTATE_DIR, HEADs each object, and PUTs only the misses, so a warm cache is a
// cheap no-op and a cold one uploads exactly once. `downloads push` is the same engine
// over a flat DL_DIR. Neither talks to hashserv: they walk the on-disk cache only.

// mirrorKind is the /cache backend segment: "sstate" or "downloads".
type mirrorKind string

const (
	kindSstate    mirrorKind = "sstate"
	kindDownloads mirrorKind = "downloads"
)

// entry is one local file to consider: its cache key (the relative path for sstate, the
// basename for downloads) and where to read its bytes.
type entry struct {
	key  string
	path string
	size int64
}

// fatal errors abort the whole push: every subsequent request would fail identically,
// so continuing is noise. They are distinguished from a per-object transient failure,
// which is recorded and the push continues.
var (
	errPushUnauthorized = errors.New("unauthorized")
	errPushForbidden    = errors.New("forbidden")
	errPushNoBackend    = errors.New("no backend")
)

// pushSummary is the machine-readable result. --json emits it verbatim; the human form
// is one terse line.
type pushSummary struct {
	Scanned        int           `json:"scanned"`
	Uploaded       int           `json:"uploaded"`
	UploadedBytes  int64         `json:"uploaded_bytes"`
	AlreadyPresent int           `json:"already_present"`
	Failed         int           `json:"failed"`
	Failures       []pushFailure `json:"failures,omitempty"`
	DurationMS     int64         `json:"duration_ms"`
}

type pushFailure struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

func sstatePush(ctx context.Context, c *Client, r renderer, cmd config.SstatePushCmd) error {
	entries, err := walkSstate(cmd.Dir)
	if err != nil {
		return err
	}

	return runPush(ctx, c, r, kindSstate, cmd.Org, cmd.Project, entries, pushOpts{
		concurrency: cmd.Concurrency,
		dryRun:      cmd.DryRun,
		cred:        cacheCredential{Key: cmd.Key},
	})
}

func downloadsPush(ctx context.Context, c *Client, r renderer, cmd config.DownloadsPushCmd) error {
	entries, err := walkDownloads(cmd.Dir)
	if err != nil {
		return err
	}

	return runPush(ctx, c, r, kindDownloads, cmd.Org, cmd.Project, entries, pushOpts{
		concurrency: cmd.Concurrency,
		dryRun:      cmd.DryRun,
		cred:        cacheCredential{Key: cmd.Key},
	})
}

// runPush drives the engine and renders the summary, translating a fatal abort into the
// one actionable sentence the user can act on.
func runPush(
	ctx context.Context, c *Client, r renderer,
	kind mirrorKind, org, project string, entries []entry, opts pushOpts,
) error {
	summary, fatal := pushMirror(ctx, c, kind, org, project, entries, opts)

	// The summary is worth printing even on a fatal abort: it says how far the push got
	// before the wall it hit.
	if rerr := renderPushSummary(r, kind, org, project, summary, opts.dryRun); rerr != nil {
		return rerr
	}

	if fatal != nil {
		return fatalPushError(fatal, kind, org, project, opts.cred)
	}

	if summary.Failed > 0 {
		// A per-object failure is not fatal, but the push did not fully succeed, so the
		// process must exit non-zero.
		return fmt.Errorf("%d object(s) failed to upload", summary.Failed)
	}

	return nil
}

// fatalPushError turns a fatal sentinel into an actionable message. The credential
// decides the 401 remedy: a bad --key is not fixed by `bakery login`.
func fatalPushError(fatal error, kind mirrorKind, org, project string, cred cacheCredential) error {
	switch {
	case errors.Is(fatal, errPushUnauthorized):
		if cred.Key != "" {
			return errors.New("the server rejected the credential: check --key or BAKERY_API_KEY")
		}

		return ErrNeedsLogin
	case errors.Is(fatal, errPushForbidden):
		return fmt.Errorf("your key or role lacks write access to %s/%s", org, project)
	case errors.Is(fatal, errPushNoBackend):
		return fmt.Errorf("no %s backend at %s/%s", kind, org, project)
	default:
		return fatal
	}
}

// ---------------------------------------------------------------------------
// the engine
// ---------------------------------------------------------------------------

type pushOpts struct {
	concurrency int
	dryRun      bool
	cred        cacheCredential
}

// defaultConcurrency is the semaphore width when the flag is unset or non-positive.
const defaultConcurrency = 8

// pushMirror HEADs then PUTs every entry under one semaphore. It returns the summary
// and, if the push hit a wall that dooms every remaining request, a fatal error. A
// fatal cancels the in-flight work; a per-object transient is recorded and the rest
// proceed.
func pushMirror(
	ctx context.Context, c *Client, kind mirrorKind, org, project string, entries []entry, opts pushOpts,
) (pushSummary, error) {
	start := time.Now()

	concurrency := opts.concurrency
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		summary  = pushSummary{Scanned: len(entries)}
		fatalErr error
		fatal    sync.Once
	)

	setFatal := func(err error) {
		fatal.Do(func() {
			fatalErr = err
			cancel() // stop the remaining probes/uploads: they would fail identically
		})
	}

	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup

	for _, e := range entries {
		if ctx.Err() != nil {
			break // a fatal already fired; do not launch more work
		}

		wg.Add(1)

		sem <- struct{}{}

		go func(e entry) {
			defer wg.Done()
			defer func() { <-sem }()

			res := pushOne(ctx, c, kind, org, project, e, opts)

			if res.fatal != nil {
				setFatal(res.fatal)

				return
			}

			mu.Lock()
			defer mu.Unlock()

			switch res.kind {
			case resultUploaded:
				summary.Uploaded++
				summary.UploadedBytes += e.size
			case resultPresent:
				summary.AlreadyPresent++
			case resultFailed:
				summary.Failed++
				summary.Failures = append(summary.Failures, pushFailure{Key: e.key, Error: res.err.Error()})
			}
		}(e)
	}

	wg.Wait()

	// Stable order so --json and the failure list are deterministic across runs.
	sort.Slice(summary.Failures, func(i, j int) bool {
		return summary.Failures[i].Key < summary.Failures[j].Key
	})

	summary.DurationMS = time.Since(start).Milliseconds()

	return summary, fatalErr
}

type resultKind int

const (
	resultPresent resultKind = iota
	resultUploaded
	resultFailed
)

type oneResult struct {
	kind  resultKind
	err   error // set for resultFailed
	fatal error // set to abort the whole push
}

// pushOne does the HEAD-then-PUT for a single object.
//
// A route that is absent (org/project/backend) and an object that is merely missing
// both answer HEAD with 404. They are told apart by the PUT: a real miss PUTs to 201,
// while an absent route answers the PUT itself with 404 -- which the server decides
// before it reads the body, so the doomed upload never streams.
func pushOne(ctx context.Context, c *Client, kind mirrorKind, org, project string, e entry, opts pushOpts) oneResult {
	status, err := c.head(ctx, string(kind), org, project, e.key, opts.cred)
	if err != nil {
		if ctx.Err() != nil {
			return oneResult{kind: resultFailed, err: ctx.Err()}
		}

		return oneResult{kind: resultFailed, err: err}
	}

	switch status {
	case http.StatusOK:
		return oneResult{kind: resultPresent}
	case http.StatusNotFound:
		// miss -> upload it (below)
	case http.StatusUnauthorized:
		return oneResult{fatal: errPushUnauthorized}
	case http.StatusForbidden:
		return oneResult{fatal: errPushForbidden}
	default:
		return oneResult{kind: resultFailed, err: fmt.Errorf("HEAD returned %d", status)}
	}

	if opts.dryRun {
		// Report what WOULD upload; PUT nothing. (A dry run cannot tell a real miss from
		// an absent route, but it never mutates, so the ambiguity is harmless.)
		return oneResult{kind: resultUploaded}
	}

	return putOne(ctx, c, kind, org, project, e, opts)
}

// putOne opens the file and streams it. The status decides the outcome; a 404 here is
// the route-absent signal HEAD could not distinguish.
func putOne(ctx context.Context, c *Client, kind mirrorKind, org, project string, e entry, opts pushOpts) oneResult {
	f, err := os.Open(e.path)
	if err != nil {
		return oneResult{kind: resultFailed, err: fmt.Errorf("open %s: %w", e.path, err)}
	}

	defer func() { _ = f.Close() }()

	status, err := c.put(ctx, string(kind), org, project, e.key, f, e.size, opts.cred)
	if err != nil {
		if ctx.Err() != nil {
			return oneResult{kind: resultFailed, err: ctx.Err()}
		}

		return oneResult{kind: resultFailed, err: err}
	}

	switch status {
	case http.StatusCreated:
		return oneResult{kind: resultUploaded}
	case http.StatusOK:
		// Another pusher won the HEAD/PUT race and the immutable row stands: an
		// idempotent no-op, counted as already-present rather than uploaded.
		return oneResult{kind: resultPresent}
	case http.StatusUnauthorized:
		return oneResult{fatal: errPushUnauthorized}
	case http.StatusForbidden:
		return oneResult{fatal: errPushForbidden}
	case http.StatusNotFound:
		return oneResult{fatal: errPushNoBackend}
	default:
		return oneResult{kind: resultFailed, err: fmt.Errorf("PUT returned %d", status)}
	}
}

// ---------------------------------------------------------------------------
// walks
// ---------------------------------------------------------------------------

// walkSstate recursively walks an SSTATE_DIR. The cache key is the path relative to
// dir, preserving the [universal/]<hh>/<hh>/ layout and the sstate: colons. It uploads
// the served objects -- *.tar.zst and their always-probed *.tar.zst.siginfo sidecars,
// plus *.tar.zst.sig when signing is on -- and skips *.done, the client-side donestamp
// that is never served from a mirror.
func walkSstate(dir string) ([]entry, error) {
	var entries []entry

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		name := d.Name()
		if !servedSstateName(name) {
			return nil
		}

		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return fmt.Errorf("relativize %s: %w", path, rerr)
		}

		// Cache keys are '/'-separated on the wire regardless of the local OS separator.
		rel = filepath.ToSlash(rel)

		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("stat %s: %w", path, ierr)
		}

		entries = append(entries, entry{key: rel, path: path, size: info.Size()})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}

	return entries, nil
}

// servedSstateName reports whether a filename is a served sstate object. A dotfile,
// lock or temp file is skipped; a .done donestamp is skipped (client-only, never
// mirrored).
func servedSstateName(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".done") {
		return false
	}

	return strings.HasSuffix(name, ".tar.zst") ||
		strings.HasSuffix(name, ".tar.zst.siginfo") ||
		strings.HasSuffix(name, ".tar.zst.sig")
}

// walkDownloads walks the TOP LEVEL of a DL_DIR only. The cache key is the flat
// basename. It skips subdirectories (VCS mirror trees like git2/ are not premirror
// tarballs), and *.done / *.lock / *.tmp control files.
func walkDownloads(dir string) ([]entry, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var entries []entry

	for _, d := range dirEntries {
		if d.IsDir() {
			continue // git2/, svn mirrors, etc: not flat premirror tarballs
		}

		name := d.Name()
		if !servedDownloadName(name) {
			continue
		}

		info, ierr := d.Info()
		if ierr != nil {
			return nil, fmt.Errorf("stat %s: %w", name, ierr)
		}

		entries = append(entries, entry{
			key:  name,
			path: filepath.Join(dir, name),
			size: info.Size(),
		})
	}

	return entries, nil
}

// servedDownloadName reports whether a top-level DL_DIR file is a served premirror
// artifact. Dotfiles and the *.done / *.lock / *.tmp control files are skipped.
func servedDownloadName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}

	for _, suffix := range []string{".done", ".lock", ".tmp"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// summary rendering
// ---------------------------------------------------------------------------

func renderPushSummary(r renderer, kind mirrorKind, org, project string, s pushSummary, dryRun bool) error {
	return r.value(s, func(out io.Writer) {
		verb := "uploaded"
		if dryRun {
			verb = "would upload"
		}

		fmt.Fprintf(out, "%s push to %s/%s: scanned %d; %s %d (%s); already present %d; failed %d in %s\n",
			kind, org, project,
			s.Scanned, verb, s.Uploaded, humanBytes(s.UploadedBytes), s.AlreadyPresent, s.Failed,
			(time.Duration(s.DurationMS) * time.Millisecond),
		)

		for _, f := range s.Failures {
			fmt.Fprintf(out, "  failed %s: %s\n", f.Key, f.Error)
		}
	})
}

// humanBytes renders a byte count in binary units with one decimal, in the tabular,
// terse voice the design fixes (GiB, not GB).
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
