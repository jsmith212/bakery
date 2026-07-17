package bazelconf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/bazel"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/server"
	"github.com/jsmith212/bakery/internal/storage"
)

// grpcMaxMsgSize mirrors internal/server/boot.go: 4x the advertised
// max_batch_total_size_bytes (4 MiB). moon's worst-case BatchUpdateBlobs at B=4 MiB is
// 4,097,613 bytes -- inside grpc-go's 4 MiB default by only 2.3%, which is not a margin.
// The harness constructs the gRPC server with the SAME options boot does, so a message
// grpc-go would reject in production is rejected here too.
const grpcMaxMsgSize = 16 << 20

// The mount under test. SeedDevLogin creates dev-org/playground and makes the dev user
// their owner/admin, so the harness does not have to invent a control plane of its own.
const (
	envOrg     = auth.DevOrgSlug
	envProject = auth.DevProjectSlug
)

// env is a booted Bakery M4 tree: the REAL Bazel REAPI on a REAL, dedicated gRPC
// listener (the production topology -- REAPI is its own port, never the public mux), the
// REAL /ac + /cas + sccache HTTP mounts through server.NewHandler (middleware and the
// /cache catch-all included), the REAL control-plane API, and a real, ephemeral Postgres.
//
// Nothing here is a lookalike. The org, the project, the bazel backend row and the
// write-scoped API key are all created by driving the production HTTP API, exactly as a
// human would in the console -- so the key the REAPI/ccache/sccache clients authenticate
// with was minted by the code path that mints customers' keys, and validated by the code
// path that validates them. The gRPC server is built with grpc.NewServer + the same
// MaxRecv/MaxSend options and the same RegisterGRPC loop boot.go runs.
type env struct {
	// httpBase is the public HTTP listener's URL: http://127.0.0.1:PORT. ccache's
	// remote_storage and sccache's SCCACHE_WEBDAV_ENDPOINT are built from it.
	httpBase string

	// grpcAddr is the REAPI listener: 127.0.0.1:PORT, handed to the remote-apis-sdks
	// client as DialParams.Service with NoSecurity (cleartext prior-knowledge h2c).
	grpcAddr string

	// rec wraps the public handler and records every request the HTTP cache clients make,
	// so the ccache/sccache tests can prove -- against the CLIENT, not a doc -- that ccache
	// hit /ac and never /cas, and that sccache did PROPFIND-then-PUT.
	rec *recorder

	// writeKey is a real `bkry_` token. It is ONE OPAQUE TOKEN, not an id:secret pair, so
	// it goes verbatim into the gRPC "authorization: Bearer <token>" metadata, ccache's
	// |bearer-token= attribute and sccache's SCCACHE_WEBDAV_TOKEN alike.
	writeKey string
}

// newEnv boots the server and seeds the mount through the real API.
//
// It must NOT be called before the require* guards in the ccache/sccache tests:
// dbtest.New spawns a Postgres (or clones a template on TEST_DB_URL), and
// `just race`/`just coverage` glob ./... -- so on a runner with no ccache/sccache this
// package is compiled and run, and the binary-guarded tests have to cost nothing when
// they cannot prove anything. The remote-apis-sdks tests need no external binary and call
// newEnv directly (dbtest itself skips cheaply when it can reach neither docker nor
// TEST_DB_URL).
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

	routes := httpblob.NewCachedResolver(store, log)
	authn := cacheAuth{svc: authSvc}

	// The gRPC server, built EXACTLY as boot.go builds it: same MaxRecv/MaxSend, same
	// RegisterGRPC loop. bazel.New owns the /ac and /cas HTTP mounts internally and ALSO
	// implements cache.GRPCBackend; NewSccache is the sibling WebDAV backend on its own
	// route and namespace. These two are the whole M4 surface the three clients touch.
	grpcSrv := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpcMaxMsgSize),
		grpc.MaxSendMsgSize(grpcMaxMsgSize),
	)

	cacheBackends := []cache.Backend{
		bazel.New(deps, routes, authn, bazelAuth{svc: authSvc}),
		httpblob.NewSccache(deps, routes, authn),
	}

	for _, backend := range cacheBackends {
		gb, ok := backend.(cache.GRPCBackend)
		if !ok {
			continue
		}

		if err := gb.RegisterGRPC(grpcSrv); err != nil {
			t.Fatalf("register grpc backend %q: %v", backend.Kind(), err)
		}
	}

	// A REAL, dedicated gRPC listener on loopback -- the production topology, where REAPI
	// is its own port. grpc.Server.Serve(lis) builds grpc-go's real http2Server (not the
	// ServeHTTP transport whose Drain panics), so GracefulStop at cleanup drains cleanly.
	var lc net.ListenConfig

	grpcLn, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind grpc listener: %v", err)
	}

	go func() {
		// Serve returns nil once GracefulStop/Stop is called; anything else is a real
		// serve failure, and t.Errorf from a background goroutine is the honest report.
		if serveErr := grpcSrv.Serve(grpcLn); serveErr != nil {
			t.Errorf("grpc serve: %v", serveErr)
		}
	}()

	t.Cleanup(grpcSrv.GracefulStop)

	// The public HTTP tree, wrapped in a recorder. This is the REAL server.NewHandler --
	// the same function `bakery serve` runs, middleware and the /cache 404 catch-all and
	// all -- so the /ac, /cas and sccache routes go through exactly the production stack;
	// the recorder only observes method + path + status on the way through.
	rec := &recorder{next: server.NewHandler(server.Config{
		API:           apiSrv.Handler(),
		CacheBackends: cacheBackends,
		Headless:      true, // no SPA: there is no embedded dist in a test binary.
		Pool:          pool,
	})}

	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	e := &env{
		httpBase: srv.URL,
		grpcAddr: grpcLn.Addr().String(),
		rec:      rec,
	}

	c := newAPIClient(t, srv.URL)
	c.devLogin(t)

	// read_auth_required=false: the open-mirror case. Reads then need no credential, while
	// every WRITE still requires the write-scoped key below -- there is no WriteAuthRequired
	// knob and there must not be.
	c.createBackend(t, `{"kind":"bazel","enabled":true,"read_auth_required":false}`)

	e.writeKey = c.createKey(t, `{"name":"bazel-conformance","scope":"write"}`)

	// The control-plane calls above (dev-login, create backend, create key) all hit
	// /api/v1/... and are recorded too. Drop them so a test's recorder assertions see only
	// the cache client's own traffic.
	rec.reset()

	return e
}

// instanceName is the REAPI instance_name: "{org}/{project}", WITH the slash -- the exact
// shape ByteStream resource-name parsing must scan past (never split positionally on).
func (e *env) instanceName() string { return envOrg + "/" + envProject }

// ---------------------------------------------------------------------------
// cacheAuth / bazelAuth: the two widening adapters boot.go declares, replicated here so
// the harness wires the same auth surface production does.
// ---------------------------------------------------------------------------

// cacheAuth widens *auth.Service to httpblob.Authenticator (the /ac, /cas and sccache HTTP
// mounts). auth.Principal satisfies httpblob.Principal structurally, so this is a pure
// type-widening delegate with no logic to get wrong.
type cacheAuth struct{ svc *auth.Service }

func (a cacheAuth) AuthenticateCache(ctx context.Context, r *http.Request) (httpblob.Principal, error) {
	return a.svc.AuthenticateCache(ctx, r)
}

// bazelAuth widens *auth.Service to bazel.Authenticator (the gRPC surface). The REAPI
// credential arrives in gRPC "authorization" metadata, not an *http.Request, so the gRPC
// handlers need AuthenticateToken -- the same constant-time, zero-join, index-only key
// probe the Basic path runs.
type bazelAuth struct{ svc *auth.Service }

func (a bazelAuth) AuthenticateToken(ctx context.Context, token string) (bazel.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// ---------------------------------------------------------------------------
// The recorder: the network truth the ccache/sccache assertions rest on.
// ---------------------------------------------------------------------------

type recordedReq struct {
	method string
	path   string
	status int
}

// recorder wraps the public handler and captures every request's method, path and
// response status. It is the outermost handler, so it sees the URL exactly as the client
// sent it -- which is what lets a test assert "ccache hit /ac/<64hex> and NEVER /cas" and
// "sccache did PROPFIND then PUT", pinning the opacity claims to the real client.
type recorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []recordedReq
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	rec.next.ServeHTTP(sw, r)

	rec.mu.Lock()
	rec.reqs = append(rec.reqs, recordedReq{method: r.Method, path: r.URL.Path, status: sw.statusCode()})
	rec.mu.Unlock()
}

func (rec *recorder) reset() {
	rec.mu.Lock()
	rec.reqs = nil
	rec.mu.Unlock()
}

func (rec *recorder) snapshot() []recordedReq {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	out := make([]recordedReq, len(rec.reqs))
	copy(out, rec.reqs)

	return out
}

// statusWriter captures the status code without disturbing the response. It intentionally
// does not forward ReadFrom (so sendfile is bypassed on the /cas read path) -- correctness
// is unaffected, the bytes are copied through Write, and a conformance test does not need
// the zero-copy fast path.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}

	return sw.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer when it supports it, so a streamed /cas read is not
// stalled behind the recorder.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) statusCode() int {
	if sw.status == 0 {
		return http.StatusOK
	}

	return sw.status
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
		t.Fatalf("create bazel backend: status = %d, body %s", status, raw)
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
// The guards.
// ---------------------------------------------------------------------------

// requireBinary skips loudly -- and CHEAPLY, before dbtest.New spawns a Postgres -- when
// the named client binary is not installed. `just bazel-conformance` installs ccache and
// sccache and turns a skip into a job failure; `just race`/`just coverage` glob ./... and
// do not, so on a laptop without the client this returns before anything expensive.
func requireBinary(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skip(binSkipMsg(name, err))
	}

	return path
}

func binSkipMsg(name string, err error) string {
	return "\n" + strings.Repeat("=", 80) + "\n" +
		"SKIPPING BAZEL CONFORMANCE -- a real client binary is not available.\n\n" +
		"  reason: the \"" + name + "\" binary is not on PATH: " + err.Error() + "\n\n" +
		"  This suite drives the real ccache and sccache binaries against the real /ac and\n" +
		"  sccache mounts. It needs ccache, sccache and a C compiler, plus a Postgres\n" +
		"  (docker or TEST_DB_URL). Run it with `just bazel-conformance`, which installs the\n" +
		"  clients and fails on a skip.\n" +
		"\n  This proof did not run.\n" +
		strings.Repeat("=", 80)
}
