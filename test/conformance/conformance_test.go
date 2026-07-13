// Package conformance proves that Bakery serves the REAL bitbake client.
//
// The house rule for this milestone is: drive the ACTUAL client, never a
// lookalike. So this test boots the REAL sstate cache backend -- the same
// internal/cache/httpblob handler, over a real blob.Service, a real local byte
// store and a real, ephemeral Postgres -- and then points bitbake's own
// bb.fetch2 Wget fetcher and the real `wget --continue` binary at it. Every
// status-code decision (404 vs 403, 200, 206) is made by production code; the
// test only asserts on what the real client did in response.
//
// It lives OUTSIDE internal/ on purpose. `just test-db` globs ./internal/... and
// fails the job on any skip; this suite legitimately skips where the bitbake
// checkout, python3 or wget are absent (a laptop), so it must not be swept into
// that recipe. `just conformance` is its home, and there a skip DOES fail the
// job -- CI provides the client, so a skip in CI means the proof did not run.
package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	clicmd "github.com/jsmith212/bakery/internal/cli"
	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

// TestMain gives the conformance test its own ephemeral Postgres. dbtest starts a
// container (or uses TEST_DB_URL) and tears it down when the package's tests end.
func TestMain(m *testing.M) {
	dbtest.Main(m)
}

// The mount under test: one org, one project, one enabled sstate backend with
// read_auth_required=false (BitBake reads need no credential). Writes still require
// a key -- that is the whole point of the round-trip below.
const (
	confOrg     = "acme"
	confProject = "widget"
)

// ---------------------------------------------------------------------------
// The four assertions this milestone must not fake:
//
//   1. HEAD of a MISSING object -> 404, and the real fetcher does NOT fall back to
//      a GET. (A 403 would; assertion 3 proves that, which is why a miss is 404.)
//   2. HEAD of a PRESENT object -> 200 and bb.fetch2 checkstatus reports a hit.
//   3. NEGATIVE CONTROL: a handler that answers a miss with 403 DOES provoke the
//      real fetcher's full-body GET fallback. Bakery's read path structurally
//      cannot emit a 403 (a miss is 404, an unauthorized read is 401), so this is
//      asserted against a 5-line stub -- and that is the point: it is why the real
//      backend never returns 403 on the HEAD storm.
//   4. `wget --continue` resuming a partial download gets a 206 and reconstructs
//      the exact bytes, served by the backend's real http.ServeContent range path.
//
// Plus the write round-trip: an object PUT by the real `bakery sstate push`
// subcommand is then HEAD-able (by bb.fetch2) and GET-able (by wget) -- the exact
// bytes come back.
// ---------------------------------------------------------------------------

func TestBitbakeConformance(t *testing.T) {
	bbLib := requireBitbake(t)
	requireBinary(t, "python3")
	requireBinary(t, "wget")

	env := newConfEnv(t)

	// Object keys are realistic sstate names -- they carry the sstate: colon and the
	// [universal/]<hh>/<hh>/ layout. Each embeds a colon-free token so the request
	// log can be matched unambiguously whether the client sends the colon literal or
	// percent-encoded.
	const (
		missKey   = "universal/aa/bb/sstate:confmiss:core2-64:1.0-r0:abc123.tar.zst"
		hitKey    = "universal/aa/bb/sstate:confhit:core2-64:1.0-r0:def456.tar.zst"
		resumeKey = "universal/cc/dd/sstate:confresume:core2-64:1.0-r0:0ff5e7.tar.zst"
	)

	hitBody := []byte("this is a present sstate object served by the real backend")
	resumeBody := []byte("0123456789ABCDEFGHIJ") // 20 bytes; wget resumes from offset 10

	env.putObject(t, hitKey, hitBody)
	env.putObject(t, resumeKey, resumeBody)

	// -----------------------------------------------------------------------
	// 1. A miss is 404, and the real fetcher does NOT fall back to a GET.
	// -----------------------------------------------------------------------
	t.Run("head_miss_is_404_and_does_not_fall_back_to_get", func(t *testing.T) {
		out := env.driveFetcher(t, bbLib, env.objectURL(missKey))

		if got := resultFor(out, env.objectURL(missKey)); got != "miss" {
			t.Fatalf("bb.fetch2 checkstatus reported %q for the missing object, want \"miss\"\n%s", got, out)
		}

		if h := env.rec.count(http.MethodHead, "confmiss"); h == 0 {
			t.Errorf("the fetcher issued no HEAD for the miss; the request log was:\n%s", env.rec.dump())
		}

		// THE INVARIANT. A 404 must stay HEAD-only: if the backend had answered 403,
		// bitbake's HTTPMethodFallback would re-issue the whole thing as a full-body
		// GET, turning the setscene HEAD storm into a GET storm.
		if g := env.rec.count(http.MethodGet, "confmiss"); g != 0 {
			t.Errorf("a 404 miss triggered %d GET(s); a miss must never fall back to GET\n%s",
				g, env.rec.dump())
		}

		t.Logf("PROVEN: real bb.fetch2 checkstatus -> miss; %d HEAD, 0 GET on the 404 object",
			env.rec.count(http.MethodHead, "confmiss"))
	})

	// -----------------------------------------------------------------------
	// 2. A hit is 200, HEAD-only, and checkstatus reports it present.
	// -----------------------------------------------------------------------
	t.Run("head_hit_is_200_and_checkstatus_succeeds", func(t *testing.T) {
		out := env.driveFetcher(t, bbLib, env.objectURL(hitKey))

		if got := resultFor(out, env.objectURL(hitKey)); got != "hit" {
			t.Fatalf("bb.fetch2 checkstatus reported %q for the present object, want \"hit\"\n%s", got, out)
		}

		if h := env.rec.count(http.MethodHead, "confhit"); h == 0 {
			t.Errorf("the fetcher issued no HEAD for the hit\n%s", env.rec.dump())
		}

		if g := env.rec.count(http.MethodGet, "confhit"); g != 0 {
			t.Errorf("a HEAD hit issued %d GET(s); the hot path is HEAD-only\n%s", g, env.rec.dump())
		}

		t.Logf("PROVEN: real bb.fetch2 checkstatus -> hit; served HEAD-only from Stat")
	})

	// -----------------------------------------------------------------------
	// 3. NEGATIVE CONTROL (a 5-line stub, by necessity): a 403 on a miss DOES
	//    provoke the real fetcher's GET fallback. This is why the backend above
	//    returns 404 and 401, never 403, on the read path.
	// -----------------------------------------------------------------------
	t.Run("negative_control_403_triggers_the_real_get_fallback", func(t *testing.T) {
		stubRec := &recorder{}
		stub := httptest.NewServer(record(stubRec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			// The fallback GET: answer it so bb.fetch2 does not error for an unrelated
			// reason. The proof is only that a GET was issued at all.
			body := []byte("fallback body")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		})))
		defer stub.Close()

		url := stub.URL + "/forbidden-403"
		out := env.driveFetcher(t, bbLib, url)

		// The whole point of the control: a 403 is retried as a full-body GET.
		if g := stubRec.count(http.MethodGet, "forbidden-403"); g == 0 {
			t.Fatalf("a 403 did NOT trigger a GET fallback; the request log was:\n%s\ndriver:\n%s",
				stubRec.dump(), out)
		}

		t.Logf("PROVEN (against a stub, as the real backend cannot emit 403 on reads): "+
			"403 -> %d HEAD then %d GET -- the fallback bitbake's HTTPMethodFallback fires, "+
			"which is exactly why an sstate miss must be 404",
			stubRec.count(http.MethodHead, "forbidden-403"), stubRec.count(http.MethodGet, "forbidden-403"))
	})

	// -----------------------------------------------------------------------
	// 4. Real `wget --continue` against a partial file: Range -> 206 -> exact bytes.
	// -----------------------------------------------------------------------
	t.Run("wget_continue_resumes_with_206_and_exact_bytes", func(t *testing.T) {
		dir := t.TempDir()
		partial := filepath.Join(dir, "partial.tar.zst")

		// Pre-seed the first 10 bytes so wget must resume from offset 10.
		if err := os.WriteFile(partial, resumeBody[:10], 0o644); err != nil {
			t.Fatalf("seed the partial file: %v", err)
		}

		cmd := exec.Command("wget", "--debug", "--continue", "-O", partial, env.objectURL(resumeKey))
		out, _ := cmd.CombinedOutput()
		debug := string(out)

		if !strings.Contains(debug, "Range: bytes=10-") {
			t.Errorf("wget --continue did not send Range: bytes=10-\n%s", debug)
		}

		if !strings.Contains(debug, "206 Partial Content") {
			t.Errorf("the resume was not answered with 206 Partial Content\n%s", debug)
		}

		got, err := os.ReadFile(partial)
		if err != nil {
			t.Fatalf("read the resumed file: %v", err)
		}

		if !bytes.Equal(got, resumeBody) {
			t.Fatalf("resume reconstructed %q, want %q", got, resumeBody)
		}

		t.Logf("PROVEN: real wget --continue sent Range: bytes=10-, backend answered 206, "+
			"file completed to the exact %d bytes", len(got))
	})

	// -----------------------------------------------------------------------
	// 5. Write round-trip: `bakery sstate push` PUTs a missing object, then the
	//    real fetcher HEADs it (200) and wget GETs the exact bytes back.
	// -----------------------------------------------------------------------
	t.Run("roundtrip_push_then_fetch_by_the_real_client", func(t *testing.T) {
		const pushKey = "universal/ee/ff/sstate:confpush:core2-64:2.0-r1:9a9a9a.tar.zst"
		pushBody := []byte("pushed by the real bakery sstate push subcommand")

		// A local SSTATE_DIR with exactly one object, laid out the way BitBake writes
		// it. `bakery sstate push` walks it, HEADs each object, and PUTs the misses.
		sstateDir := t.TempDir()
		dst := filepath.Join(sstateDir, filepath.FromSlash(pushKey))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("make the SSTATE_DIR layout: %v", err)
		}
		if err := os.WriteFile(dst, pushBody, 0o644); err != nil {
			t.Fatalf("write the local sstate object: %v", err)
		}

		// Keep the CLI's token cache off the developer's real home directory.
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		// The ACTUAL subcommand, dispatched exactly as `main` dispatches it -- not a
		// hand-rolled PUT. --key is the write-scoped API key seeded into Postgres.
		err := clicmd.Run(t.Context(), "sstate push", config.CLI{
			Server: env.server.URL,
			Sstate: config.SstateCmd{Push: config.SstatePushCmd{
				Org: confOrg, Project: confProject, Dir: sstateDir,
				Key: env.writeKey, Concurrency: 1,
			}},
		})
		if err != nil {
			t.Fatalf("bakery sstate push: %v", err)
		}

		// The real fetcher now sees it as present.
		out := env.driveFetcher(t, bbLib, env.objectURL(pushKey))
		if got := resultFor(out, env.objectURL(pushKey)); got != "hit" {
			t.Fatalf("after push, checkstatus reported %q, want \"hit\"\n%s", got, out)
		}

		// The push HEADed (miss->404) and PUT; the fetcher HEADed (200). None of that
		// is ever a GET.
		if g := env.rec.count(http.MethodGet, "confpush"); g != 0 {
			t.Errorf("the push+HEAD round-trip issued %d GET(s); it must not\n%s", g, env.rec.dump())
		}

		// And the bytes come back byte-for-byte through a real wget GET.
		dir := t.TempDir()
		fetched := filepath.Join(dir, "fetched.tar.zst")
		cmd := exec.Command("wget", "-O", fetched, env.objectURL(pushKey))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("wget GET of the pushed object failed: %v\n%s", err, out)
		}

		got, err := os.ReadFile(fetched)
		if err != nil {
			t.Fatalf("read the fetched object: %v", err)
		}
		if !bytes.Equal(got, pushBody) {
			t.Fatalf("round-trip bytes = %q, want %q", got, pushBody)
		}

		t.Logf("PROVEN: `bakery sstate push` PUT the object; bb.fetch2 HEADs it as a hit "+
			"and wget GETs the exact %d bytes back", len(got))
	})
}

// ---------------------------------------------------------------------------
// The real backend, wired up.
// ---------------------------------------------------------------------------

// confEnv is a booted sstate backend: the real httpblob handler over a real
// blob.Service, real local storage and a real Postgres, fronted by a request
// recorder so the assertions can read what the client actually sent.
type confEnv struct {
	server   *httptest.Server
	rec      *recorder
	blobs    *blob.Service
	route    cache.Route
	writeKey string
}

func newConfEnv(t *testing.T) *confEnv {
	t.Helper()

	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	m := metrics.New()

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	byteStore := storage.NewInstrumented(local, m, metrics.DriverLocal)

	blobs, err := blob.New(blob.Config{Reader: store, Tx: store, Storage: byteStore, Metrics: m})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	authSvc, err := auth.New(auth.Deps{
		Store:    store,
		Sessions: auth.NewSessionManager(auth.NewSessionStore(pool, log), false),
		Provider: nil, Groups: nil, Metrics: m, Log: log, DevLogin: false,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	writeKey := seed(t, pool)

	deps := cache.Deps{Blobs: blobs, Metrics: m, Logger: log}
	if err := deps.Validate(); err != nil {
		t.Fatalf("cache.Deps.Validate: %v", err)
	}

	routes := httpblob.NewCachedResolver(store, log)
	authn := cacheAuth{svc: authSvc}

	// Both M2 backends, mounted exactly as server.Boot mounts them -- the sstate one
	// is the one under test; mounting downloads beside it also proves the two coexist
	// on one mux without a ServeMux registration panic.
	mux := http.NewServeMux()
	httpblob.NewSstate(deps, routes, authn).Register(mux)
	httpblob.NewDownloads(deps, routes, authn).Register(mux)

	rec := &recorder{}
	server := httptest.NewServer(record(rec, mux))
	t.Cleanup(server.Close)

	route, ok := routes.Resolve(ctx, confOrg, confProject, repository.BackendKindSstate)
	if !ok {
		t.Fatal("the seeded sstate route did not resolve; the seed is wrong")
	}

	return &confEnv{server: server, rec: rec, blobs: blobs, route: route, writeKey: writeKey}
}

// objectURL is the /cache address of one sstate object. The colon rides literal;
// the server decodes it either way and the assertions match on a colon-free token.
func (e *confEnv) objectURL(key string) string {
	return e.server.URL + "/cache/" + confOrg + "/" + confProject + "/sstate/" + key
}

// putObject seeds a present object through the REAL blob.Service, at the exact Ref
// the handler computes for that key -- so the handler then serves it end to end.
func (e *confEnv) putObject(t *testing.T, key string, body []byte) {
	t.Helper()

	ref := e.route.Ref("", "object", key)
	if _, err := e.blobs.Put(t.Context(), ref, bytes.NewReader(body),
		blob.PutOptions{Overwrite: false, Verify: blob.NoVerify()}); err != nil {
		t.Fatalf("seed object %q: %v", key, err)
	}
}

// driveFetcher runs the REAL bb.fetch2 checkstatus driver over the given URLs and
// returns its combined output. PYTHONPATH points at the bitbake checkout's lib/.
func (e *confEnv) driveFetcher(t *testing.T, bbLib string, urls ...string) string {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("testdata", "phase_a.py"))
	if err != nil {
		t.Fatalf("resolve the driver path: %v", err)
	}

	args := append([]string{script}, urls...)
	cmd := exec.Command("python3", args...)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+bbLib)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bb.fetch2 driver failed: %v\n%s", err, out)
	}

	t.Logf("bb.fetch2 driver output:\n%s", out)

	return string(out)
}

// seed writes the control-plane rows the backend needs: a user, the org/project,
// their memberships, an enabled open-read sstate backend, and a write-scoped API
// key. It returns the plaintext key. Enum values are inlined as SQL literals so no
// enum-type encode plan is needed; the rest are bound parameters.
func seed(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	ctx := t.Context()

	key, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var userID, orgID, projectID pgtype.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO users (issuer, subject, email)
		 VALUES ('conformance', 'conf-user', 'conformance@bakery.local') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ($1, 'Acme') RETURNING id`, confOrg,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	// org_memberships.role is a GENERATED column (greatest(oidc_role, local_role));
	// write the source, not the result. oidc_role is the claim-derived half, which is
	// what a real login would set.
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_memberships (user_id, org_id, oidc_role, oidc_group)
		 VALUES ($1, $2, 'owner', 'conformance')`, userID, orgID,
	); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, 'Widget') RETURNING id`,
		orgID, confProject,
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (user_id, project_id, org_id, role)
		 VALUES ($1, $2, $3, 'writer')`, userID, projectID, orgID,
	); err != nil {
		t.Fatalf("seed project membership: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO cache_backends (project_id, kind, enabled, read_auth_required)
		 VALUES ($1, 'sstate', true, false)`, projectID,
	); err != nil {
		t.Fatalf("seed sstate backend: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (user_id, project_id, name, token_sha256, token_prefix, scope)
		 VALUES ($1, $2, 'conformance', $3, $4, 'write')`,
		userID, projectID, key.Hash, key.Prefix,
	); err != nil {
		t.Fatalf("seed api key: %v", err)
	}

	return key.Token
}

// cacheAuth widens *auth.Service to httpblob.Authenticator, exactly as the
// unexported server.cacheAuth does: auth.Principal satisfies httpblob.Principal
// structurally, so this is a pure type-widening delegate.
type cacheAuth struct{ svc *auth.Service }

func (a cacheAuth) AuthenticateCache(ctx context.Context, r *http.Request) (httpblob.Principal, error) {
	return a.svc.AuthenticateCache(ctx, r)
}

// ---------------------------------------------------------------------------
// Request recorder + gating helpers.
// ---------------------------------------------------------------------------

// recorder logs every request's method and path at the socket, so an assertion can
// prove what the real client sent -- e.g. that a 404 produced zero GETs.
type recorder struct {
	mu   sync.Mutex
	reqs []string // "METHOD path"
}

// record wraps h so every request is logged before it reaches the real handler.
func record(rec *recorder, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()

		h.ServeHTTP(w, r)
	})
}

// count reports how many logged requests used method and had token in their path.
// Matching on a colon-free token sidesteps whether the client sent the sstate colon
// literal or percent-encoded.
func (rec *recorder) count(method, token string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	n := 0
	for _, s := range rec.reqs {
		m, path, _ := strings.Cut(s, " ")
		if m == method && strings.Contains(path, token) {
			n++
		}
	}

	return n
}

func (rec *recorder) dump() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	return strings.Join(rec.reqs, "\n")
}

// resultFor extracts the driver's "RESULT <url> <status>" verdict for one url.
func resultFor(out, url string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "RESULT "+url+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "RESULT "+url+" "))
		}
	}

	return ""
}

// requireBitbake resolves the bitbake checkout's lib/ from BB_LIB and skips loudly
// if it is absent or does not import. This is the gate that lets the suite skip on a
// laptop without the client -- and `just conformance` turns that skip into a job
// failure in CI, where the client is provided.
func requireBitbake(t *testing.T) string {
	t.Helper()

	lib := strings.TrimSpace(os.Getenv("BB_LIB"))
	if lib == "" {
		t.Skip(skipMsg("BB_LIB is unset -- point it at a bitbake checkout's lib/ directory " +
			"(git clone --depth 1 --branch 2.8.0 https://git.openembedded.org/bitbake; " +
			"BB_LIB=$PWD/bitbake/lib). `just conformance` does this for you."))
	}

	if _, err := os.Stat(filepath.Join(lib, "bb", "fetch2")); err != nil {
		t.Skip(skipMsg(fmt.Sprintf("BB_LIB=%q does not contain bb/fetch2: %v", lib, err)))
	}

	return lib
}

// requireBinary skips loudly if an external binary the suite drives is missing.
func requireBinary(t *testing.T, name string) {
	t.Helper()

	if _, err := exec.LookPath(name); err != nil {
		t.Skip(skipMsg(fmt.Sprintf("the %q binary is not on PATH: %v", name, err)))
	}
}

func skipMsg(reason string) string {
	return "\n" + strings.Repeat("=", 80) + "\n" +
		"SKIPPING BITBAKE CONFORMANCE -- the real client is not available.\n\n" +
		"  reason: " + reason + "\n\n" +
		"  This suite drives the ACTUAL bitbake fetcher and the real wget binary; it\n" +
		"  needs python3, wget, a Postgres (docker or TEST_DB_URL), and a bitbake\n" +
		"  checkout on BB_LIB. Run it with `just conformance`, which fails on a skip.\n" +
		"\n  This proof did not run.\n" +
		strings.Repeat("=", 80)
}
