package regconf

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two strings buildx prints that this gate reads, and the one option that makes
// every export failure silent.
//
// progressImporting is BuildKit's own vertex name for a resolved remote cache manifest.
// It appears if and ONLY if the import found something: a miss is a debug log and a
// vertex that never runs, because cache import failure is unconditionally soft.
//
// progressCached is what a vertex prints when its result came from cache rather than
// from execution. After `buildx prune -af` the builder holds nothing, so on a warm build
// it can only have come from the registry.
//
// ignoreError is in EVERY --cache-to here because it is in the snippet the console
// emits, and it is in the snippet because a failed export otherwise HARD-FAILS the
// build (solver/llbsolver/export.go:139) -- a Bakery outage must not fail an arcturus
// build. Its cost is that a broken cache is indistinguishable from a working one by exit
// status alone, which is why every assertion below is against recorded traffic.
const (
	progressImporting = "importing cache manifest"
	progressCached    = "CACHED"
	ignoreError       = "ignore-error=true"
)

// TestBuildxCacheExportImport is spec §8.1 and §8.2: the real docker buildx container
// driver exporting to, and importing from, the writable buildcache namespace.
//
// The subtests share ONE env and ONE builder and run in order, because creating a
// buildx container driver costs an image pull and a container start, and because the
// read-only-key case is only interesting against a namespace something has already
// written.
//
// Both manifest shapes are covered, and they get DIFFERENT build contexts on purpose:
// identical contexts would make the second export's blobs already present, containerd
// would skip every upload after the HEAD, and the second shape would prove only that a
// manifest PUT works.
func TestBuildxCacheExportImport(t *testing.T) {
	docker := requireBuildx(t)
	spec := dockerListenSpec(t, docker)

	e := newEnv(t, spec)
	b := newBuilder(t, docker, e)

	shapes := []struct {
		name string

		// opts is appended to --cache-to. The empty string is the DEFAULT export: an OCI
		// image INDEX whose manifests[] entries are blob descriptors rather than
		// manifests, which is what trips validating registries. image-manifest=true is
		// the other shape, whose config media type is not an image config at all.
		opts string

		dir string
		ref string
	}{
		{name: "index", opts: ""},
		{name: "image-manifest", opts: ",image-manifest=true"},
	}

	for i := range shapes {
		shapes[i].dir = newBuildContext(t, shapes[i].name)
		shapes[i].ref = e.ref("impulse-"+shapes[i].name, "main")
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			// Inside the subtest, not once above it: the read-only case below rewrites
			// this file, and a credential set by a sibling is exactly the coupling that
			// makes a reordered test suite lie.
			b.login(t, e.writeKey)

			cacheTo := "type=registry,ref=" + shape.ref +
				",mode=max,oci-mediatypes=true," + ignoreError + shape.opts

			// Prune before the COLD build too: a builder warmed by the previous subtest
			// would export a cache it never computed, which still exports -- but then the
			// warm build below would prove nothing about what came off the wire.
			b.prune(t)
			e.rec.reset()

			out := b.build(t, shape.dir, "--cache-to", cacheTo)

			assertExported(t, e, out)

			b.prune(t)
			e.rec.reset()

			out = b.build(t, shape.dir, "--cache-from", "type=registry,ref="+shape.ref)

			assertImported(t, e, out)
		})
	}

	// Spec §8.2. It is one build because the two halves are one claim: with a read-scoped
	// key an export is refused and an import still works, AND the build is green anyway.
	t.Run("a read-scoped key is refused a push and the build stays green", func(t *testing.T) {
		shape := shapes[0]

		// A repository NOTHING HAS WRITTEN, which is what forces a fresh token: BuildKit
		// computes `repository:<name>:pull,push` from the operation and caches tokens per
		// scope, so re-using an already-pushed repository could replay the write-scoped
		// token this subtest is trying to leave behind.
		//
		// Its BLOBS may exist all the same -- they are content-addressed and shared
		// across every repository under this backend_id, so containerd's HEAD answers 200
		// and it goes straight to the manifest. Which write verb gets refused therefore
		// depends on what the cache already holds, and the assertion below is written
		// against ANY write verb rather than a chosen one.
		denied := e.ref("impulse-denied", "main")

		b.login(t, e.readKey)
		b.prune(t)
		e.rec.reset()

		out := b.build(t, shape.dir,
			"--cache-to", "type=registry,ref="+denied+",mode=max,oci-mediatypes=true,"+ignoreError,
			"--cache-from", "type=registry,ref="+shape.ref)

		// The export half: refused, and refused with a code that means "not authorized"
		// rather than "not found".
		refused := e.rec.count(func(r recordedReq) bool {
			return isBuildCache(r) && isWriteVerb(r) &&
				(r.status == http.StatusUnauthorized || r.status == http.StatusForbidden)
		})
		if refused == 0 {
			t.Errorf("a read-scoped key's export was never refused: no 401/403 on any write "+
				"verb. Writes ALWAYS require a write-scoped key -- an unauthenticated or "+
				"read-only write is a cache-poisoning vector.%s", e.rec.summary())
		}

		accepted := e.rec.count(func(r recordedReq) bool {
			return isBuildCache(r) && isWriteVerb(r) &&
				(r.status == http.StatusCreated || r.status == http.StatusAccepted)
		})
		if accepted != 0 {
			t.Errorf("a read-scoped key had %d write verb(s) accepted (201/202)%s",
				accepted, e.rec.summary())
		}

		// The green half. b.build already failed the test on a non-zero exit; saying so
		// here is what names the claim: ignore-error=true is under test, not assumed.
		if strings.Contains(out, "ERROR: failed to solve") {
			t.Errorf("the build reported a solve failure despite %s on --cache-to:\n%s",
				ignoreError, out)
		}

		// The import half: still works, with no write authority anywhere.
		assertImported(t, e, out)
	})

	// Spec §8.4, over the whole gate: the writable namespace has no upstream, and no
	// request to it was ever answered by the pull-through mirror mounted beside it.
	e.assertNoUpstream(t)
}

// assertExported proves a cache export LANDED, against the recorded traffic rather than
// against the build's exit status -- which ignore-error=true makes meaningless.
//
// The three positive assertions are the three steps of containerd's push, in order. The
// 5xx assertion is separate because a 404 and a 401 are both NORMAL here: containerd
// HEADs every blob before uploading it (404 = "proceed") and makes its first request of
// a session anonymously (401 = "here is the challenge, go get a token").
func assertExported(t *testing.T, e *env, out string) {
	t.Helper()

	for _, want := range []struct {
		what   string
		match  func(recordedReq) bool
		reason string
	}{
		{
			what: "POST .../blobs/uploads/ answered 202",
			match: func(r recordedReq) bool {
				return isBuildCache(r) && r.method == http.MethodPost &&
					r.status == http.StatusAccepted
			},
			reason: "containerd parses Location out of a 200/202/204 and has nowhere to " +
				"PUT without one",
		},
		{
			what: "PUT of a blob answered 201",
			match: func(r recordedReq) bool {
				return isBuildCache(r) && r.method == http.MethodPut &&
					strings.Contains(r.path, "/blobs/uploads/") && r.status == http.StatusCreated
			},
			reason: "the final blob PUT must answer 200/201/204 -- containerd HARD-REJECTS " +
				"a 202 there (pusher.go:329)",
		},
		{
			what: "PUT of the manifest answered 201",
			match: func(r recordedReq) bool {
				return isBuildCache(r) && r.method == http.MethodPut &&
					strings.Contains(r.path, "/manifests/") && r.status == http.StatusCreated
			},
			reason: "without the manifest there is no cache to import, and with " +
				ignoreError + " nothing says so",
		},
	} {
		if e.rec.count(want.match) == 0 {
			t.Errorf("the export never got a %s: %s%s\nbuild output:\n%s",
				want.what, want.reason, e.rec.summary(), out)
		}
	}

	if n := e.rec.count(func(r recordedReq) bool { return isBuildCache(r) && r.status >= 500 }); n != 0 {
		t.Errorf("%d buildcache request(s) were answered 5xx during an export%s",
			n, e.rec.summary())
	}
}

// assertImported proves the warm build's results came from the REGISTRY.
//
// The builder was pruned, so a CACHED vertex has only one possible source; and BuildKit
// prints its importing-cache-manifest vertex only when the import actually resolved a
// manifest, because a miss is soft and silent. The recorded 200s are the third leg:
// they say the bytes came from Bakery rather than from anywhere else the client might
// have reached.
func assertImported(t *testing.T, e *env, out string) {
	t.Helper()

	if !strings.Contains(out, progressImporting) {
		t.Errorf("the build never printed %q: the cache manifest was not imported, and "+
			"BuildKit treats an import miss as a soft, silent cold build\n%s",
			progressImporting, out)
	}

	if !strings.Contains(out, progressCached) {
		t.Errorf("no step was %s after a prune: the imported cache was resolved but nothing "+
			"was reused from it\n%s", progressCached, out)
	}

	for _, want := range []struct {
		what  string
		match func(recordedReq) bool
	}{
		{
			what: "a manifest read answered 200",
			match: func(r recordedReq) bool {
				return isBuildCache(r) && !isWriteVerb(r) &&
					strings.Contains(r.path, "/manifests/") && r.status == http.StatusOK
			},
		},
		{
			what: "a blob read answered 200",
			match: func(r recordedReq) bool {
				return isBuildCache(r) && !isWriteVerb(r) &&
					strings.Contains(r.path, "/blobs/") && r.status == http.StatusOK
			},
		},
	} {
		if e.rec.count(want.match) == 0 {
			t.Errorf("the import never got %s -- the cache was not served from Bakery%s\n%s",
				want.what, e.rec.summary(), out)
		}
	}
}

// ---------------------------------------------------------------------------
// The builder: a real buildx container driver, pointed at this test's server.
// ---------------------------------------------------------------------------

// builder owns one `docker buildx` container-driver instance and the isolated
// DOCKER_CONFIG that carries its credentials.
//
// DOCKER_CONFIG is a temp dir rather than the user's real one for two reasons, and only
// one of them is hygiene: buildx keeps its builder registry there too, so an isolated
// config also means this gate cannot collide with, or clobber, a developer's own
// builders -- and `docker login` against the user's config file would write a Bakery
// token into it.
type builder struct {
	docker string
	name   string
	cfgDir string
	host   string
}

// newBuilder creates the driver and tears it down afterwards.
//
// --driver-opt network=host is what lets buildkitd -- which runs INSIDE the docker
// daemon, not in this process -- reach a listener this test bound. --config carries a
// buildkitd.toml marking this host `http = true`: BuildKit has no localhost-implies-http
// rule (go-containerregistry does, which is why the pull-through gate needs no config),
// so without it every request is attempted over TLS against a cleartext server and the
// export dies before it starts.
func newBuilder(t *testing.T, docker string, e *env) *builder {
	t.Helper()

	dir := t.TempDir()
	toml := filepath.Join(dir, "buildkitd.toml")

	if err := os.WriteFile(toml,
		[]byte(fmt.Sprintf("[registry.%q]\n  http = true\n", e.host)), 0o600); err != nil {
		t.Fatalf("write buildkitd.toml: %v", err)
	}

	b := &builder{
		docker: docker,
		name:   fmt.Sprintf("bakery-regconf-%d", time.Now().UnixNano()),
		cfgDir: t.TempDir(),
		host:   e.host,
	}

	// --bootstrap starts the container now, so a driver that cannot come up fails here
	// with its own error rather than three minutes later inside a build.
	out, err := b.run(t, "buildx", "create",
		"--name", b.name,
		"--driver", "docker-container",
		"--driver-opt", "network=host",
		"--driver-opt", "image="+buildkitImage,
		"--config", toml,
		"--bootstrap")
	if err != nil {
		t.Fatalf("docker buildx create: %v\n%s", err, out)
	}

	t.Cleanup(b.remove)

	return b
}

// login writes the credential buildx forwards to buildkitd for this registry host.
//
// It is a config.json rather than a `docker login` call because login would prompt or
// need a stdin dance, and because the file IS the interface: buildx's auth provider
// reads it per build and sends the credential over the session, so rewriting it between
// builds is how this gate switches from a write-scoped key to a read-scoped one.
func (b *builder) login(t *testing.T, token string) {
	t.Helper()

	// One opaque token, both fields' worth: "bakery" is filler, the token is the whole
	// credential and authenticates from either half.
	auth := base64.StdEncoding.EncodeToString([]byte("bakery:" + token))

	body := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, b.host, auth)

	if err := os.WriteFile(filepath.Join(b.cfgDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write docker config.json: %v", err)
	}
}

// build runs one buildx build and returns its combined output.
//
// --output type=cacheonly is the whole point of the build: this gate is about the cache,
// not about an image, and cacheonly means no exporter, no image store write and nothing
// to clean up. --progress=plain makes the vertex log parseable; the default tty renderer
// rewrites lines in place and prints CACHED nowhere a test can read it.
func (b *builder) build(t *testing.T, contextDir string, cacheArgs ...string) string {
	t.Helper()

	args := []string{
		"buildx", "build",
		"--builder", b.name,
		"--progress=plain",
		"--output", "type=cacheonly",
	}
	args = append(args, cacheArgs...)
	args = append(args, contextDir)

	out, err := b.run(t, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// prune empties the builder's own cache, which is what makes a CACHED step on the next
// build mean "it came from the registry" instead of "it never left".
func (b *builder) prune(t *testing.T) {
	t.Helper()

	if out, err := b.run(t, "buildx", "prune", "-af", "--builder", b.name); err != nil {
		t.Fatalf("docker buildx prune: %v\n%s", err, out)
	}
}

// remove tears the driver's container down.
//
// It runs on its OWN context: t.Cleanup fires after the test's context is cancelled, so
// a cleanup that used t.Context() would be killed before it removed anything and would
// leak a container per run.
func (b *builder) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.docker, "buildx", "rm", "-f", b.name)
	cmd.Env = b.env()

	_ = cmd.Run()
}

func (b *builder) run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), b.docker, args...)
	cmd.Env = b.env()

	out, err := cmd.CombinedOutput()

	//nolint:wrapcheck // the caller renders the command and the output; decorating here
	// would only bury them.
	return string(out), err
}

func (b *builder) env() []string {
	return append(os.Environ(), "DOCKER_CONFIG="+b.cfgDir)
}

// newBuildContext writes a build context whose steps are cacheable and whose content is
// unique to `seed`.
//
// FROM scratch, so the gate needs no base image and reaches no registry but Bakery's --
// the only image anything pulls here is buildkitd itself. Two COPY steps rather than
// one: a single-vertex build makes "≥1 CACHED step" and "the whole build was cached"
// the same assertion, and they are not the same claim.
func newBuildContext(t *testing.T, seed string) string {
	t.Helper()

	dir := t.TempDir()

	files := map[string]string{
		"Dockerfile": "FROM scratch\nCOPY alpha /alpha\nCOPY beta /beta\n",
		"alpha":      "alpha for " + seed + "\n",
		"beta":       "beta for " + seed + "\n",
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write build context %s: %v", name, err)
		}
	}

	return dir
}
