// Package hashservconf proves that Bakery serves the REAL bitbake hash-equivalence
// client. It is M3's CI gate, and DESIGN.md calls it non-negotiable.
//
// It has TWO HALVES, because upstream only gives us one:
//
//	Half 1 -- bitbake's OWN hashserv suite, in external mode. `hashserv.tests` has a
//	TestHashEquivalenceExternalServer class that runs every common test against a server
//	named by BB_TEST_HASHSERV instead of one it starts itself. We point it at a booted
//	Bakery and pin the EXACT ran/failed/errored/skipped counts.
//
//	Half 2 -- the real `bitbake-hashclient` binary. Upstream's external suite never runs
//	it: all 27 run_hashclient call sites live in TestHashEquivalenceClient, which spawns
//	its own local unix:// python server, and TestHashEquivalenceExternalServer does not
//	inherit them. So the operator CLI against a multi-tenant WebSocket mount is untested
//	by upstream, and this half is Bakery-authored -- the analogue of the sstate suite's
//	testdata/phase_a.py.
//
// It lives OUTSIDE internal/ on purpose. `just test-db` globs ./internal/... and fails the
// job on any skip; this suite legitimately skips where the bitbake checkout or a pinned
// websockets is absent (a laptop), so it must not be swept into that recipe. `just
// hashserv-conformance` is its home, and there a skip DOES fail the job -- CI provides the
// client, so a skip in CI means the proof did not run.
//
// But `just race` and `just coverage` glob ./..., so this package IS compiled and run in
// the `build` job, where BB_LIB is unset. Every test therefore calls requireBitbake FIRST,
// before dbtest.New and before the server boots: the skip must cost nothing.
package hashservconf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// TestMain gives the suite its own ephemeral Postgres. dbtest.Main is lazy -- the container
// (or the TEST_DB_URL template clone) is created by the first dbtest.New and by nothing
// else -- so a run that skips every test pays nothing for it.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// ---------------------------------------------------------------------------
// HALF 1 -- bitbake's own suite, external mode.
// ---------------------------------------------------------------------------

// externalSuite is the unittest path of upstream's external-server class.
const externalSuite = "hashserv.tests.TestHashEquivalenceExternalServer"

// runTests is EXACTLY what we ask upstream's suite to run. It is an explicit list, not a
// `-k` pattern, for two reasons: unittest's -k has no negation (it is a substring INCLUDE
// filter -- the pytest `not auth` syntax silently matches nothing here), and an explicit
// list cannot quietly widen. TestUpstreamSuiteCoversEveryTest asserts this list plus
// excludedTests accounts for every test method on the class, so a bitbake bump that adds a
// test fails the gate instead of skipping past it.
var runTests = []string{
	// The equivalence engine itself: report, mint, and the "same outhash, different
	// taskhash -> inherit the older unihash" remap that the whole system exists for.
	"test_create_hash",
	"test_create_equivalent",
	"test_duplicate_taskhash",

	// `remove`. Bakery implements it (project-scoped, write-scoped key) precisely because
	// it is how this suite resets between tests -- see setUp/tearDown.
	"test_remove_taskhash",
	"test_remove_unihash",
	"test_remove_outhash",
	"test_remove_method",

	// A 131 072-byte outhash_siginfo in ONE WebSocket frame. WS messages are not chunked
	// (chunking is stream-transport-only), so this is the test that catches a read limit
	// left at coder/websocket's 32 KiB default.
	"test_huge_message",

	// 1000 sequential reports, and then -- on paper -- 100 concurrent clients x 1000
	// lookups.
	//
	// READ THIS BEFORE TRUSTING IT: in bitbake 2.8.0 the concurrent half of this test is
	// DEAD. tests.py:300 calls `Client(self.server_address)`, and tests.py imports only
	// `create_client` and `ClientPool` -- there is no name `Client` in that module. Every
	// one of the 100 threads dies with `NameError: name 'Client' is not defined`, the
	// exception is swallowed by threading, `failures` stays empty, and `assertFalse(failures)`
	// passes. Upstream's own stress test proves nothing, on every transport, in this tag.
	//
	// It is still run, because its 1000 sequential reports are real and because the day
	// upstream fixes the NameError we inherit the concurrency proof for free. But the
	// one-writer-per-connection invariant CANNOT rest on it, so Half 2 carries that proof
	// itself, through bitbake-hashclient's `stress` subcommand -- which builds its clients
	// with hashserv.create_client and therefore actually runs.
	"test_stress",

	// exists-stream (upstream's typo, kept verbatim -- it is the test's real name).
	"test_unihash_exsits",

	// Two clients reporting different unihashes for the same outhash, in both orders. The
	// server must converge on ONE answer and hand it to both.
	"test_diverging_report_race",
	"test_diverging_report_reverse_race",

	// The ClientPool: bitbake's real parallel get path (get_unihash_batch /
	// unihash_exists_batch over many connections at once).
	"test_client_pool_get_unihashes",
	"test_client_pool_unihash_exists",

	// The next three are RUN, not excluded, and they SKIP THEMSELVES: each calls
	// start_server(), which TestHashEquivalenceExternalServer overrides with skipTest
	// ("Cannot start local server when testing external servers"). They are included so the
	// skip is VISIBLE and PINNED -- wantSkipped below names all three. Dropping them from
	// the invocation would make the skip count trivially zero and hide the day one of them
	// stops skipping and starts erroring.
	"test_upstream_server",
	"test_ro_server",
	"test_slow_server_start",
}

// wantSkipped is the exact set of tests that must self-skip, by name. A skip we did not
// predict is a test that did not run, and this gate exists because a suite that skips
// everything and reports a triumphant green is the failure mode we are guarding against:
// get_env() calls skipTest on an EMPTY string too, so a BB_TEST_HASHSERV that came through
// as "" skips all of them.
var wantSkipped = []string{
	"test_ro_server",
	"test_slow_server_start",
	"test_upstream_server",
}

// excludedTests is every test we deliberately do NOT run, and why. A reason is mandatory:
// an exclusion with no recorded reason is how a gate rots into a formality.
//
// Both groups are refusals Bakery makes ON PURPOSE (spec §1), not gaps:
//
//   - The user-admin RPCs (new-user, set-user-perms, become-user, get-user, get-all-users,
//     delete-user, refresh-token) mint and re-scope credentials FROM A CACHE CLIENT.
//     Credentials are minted by the Bakery API, by an authenticated human, and a cache
//     credential can never mint another. hashserv's @user-admin permission is unreachable
//     by construction -- an API-key principal's CanAdminProject is hard-false -- so these
//     RPCs do not exist to be tested.
//
//   - The GC/db-admin RPCs (gc-mark, gc-sweep, gc-status, clean-unused, get-db-usage,
//     get-db-query-columns) are a SECOND, client-reachable collector on the same rows
//     Bakery's in-process M6 GC sweeps under the two-half write barrier. There is no
//     gc_mark column and no config table for them to run against, which is what stops the
//     second collector being reintroduced one column at a time.
//
// Neither group is called by a build: zero call sites in lib/bb for any of them.
var excludedTests = map[string]string{
	"test_auth_read_perms":                      "user-admin: new_user + set_user_perms mint a credential from a cache client",
	"test_auth_report_perms":                    "user-admin: new_user + set_user_perms mint a credential from a cache client",
	"test_auth_no_token_refresh_from_anon_user": "user-admin: refresh-token",
	"test_auth_self_token_refresh":              "user-admin: refresh-token",
	"test_auth_token_refresh":                   "user-admin: refresh-token",
	"test_auth_self_get_user":                   "user-admin: get-user",
	"test_auth_get_user":                        "user-admin: get-user",
	"test_auth_reconnect":                       "user-admin: new_user, then re-auth as that minted user",
	"test_auth_delete_user":                     "user-admin: delete-user",
	"test_auth_set_user_perms":                  "user-admin: set-user-perms",
	"test_auth_get_all_users":                   "user-admin: get-all-users (upstream itself skips this one in external mode)",
	"test_auth_new_user":                        "user-admin: new-user",
	"test_auth_become_user":                     "user-admin: become-user -- privilege assumption from a cache client",
	"test_auth_is_owner":                        "user-admin: new-user + get-user",
	"test_auth_gc":                              "user-admin AND gc: new-user, then gc-mark/gc-sweep",
	"test_gc":                                   "gc: gc-mark/gc-sweep/gc-status -- Bakery GCs in-process (M6), against the DB",
	"test_gc_switch_mark":                       "gc: gc-mark/gc-sweep/gc-status",
	"test_gc_switch_sweep_mark":                 "gc: gc-mark/gc-sweep/gc-status",
	"test_gc_new_hashes":                        "gc: gc-mark/gc-sweep/gc-status",
	"test_clean_unused":                         "db-admin: clean-unused -- client-driven deletion by age; Bakery's retention is server-side",
	"test_get_db_usage":                         "db-admin: get-db-usage -- database introspection over the cache protocol",
	"test_get_db_query_columns":                 "db-admin: get-db-query-columns -- database introspection over the cache protocol",
}

// TestUpstreamSuiteCoversEveryTest is the anti-rot assertion: every test method upstream
// declares on the external-server class is either RUN or EXCLUDED WITH A REASON. Nothing
// falls between. Bump bb_tag and add a test, and this fails loudly rather than letting the
// gate quietly shrink.
func TestUpstreamSuiteCoversEveryTest(t *testing.T) {
	bb := requireBitbake(t)

	all := discoverTests(t, bb)

	if len(all) == 0 {
		t.Fatal("discovered no tests on " + externalSuite + "; the introspection is broken")
	}

	for _, name := range all {
		_, excluded := excludedTests[name]
		if !slices.Contains(runTests, name) && !excluded {
			t.Errorf("upstream test %q is neither in runTests nor in excludedTests. It must be "+
				"one or the other, and an exclusion needs a written reason.", name)
		}
	}

	for _, name := range runTests {
		if !slices.Contains(all, name) {
			t.Errorf("runTests names %q, which %s does not declare (a rename, or a stale list)",
				name, externalSuite)
		}
	}

	for name := range excludedTests {
		if !slices.Contains(all, name) {
			t.Errorf("excludedTests names %q, which %s does not declare (a stale exclusion -- "+
				"drop it, do not leave it)", name, externalSuite)
		}
	}

	t.Logf("PROVEN: %s declares %d tests = %d run + %d excluded, and every exclusion carries a reason",
		externalSuite, len(all), len(runTests), len(excludedTests))
}

// TestUpstreamHashservSuite runs bitbake's own hashserv suite against a booted Bakery.
//
// It is run AUTHENTICATED, and that is not optional: setUp AND tearDown both call
// client.remove({"method": METHOD}), which is a @db-admin operation, and Bakery maps
// @db-admin to a WRITE-SCOPED key. Without BB_TEST_HASHSERV_USERNAME/PASSWORD every single
// test errors in setUp, and the output tells you nothing about the protocol.
//
// Bakery's credential is ONE OPAQUE bkry_ TOKEN, not an id:secret pair, so it goes in both
// fields -- the in-band `auth` RPC reads the token field and falls back to the username,
// mirroring AuthenticateCache's Basic password-then-username fallback.
func TestUpstreamHashservSuite(t *testing.T) {
	bb := requireBitbake(t)

	e := newEnv(t)

	args := []string{"-m", "unittest", "-v"}
	for _, name := range runTests {
		args = append(args, externalSuite+"."+name)
	}

	cmd := exec.CommandContext(t.Context(), bb.python, args...)

	// A temp cwd: upstream's suite writes bbhashserv-*.log files into the working directory
	// when it starts a server. It never starts one here, but a suite that litters the repo
	// on a future bitbake bump is a bad surprise to leave lying around.
	cmd.Dir = t.TempDir()
	cmd.Env = bb.env([]string{
		// The FULL address, passed verbatim to websockets.connect. The multi-tenant path
		// survives: parse_address returns the whole ws:// string untouched.
		"BB_TEST_HASHSERV=" + e.wsURL,
		"BB_TEST_HASHSERV_USERNAME=" + e.writeKey,
		"BB_TEST_HASHSERV_PASSWORD=" + e.writeKey,
	})

	out, err := cmd.CombinedOutput()
	report := string(out)

	res := parseUnittest(t, report)

	// EXACT counts, all four of them. `ran` alone is not enough and neither is the exit
	// status: get_env() calls skipTest on an empty string, so a harness that handed through
	// an empty BB_TEST_HASHSERV would skip all 17, exit 0, and report a triumphant green
	// having proven nothing at all. Pinning ran AND skipped is what makes that impossible.
	wantRan := len(runTests)

	switch {
	case res.ran != wantRan:
		t.Errorf("upstream suite ran %d tests, want exactly %d", res.ran, wantRan)
	case res.failures != 0:
		t.Errorf("upstream suite reported %d failures, want 0", res.failures)
	case res.errors != 0:
		t.Errorf("upstream suite reported %d errors, want 0", res.errors)
	case res.skipped != len(wantSkipped):
		t.Errorf("upstream suite skipped %d tests, want exactly %d", res.skipped, len(wantSkipped))
	case !slices.Equal(res.skippedNames, wantSkipped):
		t.Errorf("upstream suite skipped %v, want exactly %v", res.skippedNames, wantSkipped)
	case err != nil:
		t.Errorf("upstream suite exited non-zero with clean counts: %v", err)
	default:
		t.Logf("PROVEN: bitbake's own hashserv suite, against the real backend over the "+
			"multi-tenant WebSocket mount %s: ran=%d failures=%d errors=%d skipped=%d %v",
			e.wsURL, res.ran, res.failures, res.errors, res.skipped, res.skippedNames)

		return
	}

	t.Fatalf("the upstream hashserv suite did not pass cleanly. Its full output:\n%s", report)
}

// unittestResult is what the gate pins.
type unittestResult struct {
	ran      int
	failures int
	errors   int
	skipped  int

	// skippedNames is sorted, so the assertion is on a set and not on execution order.
	skippedNames []string
}

var (
	ranRE     = regexp.MustCompile(`(?m)^Ran (\d+) tests? in`)
	countRE   = regexp.MustCompile(`(failures|errors|skipped)=(\d+)`)
	skippedRE = regexp.MustCompile(`(?m)^(test_[A-Za-z0-9_]+) .*\.\.\. skipped`)
	verdictRE = regexp.MustCompile(`(?m)^(OK|FAILED)(?: \((.*)\))?\s*$`)
)

// parseUnittest reads the counts out of unittest's own summary. It Fatals when the summary
// is not there at all: no summary means the interpreter died before running anything (an
// import error, say), and treating "no failures reported" as "no failures" would be the
// exact green-without-proof the gate exists to prevent.
func parseUnittest(t *testing.T, out string) unittestResult {
	t.Helper()

	m := ranRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("unittest printed no \"Ran N tests\" summary -- it did not get as far as "+
			"running the suite. Its full output:\n%s", out)
	}

	res := unittestResult{ran: atoi(t, m[1])}

	v := verdictRE.FindStringSubmatch(out)
	if v == nil {
		t.Fatalf("unittest printed no OK/FAILED verdict. Its full output:\n%s", out)
	}

	for _, c := range countRE.FindAllStringSubmatch(v[2], -1) {
		switch c[1] {
		case "failures":
			res.failures = atoi(t, c[2])
		case "errors":
			res.errors = atoi(t, c[2])
		case "skipped":
			res.skipped = atoi(t, c[2])
		}
	}

	for _, s := range skippedRE.FindAllStringSubmatch(out, -1) {
		res.skippedNames = append(res.skippedNames, s[1])
	}

	slices.Sort(res.skippedNames)

	return res
}

func atoi(t *testing.T, s string) int {
	t.Helper()

	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse %q as a count: %v", s, err)
	}

	return n
}

// discoverTests asks unittest itself which tests the class declares. Introspection, not a
// hand-maintained copy of upstream's file: the point of the partition assertion is to
// notice when upstream changes, and a list we wrote down cannot notice anything.
func discoverTests(t *testing.T, bb bbEnv) []string {
	t.Helper()

	const probe = `import unittest
import hashserv.tests as t
print("\n".join(unittest.TestLoader().getTestCaseNames(t.TestHashEquivalenceExternalServer)))`

	cmd := exec.CommandContext(t.Context(), bb.python, "-c", probe)
	cmd.Env = bb.env(nil)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("enumerate %s: %v\n%s", externalSuite, err, out)
	}

	return strings.Fields(string(out))
}

// ---------------------------------------------------------------------------
// HALF 2 -- the real bitbake-hashclient, which upstream's external suite never runs.
// ---------------------------------------------------------------------------

// hcMethod is bitbake-hashclient's own hardcoded stress method name (METHOD in
// bin/bitbake-hashclient). The `stress` subcommand is the only one that both READS and
// REPORTS, so it is what seeds the rows the rest of this half then queries.
const hcMethod = "stress.test.method"

// The seeds fix the taskhashes and outhashes stress will use: taskhash_i =
// sha256(taskhash_seed + str(i)), outhash_i = sha256(outhash_seed + str(i)), and the
// reported unihash IS the taskhash. Recomputing them in Go is what lets the assertions
// below name a specific row.
const (
	hcTaskSeed = "bakery-conformance-task"
	hcOutSeed  = "bakery-conformance-out"
	hcRequests = 4
)

// TestBitbakeHashclient drives the REAL bitbake-hashclient binary against Bakery.
//
// Upstream never does this against an external server: every run_hashclient call site is in
// TestHashEquivalenceClient, which spawns its own local unix:// python server. So the
// operator CLI has never been pointed at a multi-tenant ws:// mount by anybody, and the
// subcommands below (get, get-outhash, report-via-stress, unihash-exists, remove) are the
// ones an operator actually reaches for.
//
// Every invocation passes -n/--no-netrc: bitbake-hashclient reads ~/.netrc keyed on the
// FULL ws:// URL, and a stray netrc on a runner must not be able to perturb the run.
func TestBitbakeHashclient(t *testing.T) {
	bb := requireBitbake(t)

	e := newEnv(t)

	taskhash0 := hcTaskhash(0)
	outhash0 := hcOuthash(0)

	// -----------------------------------------------------------------------
	// 1. report. `stress --report` is the client's only write path, and its READER
	//    connections are ANONYMOUS (handle_stress calls create_client(address) with no
	//    credential), so this one command exercises both halves of the open-mirror rule:
	//    anonymous reads are served, and the write rides the write-scoped key.
	// -----------------------------------------------------------------------
	t.Run("report_writes_through_the_real_client", func(t *testing.T) {
		out := hcMust(t, bb, e, authed, "stress",
			"--report", "--clients", "1", "--requests", strconv.Itoa(hcRequests),
			"--taskhash-seed", hcTaskSeed, "--outhash-seed", hcOutSeed)

		// The reads happen BEFORE the reports, so on a virgin cache every one of them
		// misses. A "found" here would mean the seeds collided with an existing row and
		// the rest of this test would be proving nothing.
		want := fmt.Sprintf("Found 0 hashes, missed %d", hcRequests)
		if !strings.Contains(out, want) {
			t.Fatalf("stress --report reported %q, want %q\n%s", firstLines(out), want, out)
		}

		t.Logf("PROVEN: real bitbake-hashclient reported %d unihashes over the write-scoped key",
			hcRequests)
	})

	// -----------------------------------------------------------------------
	// 2. get-stream. The same seeds, no --report, two client connections: every lookup must
	//    now hit. This is the round trip -- the reports above went into Postgres and come
	//    back out through the streaming hot path bitbake actually uses.
	// -----------------------------------------------------------------------
	t.Run("get_stream_reads_them_back", func(t *testing.T) {
		out := hcMust(t, bb, e, anonymous, "stress",
			"--clients", "2", "--requests", strconv.Itoa(hcRequests),
			"--taskhash-seed", hcTaskSeed)

		want := fmt.Sprintf("Found %d hashes, missed 0", 2*hcRequests)
		if !strings.Contains(out, want) {
			t.Fatalf("stress reported %q, want %q\n%s", firstLines(out), want, out)
		}

		t.Logf("PROVEN: %d anonymous get-stream lookups over 2 connections, all hits", 2*hcRequests)
	})

	// -----------------------------------------------------------------------
	// 2b. THE CONCURRENCY PROOF, and it lives here because upstream's is broken.
	//
	// bitbake's own test_stress spawns 100 threads that all die instantly with a NameError
	// (tests.py:300 references an unimported `Client`), so its assertion passes over an
	// empty failure list and proves nothing -- see the note on test_stress in runTests.
	// bitbake-hashclient's `stress` subcommand has no such bug: it builds each thread's
	// client with hashserv.create_client, so this really is N connections issuing lookups at
	// once, which is the shape of the setscene storm.
	//
	// What it catches: the protocol has NO REQUEST IDS. Responses are strictly in order, one
	// per request, so a second goroutine writing to a connection -- or an interleaved
	// response -- desynchronizes that connection permanently and SILENTLY, and bitbake then
	// hangs forever with no error. A mismatched answer surfaces here as a "missed" hash.
	// -----------------------------------------------------------------------
	t.Run("a_concurrent_lookup_storm_stays_in_order", func(t *testing.T) {
		const (
			clients  = 20
			requests = 50
		)

		// Fresh seeds: this must not read the rows the subtests above wrote.
		const (
			taskSeed = "bakery-conformance-storm-task"
			outSeed  = "bakery-conformance-storm-out"
		)

		out := hcMust(t, bb, e, authed, "stress",
			"--report", "--clients", strconv.Itoa(clients), "--requests", strconv.Itoa(requests),
			"--taskhash-seed", taskSeed, "--outhash-seed", outSeed)

		// The read pass runs BEFORE the report pass, over a cache that has none of these:
		// clients x requests concurrent lookups, every one a miss. The negative path is the
		// one a real first build takes, and it is the one an LRU that caches only positives
		// would send to Postgres in full.
		want := fmt.Sprintf("Found 0 hashes, missed %d", clients*requests)
		if !strings.Contains(out, want) {
			t.Fatalf("the cold storm reported %q, want %q\n%s", firstLines(out), want, out)
		}

		// Now the same storm against a warm cache. Every lookup must return the unihash that
		// belongs to ITS taskhash -- the client counts a mismatch as a miss, so a single
		// crossed response anywhere in 1000 concurrent round trips fails this.
		out = hcMust(t, bb, e, anonymous, "stress",
			"--clients", strconv.Itoa(clients), "--requests", strconv.Itoa(requests),
			"--taskhash-seed", taskSeed)

		want = fmt.Sprintf("Found %d hashes, missed 0", clients*requests)
		if !strings.Contains(out, want) {
			t.Fatalf("the warm storm reported %q, want %q\n%s", firstLines(out), want, out)
		}

		// The storm's rows are left in place: they share hcMethod with the rows above but not
		// their taskhashes, so they neither answer nor hide any assertion below -- and the
		// authenticated `remove` at the end of this test purges the method wholesale anyway,
		// which is a slightly harder thing to ask of it than purging four rows.
		t.Logf("PROVEN: %d concurrent lookups over %d connections, cold (all misses) and warm "+
			"(all hits, none crossed) -- the proof upstream's test_stress cannot give, because "+
			"in 2.8.0 all 100 of its threads die with a NameError before they connect",
			clients*requests, clients)
	})

	// -----------------------------------------------------------------------
	// 3. get (the `get` RPC, all properties).
	// -----------------------------------------------------------------------
	t.Run("get_returns_the_reported_unihash", func(t *testing.T) {
		out := hcMust(t, bb, e, anonymous, "get", hcMethod, taskhash0)

		row := hcJSON(t, out)

		if row["unihash"] != taskhash0 {
			t.Errorf("get unihash = %v, want %v", row["unihash"], taskhash0)
		}

		if row["method"] != hcMethod || row["taskhash"] != taskhash0 {
			t.Errorf("get returned method %v taskhash %v, want %v / %v",
				row["method"], row["taskhash"], hcMethod, taskhash0)
		}

		t.Logf("PROVEN: `bitbake-hashclient get` -> %s", out)
	})

	// -----------------------------------------------------------------------
	// 4. get-outhash. Not called from lib/bb, but it is the upstream-chaining primitive and
	//    the operator's only way to ask "what produced this output?".
	// -----------------------------------------------------------------------
	t.Run("get_outhash_returns_the_row", func(t *testing.T) {
		out := hcMust(t, bb, e, anonymous, "get-outhash", hcMethod, outhash0, taskhash0)

		row := hcJSON(t, out)

		if row["unihash"] != taskhash0 || row["outhash"] != outhash0 {
			t.Errorf("get-outhash = unihash %v outhash %v, want %v / %v",
				row["unihash"], row["outhash"], taskhash0, outhash0)
		}

		t.Logf("PROVEN: `bitbake-hashclient get-outhash` -> %s", out)
	})

	// -----------------------------------------------------------------------
	// 5. unihash-exists (the exists-stream RPC), both answers.
	// -----------------------------------------------------------------------
	t.Run("unihash_exists_answers_true_and_false", func(t *testing.T) {
		out := hcMust(t, bb, e, anonymous, "unihash-exists", taskhash0)
		if strings.TrimSpace(out) != "true" {
			t.Errorf("unihash-exists %s = %q, want \"true\"", taskhash0, strings.TrimSpace(out))
		}

		// Well-formed (64 hex) and definitely not reported. A negative result must be a
		// clean "false", not an error -- the LRU caches negative results precisely because
		// on a cold cache every lookup is one of these.
		unknown := sha256hex("no-such-unihash")

		out = hcMust(t, bb, e, anonymous, "unihash-exists", unknown)
		if strings.TrimSpace(out) != "false" {
			t.Errorf("unihash-exists %s = %q, want \"false\"", unknown, strings.TrimSpace(out))
		}

		t.Logf("PROVEN: `bitbake-hashclient unihash-exists` -> true for a known unihash, false for an unknown one")
	})

	// -----------------------------------------------------------------------
	// 6. THE INVARIANT: a write with no credential is refused, LOUDLY. `remove` is
	//    @db-admin, which Bakery maps to a write-scoped key. An anonymous caller holds
	//    permRead on this open mirror and nothing else.
	//
	//    Loud is the whole point. The denial travels in-band as {"invoke-error": ...},
	//    which bb.asyncrpc raises as InvokeError -- it is in no retry tuple and has no
	//    except on the build path, so it HALTS. A 401 on the upgrade would instead surface
	//    as ConnectionError, which bb.siggen CATCHES: the build would complete with
	//    unihash = taskhash and a silently dead cache.
	// -----------------------------------------------------------------------
	t.Run("an_unauthenticated_remove_is_refused_loudly", func(t *testing.T) {
		out, err := hcRun(t, bb, e, anonymous, "remove", "-w", "method", hcMethod)
		if err == nil {
			t.Fatalf("an unauthenticated `remove` SUCCEEDED. A write with no credential must "+
				"never be accepted -- it is a cache-poisoning vector.\n%s", out)
		}

		if !strings.Contains(out, "ERROR") {
			t.Errorf("the unauthenticated `remove` failed without an in-band ERROR:\n%s", out)
		}

		// And it really did not write: the rows are all still there.
		if got := strings.TrimSpace(hcMust(t, bb, e, anonymous, "unihash-exists", taskhash0)); got != "true" {
			t.Errorf("after the refused remove, unihash-exists = %q, want \"true\" -- the "+
				"denied call deleted rows anyway", got)
		}

		t.Logf("PROVEN: an unauthenticated remove is an in-band invoke-error the real client "+
			"surfaces as `ERROR` and a non-zero exit, and it wrote nothing: %s", firstLines(out))
	})

	// -----------------------------------------------------------------------
	// 7. remove, authenticated -- and then the rows really are gone. This is the RPC the
	//    upstream suite's setUp/tearDown depend on, which is why Bakery implements it.
	// -----------------------------------------------------------------------
	t.Run("remove_with_a_write_key_purges_the_method", func(t *testing.T) {
		out := hcMust(t, bb, e, authed, "remove", "-w", "method", hcMethod)

		n := removedCount(t, out)
		if n < hcRequests {
			t.Errorf("remove reported %d row(s), want at least the %d unihashes reported\n%s",
				n, hcRequests, out)
		}

		if got := strings.TrimSpace(hcMust(t, bb, e, anonymous, "unihash-exists", taskhash0)); got != "false" {
			t.Errorf("after remove, unihash-exists = %q, want \"false\"", got)
		}

		// `get` on a purged taskhash prints nothing at all (the client prints the row only
		// when there is one) and still exits 0.
		if got := strings.TrimSpace(hcMust(t, bb, e, anonymous, "get", hcMethod, taskhash0)); got != "" {
			t.Errorf("after remove, get printed %q, want nothing", got)
		}

		t.Logf("PROVEN: `bitbake-hashclient remove -w method %s` over the write key removed %d "+
			"row(s), and the method is empty afterwards", hcMethod, n)
	})
}

// credentials says whether an invocation carries the write-scoped key.
type credentials bool

const (
	anonymous credentials = false
	authed    credentials = true
)

// hcRun runs the REAL bitbake-hashclient and returns its combined output.
//
// It is invoked through bb.python rather than by its shebang on purpose: the shebang is
// `#!/usr/bin/env python3`, which picks whatever python3 is first on PATH -- not
// necessarily the interpreter the pinned websockets was installed for.
func hcRun(t *testing.T, bb bbEnv, e *env, creds credentials, args ...string) (string, error) {
	t.Helper()

	// -n: never read ~/.netrc. bitbake-hashclient keys netrc on the full ws:// URL, and a
	// runner with a stray netrc must not be able to change what this suite proves.
	argv := []string{bb.hashclient, "--address", e.wsURL, "-n"}

	if creds == authed {
		// One opaque token, in BOTH fields. There is no id:secret pair to split: the server
		// reads the token field and falls back to the username, so either alone would do --
		// and the snippet generator emits it in both for exactly this reason.
		argv = append(argv, "-l", e.writeKey, "-p", e.writeKey)
	}

	argv = append(argv, args...)

	cmd := exec.CommandContext(t.Context(), bb.python, argv...)
	cmd.Dir = t.TempDir()
	cmd.Env = bb.env(nil)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// hcMust runs bitbake-hashclient and fails the test if it exits non-zero.
func hcMust(t *testing.T, bb bbEnv, e *env, creds credentials, args ...string) string {
	t.Helper()

	out, err := hcRun(t, bb, e, creds, args...)
	if err != nil {
		t.Fatalf("bitbake-hashclient %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// hcJSON decodes the client's `get`/`get-outhash` output, which is a JSON object.
func hcJSON(t *testing.T, out string) map[string]any {
	t.Helper()

	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("decode the hashclient's JSON output: %v\n%s", err, out)
	}

	return row
}

var removedRE = regexp.MustCompile(`Removed (\d+) row`)

func removedCount(t *testing.T, out string) int {
	t.Helper()

	m := removedRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("`remove` printed no \"Removed N row(s)\" line:\n%s", out)
	}

	return atoi(t, m[1])
}

// hcTaskhash and hcOuthash recompute bitbake-hashclient's stress hashes: sha256(seed + i).
func hcTaskhash(i int) string { return sha256hex(hcTaskSeed + strconv.Itoa(i)) }
func hcOuthash(i int) string  { return sha256hex(hcOutSeed + strconv.Itoa(i)) }

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// firstLines trims a long capture down to something a failure message can carry.
func firstLines(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 4 {
		lines = lines[:4]
	}

	return strings.Join(lines, " | ")
}
