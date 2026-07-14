package hashservconf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/hashserv"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/server"
	"github.com/jsmith212/bakery/internal/storage"
)

// env is a booted Bakery: the REAL public routing tree (server.NewHandler -- the same
// function `bakery serve` calls, middleware included), the REAL control-plane API, and
// the REAL hashserv backend over a real auth.Service, a real route resolver and a real,
// ephemeral Postgres.
//
// Nothing here is a lookalike. The org, the project, the hashserv backend row and the
// write-scoped API key are all created by driving the production HTTP API, exactly as a
// human would in the console -- so the key the bitbake client authenticates with was
// minted by the code path that mints customers' keys, and validated by the code path
// that validates them.
type env struct {
	server *httptest.Server

	// wsURL is what BB_TEST_HASHSERV and `bitbake-hashclient --address` are given:
	// ws://127.0.0.1:PORT/cache/{org}/{project}/hashserv. bitbake passes it VERBATIM to
	// websockets.connect (asyncrpc/client.py parse_address returns the whole string for a
	// ws:// address), which is what makes the multi-tenant path legal here.
	wsURL string

	// writeKey is a real `bkry_` token. It is ONE OPAQUE TOKEN, not an id:secret pair --
	// so it goes in the username field and the password field, both, and either one alone
	// would also authenticate (auth.AuthenticateCache reads the Basic password and falls
	// back to the username; hashserv's in-band `auth` RPC does the same with token/username).
	writeKey string
}

// The mount under test. SeedDevLogin creates dev-org/playground and makes the dev user
// their owner/admin, so the harness does not have to invent a control plane of its own.
const (
	envOrg     = auth.DevOrgSlug
	envProject = auth.DevProjectSlug
)

// newEnv boots the server and seeds the mount through the real API.
//
// It must NOT be called before the require* guards: dbtest.New spawns a Postgres (or
// clones a template on TEST_DB_URL), and `just race`/`just coverage` glob ./... -- so on
// a runner with no bitbake checkout this package is compiled and run, and it has to cost
// nothing when it cannot prove anything.
func newEnv(t *testing.T) *env {
	t.Helper()

	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	m := metrics.New()

	sessions := auth.NewSessionManager(auth.NewSessionStore(pool, log), false)

	authSvc, err := auth.New(auth.Deps{
		Store: store, Sessions: sessions, Provider: nil, Groups: nil,
		Metrics: m, Log: log, DevLogin: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// dev-login is the only credential-free way into the API, and it is why this harness
	// needs no hand-written INSERT: it creates dev@bakery.local, dev-org and
	// dev-org/playground, with the memberships a real login would have written.
	if err := authSvc.SeedDevLogin(ctx); err != nil {
		t.Fatalf("seed dev login: %v", err)
	}

	apiSrv, err := api.New(api.Config{
		Store: store, Auth: authSvc, Metrics: m, Log: log,
		AllowSelfServeOrgs: true, AllowLocalSiteAdmins: true,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	// hashserv is the ONE backend that does not route through blob.Service -- but
	// cache.Deps is shared, and Validate requires the blob service, so it is built here
	// exactly as server.Boot builds it.
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}

	blobs, err := blob.New(blob.Config{
		Reader:  store,
		Tx:      store,
		Storage: storage.NewInstrumented(local, m, metrics.DriverLocal),
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	deps := cache.Deps{Blobs: blobs, Metrics: m, Logger: log}
	if err := deps.Validate(); err != nil {
		t.Fatalf("cache.Deps.Validate: %v", err)
	}

	// Upstream chaining is left ON (the production default) and no backend configures an
	// upstream, so nothing is ever dialled. That is the case a build actually runs in, and
	// it proves the "no upstream configured" path costs nothing rather than assuming it.
	upstreams := hashserv.NewUpstreams(store, m, log, false)
	t.Cleanup(func() {
		if err := upstreams.Close(); err != nil {
			t.Errorf("close hashserv upstreams: %v", err)
		}
	})

	backend := hashserv.New(deps, httpblob.NewCachedResolver(store, log),
		hashservAuth{svc: authSvc}, store, upstreams)

	// server.NewHandler, not a hand-rolled mux: it is exported precisely so a test can
	// exercise the REAL routing tree -- middleware, /healthz, /api/v1 and the cache
	// backends -- over httptest without binding a port. The WebSocket upgrade therefore
	// goes through the same logging/metrics middleware production runs, which is the only
	// way this suite can prove that the middleware's responseRecorder still hijacks.
	srv := httptest.NewServer(server.NewHandler(server.Config{
		API:           apiSrv.Handler(),
		CacheBackends: []cache.Backend{backend},
		Headless:      true, // no SPA: there is no embedded dist in a test binary.
		Pool:          pool,
	}))
	t.Cleanup(srv.Close)

	e := &env{
		server: srv,
		wsURL:  "ws://" + srv.Listener.Addr().String() + "/cache/" + envOrg + "/" + envProject + "/hashserv",
	}

	c := newAPIClient(t, srv.URL)
	c.devLogin(t)

	// read_auth_required=false: the open-mirror case, and the one the clients need.
	// bitbake-hashclient's `stress` subcommand opens its reader connections with NO
	// credential at all (handle_stress calls hashserv.create_client(address) with no
	// username), so an anonymous read must work -- while every write still requires the
	// write-scoped key below. There is no WriteAuthRequired knob and there must not be.
	c.createBackend(t, `{"kind":"hashserv","enabled":true,"read_auth_required":false}`)

	e.writeKey = c.createKey(t, `{"name":"hashserv-conformance","scope":"write"}`)

	return e
}

// hashservAuth widens *auth.Service to hashserv.Authenticator, exactly as the unexported
// server.hashservAuth does. auth.Principal satisfies hashserv.Principal structurally, so
// this is a pure type-widening delegate with no logic to get wrong -- and it is the same
// AuthenticateToken probe (constant-time, zero-join, index-only) the HTTP arms run.
type hashservAuth struct{ svc *auth.Service }

func (a hashservAuth) AuthenticateToken(ctx context.Context, token string) (hashserv.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// ---------------------------------------------------------------------------
// The control-plane client: the console's flow, over HTTP.
// ---------------------------------------------------------------------------

type apiClient struct {
	base   string
	client *http.Client
}

func newAPIClient(t *testing.T, base string) *apiClient {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	return &apiClient{base: base, client: &http.Client{Jar: jar}}
}

func (c *apiClient) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	r, err := http.NewRequestWithContext(t.Context(), method, c.base+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}

	return resp.StatusCode, raw
}

func (c *apiClient) devLogin(t *testing.T) {
	t.Helper()

	status, body := c.do(t, http.MethodPost, api.Prefix+"/auth/dev-login", "")
	if status != http.StatusOK {
		t.Fatalf("dev-login: status = %d, body %s", status, body)
	}
}

func (c *apiClient) createBackend(t *testing.T, body string) {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/backends", api.Prefix, envOrg, envProject)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create hashserv backend: status = %d, body %s", status, raw)
	}
}

// createKey mints a key through the real minting path and returns the plaintext EXACTLY
// once -- there is no second way to read it, by design.
func (c *apiClient) createKey(t *testing.T, body string) string {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/keys", api.Prefix, envOrg, envProject)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create api key: status = %d, body %s", status, raw)
	}

	var created struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}

	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode the created key: %v (body %s)", err, raw)
	}

	if !strings.HasPrefix(created.Token, "bkry_") || created.Scope != "write" {
		t.Fatalf("minted key = %q scope %q, want a bkry_ token with scope write",
			created.Token, created.Scope)
	}

	return created.Token
}

// ---------------------------------------------------------------------------
// The guards. Every one of these runs BEFORE dbtest.New and before the server boots.
// ---------------------------------------------------------------------------

// bbEnv is the resolved bitbake environment: where its python lives, and what to run.
type bbEnv struct {
	// lib is BB_LIB -- the checkout's lib/, which carries both `bb` and `hashserv`.
	lib string

	// hashclient is <checkout>/bin/bitbake-hashclient, derived from lib/ the same way
	// upstream's own tests.py derives BIN_DIR (THIS_DIR.parent.parent / "bin").
	hashclient string

	// python is the interpreter that can `import websockets`. It is the SAME interpreter
	// for the unittest run and for bitbake-hashclient: the hashclient's shebang would pick
	// whatever python3 is first on PATH, which on a runner with actions/setup-python is not
	// necessarily the one the pinned websockets was installed for.
	python string

	// pythonPath is BB_LIB, plus whatever PYTHONPATH the caller exported (the justfile
	// points it at the pip --target directory holding the pinned websockets).
	pythonPath string
}

// requireBitbake resolves the bitbake checkout and the python that can drive it, skipping
// loudly -- and CHEAPLY -- when the client is not available.
//
// `just hashserv-conformance` provides all of it and turns a skip into a job failure. This
// package is also compiled and run by `just race` and `just coverage`, which glob ./... and
// do not: there, this returns before anything expensive has happened.
func requireBitbake(t *testing.T) bbEnv {
	t.Helper()

	lib := strings.TrimSpace(os.Getenv("BB_LIB"))
	if lib == "" {
		t.Skip(skipMsg("BB_LIB is unset -- point it at a bitbake checkout's lib/ directory. " +
			"`just hashserv-conformance` clones the pinned tag and does this for you."))
	}

	lib = filepath.Clean(lib)

	if _, err := os.Stat(filepath.Join(lib, "hashserv", "tests.py")); err != nil {
		t.Skip(skipMsg(fmt.Sprintf("BB_LIB=%q does not contain hashserv/tests.py: %v", lib, err)))
	}

	hashclient := filepath.Join(filepath.Dir(lib), "bin", "bitbake-hashclient")
	if _, err := os.Stat(hashclient); err != nil {
		t.Skip(skipMsg(fmt.Sprintf("no bitbake-hashclient at %q: %v", hashclient, err)))
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(skipMsg(fmt.Sprintf("the \"python3\" binary is not on PATH: %v", err)))
	}

	bb := bbEnv{lib: lib, hashclient: hashclient, python: python, pythonPath: lib}
	if extra := strings.TrimSpace(os.Getenv("PYTHONPATH")); extra != "" {
		bb.pythonPath = lib + string(os.PathListSeparator) + extra
	}

	requireWebsockets(t, bb)

	return bb
}

// requireWebsockets proves the interpreter can drive bitbake's client BEFORE a Postgres is
// spawned -- and, crucially, that it still has the LEGACY top-level websockets.connect API.
//
// bitbake 2.8 calls `websockets.connect(uri, ping_interval=None)` (asyncrpc/client.py
// connect_websocket). That API was deprecated and then REMOVED in websockets 14, so an
// unpinned `pip install websockets` produces an AttributeError inside every test and a red
// gate that is not a Bakery bug. A MISSING websockets is a skip (a laptop); a websockets
// that is present but too new is a FAILURE, because that is a broken pin and pretending
// otherwise would let the gate go quietly green having proven nothing.
func requireWebsockets(t *testing.T, bb bbEnv) {
	t.Helper()

	const probe = `import websockets, sys
sys.stdout.write(websockets.version.version + "\n")
sys.stdout.write("legacy\n" if callable(getattr(websockets, "connect", None)) else "no-legacy\n")`

	cmd := exec.CommandContext(t.Context(), bb.python, "-c", probe)
	cmd.Env = bb.env(nil)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skip(skipMsg(fmt.Sprintf("%q cannot import websockets: %v\n%s", bb.python, err, out)))
	}

	lines := strings.Fields(string(out))
	if len(lines) != 2 || lines[1] != "legacy" {
		t.Fatalf("websockets %q has no top-level connect(): bitbake 2.8 calls the LEGACY "+
			"websockets.connect API, which was removed in websockets 14. Pin it (see websockets_pin "+
			"in the justfile).\n%s", strings.Join(lines, " "), out)
	}

	t.Logf("driving bitbake with %s, websockets %s (legacy connect present)", bb.python, lines[0])
}

// env builds the environment for one python child: PYTHONPATH plus whatever the caller adds.
func (bb bbEnv) env(extra []string) []string {
	e := append(os.Environ(), "PYTHONPATH="+bb.pythonPath)

	return append(e, extra...)
}

func skipMsg(reason string) string {
	return "\n" + strings.Repeat("=", 80) + "\n" +
		"SKIPPING HASHSERV CONFORMANCE -- the real bitbake client is not available.\n\n" +
		"  reason: " + reason + "\n\n" +
		"  This suite drives bitbake's OWN hashserv test suite and the real\n" +
		"  bitbake-hashclient binary against the real hashserv backend. It needs python3\n" +
		"  with a PINNED websockets, a Postgres (docker or TEST_DB_URL), and a bitbake\n" +
		"  checkout on BB_LIB. Run it with `just hashserv-conformance`, which provides all\n" +
		"  three and fails on a skip.\n" +
		"\n  This proof did not run.\n" +
		strings.Repeat("=", 80)
}
