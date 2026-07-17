package bazelconf

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSccache drives the REAL sccache binary against Bakery's sccache WebDAV mount.
//
// The failure mode this guards is SPECIFIC and silent: sccache's opendal WebDAV backend
// PROPFINDs a path's parent before every write, and if that PROPFIND does not deserialize
// into a well-formed multistatus, opendal's check() swallows the error into can_write=false
// and sccache goes read-only for the WHOLE process -- reads keep working, so a naive "does
// it read?" test passes while every write is silently dropped. So the assertion here is
// that a PUT actually round-trips:
//
//   - the recorder saw PROPFIND then PUT under /cache/{org}/{project}/sccache/... (the write
//     path really reached Bakery, in the opendal order), and
//   - a second identical compile HITS, and
//   - sccache --show-stats reports a cache write.
//
// WebDAV is the only backend configured (no SCCACHE_DIR), so a hit can only be the object
// the cold compile wrote to Bakery.
func TestSccache(t *testing.T) {
	sccache := requireBinary(t, "sccache")
	cc := requireCompiler(t)

	e := newEnv(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "hello.c")
	if err := os.WriteFile(src, []byte("int bakery_sccache_conformance(void) { return 7; }\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// The WebDAV endpoint, key prefix and token, exactly as the client-config snippet emits
	// them. KEY_PREFIX=sccache is REQUIRED: sccache shards every key under it
	// ({endpoint}/{prefix}/{a}/{b}/{c}/{64hex}), and without it the keys land where Bakery
	// does not route. The token becomes an "Authorization: Bearer <bkry_...>" header.
	env := append(os.Environ(),
		"SCCACHE_WEBDAV_ENDPOINT="+e.httpBase+"/cache/"+envOrg+"/"+envProject,
		"SCCACHE_WEBDAV_KEY_PREFIX=sccache",
		"SCCACHE_WEBDAV_TOKEN="+e.writeKey,
		// A private server on a free port: sccache's daemon reads the WebDAV env at start,
		// so it must be OUR server, isolated from any the runner already has going.
		"SCCACHE_SERVER_PORT="+strconv.Itoa(freePort(t)),
	)

	run := func(t *testing.T, tag string, args ...string) string {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), sccache, args...)
		cmd.Env = env

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sccache %s (%s): %v\n%s", strings.Join(args, " "), tag, err, out)
		}

		return string(out)
	}

	compile := func(t *testing.T, tag string) {
		t.Helper()

		out := filepath.Join(t.TempDir(), "hello.o")

		cmd := exec.CommandContext(t.Context(), sccache, cc, "-c", src, "-o", out)
		cmd.Env = env

		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sccache compile (%s): %v\n%s", tag, err, combined)
		}
	}

	// Start our own server so the WebDAV env is the env it reads, and stop it at cleanup so
	// the daemon does not outlive the test and leak the port.
	run(t, "start", "--start-server")
	t.Cleanup(func() {
		cmd := exec.CommandContext(context.Background(), sccache, "--stop-server")
		cmd.Env = env
		_ = cmd.Run()
	})

	run(t, "zero", "--zero-stats")

	// Cold compile: WebDAV is empty, so this misses, compiles, and WRITES -- the PROPFIND +
	// PUT the silently-read-only failure mode would swallow.
	compile(t, "cold")

	cold := sccacheStats(t, run(t, "stats-cold", "--show-stats", "--stats-format=json"))
	if cold.writes() < 1 {
		t.Errorf("cold sccache compile reported no cache write (cache_writes=%d): the WebDAV PUT "+
			"was silently dropped -- opendal latched read-only. stats=%+v", cold.writes(), cold)
	}

	// The write really reached Bakery, and in the opendal order: PROPFIND (parent probe)
	// strictly before the first PUT, both under the sccache mount.
	assertPropfindThenPut(t, e)

	run(t, "zero", "--zero-stats")
	e.rec.reset()

	// Warm compile: identical source, so it must be a hit served from Bakery.
	compile(t, "warm")

	warm := sccacheStats(t, run(t, "stats-warm", "--show-stats", "--stats-format=json"))
	if warm.hits() < 1 {
		t.Errorf("warm sccache compile was not a cache hit (cache_hits=%d): the object written to "+
			"Bakery on the cold pass did not come back. stats=%+v", warm.hits(), warm)
	}

	t.Logf("PROVEN: real sccache PROPFIND+PUT round-trip to the WebDAV mount (cache_writes=%d cold), "+
		"and a warm HIT served from Bakery (cache_hits=%d)", cold.writes(), warm.hits())
}

// assertPropfindThenPut proves the sccache write path hit Bakery in opendal's order: a
// PROPFIND on the parent collection strictly BEFORE the first PUT, both under the sccache
// mount. The ordering is the point -- opendal probes before it writes, and a PUT with no
// preceding PROPFIND would mean sccache never ran its collection check (and would have
// gone read-only if the check had failed).
func assertPropfindThenPut(t *testing.T, e *env) {
	t.Helper()

	prefix := "/cache/" + envOrg + "/" + envProject + "/sccache/"

	firstPropfind, firstPut := -1, -1

	for i, req := range e.rec.snapshot() {
		if !strings.HasPrefix(req.path, prefix) {
			continue
		}

		if req.method == methodPropfind && firstPropfind == -1 {
			firstPropfind = i
		}

		if req.method == "PUT" && firstPut == -1 {
			firstPut = i
		}
	}

	switch {
	case firstPropfind == -1:
		t.Errorf("sccache issued no PROPFIND under %s -- opendal's pre-write collection probe "+
			"never reached Bakery:\n%v", prefix, e.rec.snapshot())
	case firstPut == -1:
		t.Errorf("sccache issued no PUT under %s -- the write was silently dropped:\n%v", prefix, e.rec.snapshot())
	case firstPropfind > firstPut:
		t.Errorf("sccache PUT (req %d) preceded its PROPFIND (req %d) under %s -- opendal probes "+
			"the parent collection BEFORE it writes:\n%v", firstPut, firstPropfind, prefix, e.rec.snapshot())
	}
}

// methodPropfind is the WebDAV verb, not in net/http's method set.
const methodPropfind = "PROPFIND"

// sccStats is the slice of sccache's --stats-format=json output the assertions read.
// cache_hits is a per-language count object; cache_writes is a flat counter.
type sccStats struct {
	Stats struct {
		CacheWrites int `json:"cache_writes"`
		CacheHits   struct {
			Counts map[string]int `json:"counts"`
		} `json:"cache_hits"`
	} `json:"stats"`
}

func (s sccStats) writes() int { return s.Stats.CacheWrites }

func (s sccStats) hits() int {
	total := 0
	for _, n := range s.Stats.CacheHits.Counts {
		total += n
	}

	return total
}

func sccacheStats(t *testing.T, out string) sccStats {
	t.Helper()

	var s sccStats
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("decode sccache --show-stats json: %v\n%s", err, out)
	}

	return s
}

// freePort asks the kernel for an unused TCP port, then releases it. A brief race, but the
// window is the test's own and the daemon claims the port immediately.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port

	if cerr := l.Close(); cerr != nil {
		t.Fatalf("release the reserved port: %v", cerr)
	}

	return port
}
