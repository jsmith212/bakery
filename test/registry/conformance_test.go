// Package regconf proves that Bakery's WRITABLE `buildcache` namespace -- the
// `registry` backend kind, the target of BuildKit's `--cache-to type=registry` --
// serves the real BuildKit. It is the buildkit-cache-export spec's §8 gate, and
// DESIGN.md calls the conformance gates non-negotiable.
//
// # Why a gate, when the unit suite already covers the wire surface
//
// internal/cache/oci's own tests drive every status code and every header of the push
// API. What they cannot do is prove that the SEQUENCE a real client emits -- HEAD blob,
// POST uploads, monolithic PUT with ?digest=, HEAD manifests/<tag>, PUT manifests --
// lands on a server that answers each one the way containerd's pusher demands. Three of
// those answers are silent build-breakers when wrong and produce NO client-side error:
//
//	a 202 (rather than 201) from the final blob PUT is HARD-REJECTED by containerd;
//	a missing or wrong Docker-Content-Digest on HEAD manifests/<tag> re-pushes every
//	manifest on every build; and an export failure of ANY kind is invisible, because
//	the snippet -- correctly -- carries ignore-error=true, which is what keeps a Bakery
//	outage from failing an arcturus build.
//
// So every success assertion here is paired with an assertion about the RECORDED
// TRAFFIC, never about the build's exit status alone: "the build was green" is the one
// thing a completely broken cache also produces.
//
// # The clients
//
//	docker buildx, container driver, network=host, against a live Bakery on loopback:
//	the real BuildKit, the real containerd pusher, the real cache exporter and
//	importer. Both manifest shapes -- the default OCI index whose manifests[] entries
//	are blob descriptors, and image-manifest=true with its
//	application/vnd.buildkit.cacheconfig.v0 config -- because a registry that parses
//	either one rejects it, and Bakery parses neither.
//
//	skopeo, if installed: the byte-identity proof. --raw prints the manifest bytes a
//	third-party client actually received, and comparing them to the bytes we pushed is
//	how "stored and served verbatim" is proved by something other than the code that
//	stores them.
//
// # The anti-regression assertion (spec §8.4)
//
// The writable namespace HAS NO UPSTREAM -- NewBuildCache takes no Fetcher at all -- so
// the open-relay class of bug is unrepresentable rather than disabled. That claim is
// worth an assertion anyway, because it is a claim about ROUTING as much as about
// construction: the pull-through mirror is mounted on the SAME project, pointed at a
// fake upstream that counts every request it receives, and every test here ends by
// asserting that counter is still zero. A buildcache request that fell through to the
// mirror's {rest...} pattern would show up there and nowhere else.
//
// It lives OUTSIDE internal/ on purpose. `just race`/`just coverage` glob ./... and
// compile this package, so each binary-driven test calls its require* guard FIRST,
// before dbtest.New spawns a Postgres. `just registry-conformance` is its home, and
// there a SKIP FAILS THE JOB.
package regconf

import (
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db/dbtest"
)

// TestMain gives the suite its own ephemeral Postgres. dbtest.Main is lazy -- the
// container (or the TEST_DB_URL template clone) is created by the first dbtest.New and
// by nothing else -- so a run that skips every test pays nothing for it.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// httpResult is one raw exchange with Bakery: everything an assertion might need,
// already drained.
type httpResult struct {
	status int
	header http.Header
	body   []byte
}

// request issues one request at Bakery with no client library in the way -- the push
// helpers in env_test.go are built on it, because the point of a push is the exact
// bytes and the exact headers on the wire.
func request(
	t *testing.T, method, url string, body io.Reader, headers map[string]string,
) httpResult {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}

	return httpResult{status: resp.StatusCode, header: resp.Header, body: raw}
}

// ---------------------------------------------------------------------------
// The guards. Each runs BEFORE dbtest.New, and each skip is loud enough that a reader
// of a CI log knows a proof did not run rather than that a test passed.
// ---------------------------------------------------------------------------

// requireBinary skips loudly -- and CHEAPLY, before dbtest.New spawns a Postgres -- when
// the named client binary is not installed. `just registry-conformance` installs the
// clients and turns a skip into a job failure; `just race`/`just coverage` glob ./... and
// do not, so on a laptop without the client this returns before anything expensive.
func requireBinary(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skip(skipMsg("the \"" + name + "\" binary is not on PATH: " + err.Error()))
	}

	return path
}

// requireBuildx returns the docker binary, having proved the buildx PLUGIN answers.
//
// It checks the plugin rather than a `buildx` executable on PATH: the supported
// installation is `docker buildx` (a CLI plugin under ~/.docker/cli-plugins or
// /usr/libexec/docker/cli-plugins), which is what ubuntu-latest ships and what Docker
// Desktop installs. A standalone buildx binary on PATH is neither necessary nor
// sufficient, so looking for one would skip this gate on almost every machine that can
// actually run it.
func requireBuildx(t *testing.T) string {
	t.Helper()

	docker := requireBinary(t, "docker")

	out, err := exec.CommandContext(t.Context(), docker, "buildx", "version").CombinedOutput()
	if err != nil {
		t.Skip(skipMsg("`docker buildx version` failed -- the buildx plugin is not usable: " +
			err.Error() + "\n  output: " + strings.TrimSpace(string(out))))
	}

	return docker
}

// skipMsg renders the one message shape this suite skips with. It says what did not
// run, not merely what was missing: a green line in a CI log that means "this proof was
// never attempted" is the failure mode every conformance recipe in this repo exists to
// prevent.
func skipMsg(reason string) string {
	return "\n" + strings.Repeat("=", 80) + "\n" +
		"SKIPPING REGISTRY CONFORMANCE -- the real client did not run.\n\n" +
		"  reason: " + reason + "\n\n" +
		"  This suite drives the REAL docker buildx container driver (and skopeo) at the\n" +
		"  writable buildcache namespace: it is the only proof that BuildKit's cache\n" +
		"  export -- whose every failure is silent, because the snippet carries\n" +
		"  ignore-error=true -- actually lands. It needs docker with the buildx plugin, a\n" +
		"  docker daemon whose host network can reach this test's listener, skopeo, and a\n" +
		"  Postgres (docker or TEST_DB_URL). Run it with `just registry-conformance`,\n" +
		"  which installs the clients and fails on a skip.\n" +
		"\n  This proof did not run.\n" +
		strings.Repeat("=", 80)
}
