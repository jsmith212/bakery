package bazelconf

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestCcache drives the REAL ccache binary against Bakery's /ac mount over @layout=bazel.
//
// It proves TWO things the docs assert but only a real client can settle:
//
//  1. A cold miss becomes a warm HIT on rebuild -- the cache actually round-trips through
//     Bakery's /ac store over HTTP.
//  2. ccache in bazel layout touches /ac AND ONLY /ac. It never issues a /cas request.
//     The recorder proves that against the CLIENT: if ccache ever hit /cas, or if the /ac
//     mount rejected a write, this fails. `@layout=bazel` writes to /ac/<hash>; without it
//     ccache uses `subdirs` layout and every GET 404s and latches the backend off.
//
// CCACHE_REMOTE_ONLY takes local storage out of the picture entirely, so the only place a
// warm hit can come from is Bakery -- a local hit could never masquerade as a remote one.
func TestCcache(t *testing.T) {
	ccache := requireBinary(t, "ccache")
	cc := requireCompiler(t)

	e := newEnv(t)

	ccacheDir := t.TempDir()
	srcDir := t.TempDir()

	src := filepath.Join(srcDir, "hello.c")
	if err := os.WriteFile(src, []byte("int bakery_ccache_conformance(void) { return 42; }\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// The bazel-layout remote store with the write-scoped token. The token is ONE opaque
	// bkry_ value carried in the `bearer-token` attribute -- there is no id:secret pair.
	// `layout=bazel` is what makes ccache write to /ac/<hash> instead of the subdirs layout
	// this mount does not route.
	remote := "http://" + hostOf(t, e.httpBase) + "/cache/" + envOrg + "/" + envProject +
		"|layout=bazel|bearer-token=" + e.writeKey

	ccacheEnv := func() []string {
		return append(os.Environ(),
			"CCACHE_DIR="+ccacheDir,
			"CCACHE_REMOTE_STORAGE="+remote,
			"CCACHE_REMOTE_ONLY=true",
		)
	}

	compile := func(t *testing.T, tag string) {
		t.Helper()

		out := filepath.Join(t.TempDir(), "hello.o")

		cmd := exec.CommandContext(t.Context(), ccache, cc, "-c", src, "-o", out)
		cmd.Env = ccacheEnv()

		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ccache compile (%s): %v\n%s", tag, err, combined)
		}
	}

	// Cold compile: the remote is empty, so this misses and WRITES the result to /ac.
	compile(t, "cold")

	// Zero the counters and the recorder, so the warm compile's stats and traffic stand
	// alone: a hit here can only be the object the cold compile just wrote to Bakery.
	if out, err := exec.CommandContext(t.Context(), ccache, "-z").CombinedOutput(); err != nil {
		t.Fatalf("ccache -z: %v\n%s", err, out)
	}

	// Do NOT reset the recorder yet: the /cas assertion below must cover the WHOLE run,
	// cold write included -- a stray /cas request on the cold path would be the bug.
	compile(t, "warm")

	// The warm compile must be a remote hit. --print-stats is the stable machine-readable
	// form (tab-separated key<TAB>value); remote_storage_hit is the counter that proves the
	// object came back from Bakery specifically, not from any local fallback.
	stats := ccachePrintStats(t, ccache, ccacheDir)
	if stats["remote_storage_hit"] < 1 {
		t.Errorf("warm ccache compile was not a remote hit: remote_storage_hit=%d, want >=1\nstats=%v",
			stats["remote_storage_hit"], stats)
	}

	// The network truth: ccache hit /ac/<64hex> and NEVER /cas. This is the opacity claim,
	// pinned to the real client rather than a doc.
	sawACWrite, sawACRead, sawCAS := false, false, false

	acPath := regexp.MustCompile(`^/cache/` + regexp.QuoteMeta(envOrg+"/"+envProject) + `/ac/[0-9a-f]{64}$`)

	for _, req := range e.rec.snapshot() {
		switch {
		case strings.Contains(req.path, "/cache/"+envOrg+"/"+envProject+"/cas/"):
			sawCAS = true
		case req.method == "PUT" && acPath.MatchString(req.path):
			sawACWrite = true
		case (req.method == "GET" || req.method == "HEAD") && acPath.MatchString(req.path):
			sawACRead = true
		}
	}

	if sawCAS {
		t.Errorf("ccache in bazel layout hit /cas -- it must touch /ac ONLY:\n%v", e.rec.snapshot())
	}

	if !sawACWrite {
		t.Errorf("ccache never PUT to /ac/<64hex> -- the cold write did not reach Bakery:\n%v", e.rec.snapshot())
	}

	if !sawACRead {
		t.Errorf("ccache never GET/HEAD /ac/<64hex> -- the warm read did not reach Bakery:\n%v", e.rec.snapshot())
	}

	t.Logf("PROVEN: real ccache cold-miss->warm-hit over /ac (remote_storage_hit=%d), and it made "+
		"ZERO /cas requests", stats["remote_storage_hit"])
}

// ccachePrintStats runs `ccache --print-stats` and returns the counters as a map. The
// format is stable and machine-readable: one `key<TAB>value` per line.
func ccachePrintStats(t *testing.T, ccache, dir string) map[string]int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), ccache, "--print-stats")
	cmd.Env = append(os.Environ(), "CCACHE_DIR="+dir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ccache --print-stats: %v\n%s", err, out)
	}

	stats := make(map[string]int)

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, val, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}

		if n, cerr := strconv.Atoi(strings.TrimSpace(val)); cerr == nil {
			stats[strings.TrimSpace(key)] = n
		}
	}

	return stats
}

// ---------------------------------------------------------------------------
// Shared helpers for the binary-driven halves (ccache + sccache).
// ---------------------------------------------------------------------------

// requireCompiler resolves a C compiler for the ccache/sccache wrappers to invoke,
// skipping cheaply -- before dbtest.New -- when none is on PATH. It prefers `cc`, the
// POSIX name every runner has, then gcc/clang.
func requireCompiler(t *testing.T) string {
	t.Helper()

	for _, name := range []string{"cc", "gcc", "clang"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}

	t.Skip(binSkipMsg("cc", errNoCompiler))

	return ""
}

// errNoCompiler is the sentinel requireCompiler hands binSkipMsg when no C compiler is
// installed -- the ccache/sccache halves cannot run a real compile without one.
var errNoCompiler = compilerAbsent{}

type compilerAbsent struct{}

func (compilerAbsent) Error() string { return "no C compiler (cc/gcc/clang) on PATH" }

// hostOf extracts host:port from an http://host:port base URL, for building the ccache
// and sccache endpoint strings.
func hostOf(t *testing.T, base string) string {
	t.Helper()

	h := strings.TrimPrefix(base, "http://")
	h = strings.TrimPrefix(h, "https://")

	return strings.TrimSuffix(h, "/")
}
