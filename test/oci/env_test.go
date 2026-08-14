package ociconf

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

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/cache/oci"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/server"
	"github.com/jsmith212/bakery/internal/storage"
)

// The tenants this suite drives. SeedDevLogin creates dev-org/playground and makes the
// dev user their owner, so the harness needs no hand-written INSERT; the other two
// projects are created through the real control-plane API.
//
// THE THREE DIFFER ONLY IN BACKEND CONFIG, and each difference is load-bearing:
//
//	projMain    read_auth_required=TRUE, tag_ttl=1h. The private-mirror shape, and the
//	            only one that makes a client run the Bearer token dance -- an open
//	            backend never 401s, so containerd would never authenticate and the
//	            first (cache-warming) pull would take the anonymous-miss 404.
//	projOpen    read_auth_required=false, tag_ttl=1h. The open-mirror shape, where the
//	            anonymous-miss and unrecognized-credential rules are observable.
//	projSWR     read_auth_required=TRUE, tag_ttl=2s. Short enough to go stale inside a
//	            test, long enough that the "freshness was restored" window is not a race.
//
// The route cache holds a resolved route (config included) for 30s, so a test may NOT
// patch a backend's config and expect it to take effect -- hence three projects rather
// than three PATCHes.
const (
	envOrg   = auth.DevOrgSlug
	projMain = auth.DevProjectSlug
	projOpen = "open-mirror"
	projSWR  = "swr"
)

// swrTagTTL is projSWR's tag_ttl. It is duplicated as a Go duration in swr_test.go's
// waits; keep the two in step.
const swrTagTTL = "2s"

// env is a booted Bakery M5 tree in front of a FAKE UPSTREAM REGISTRY.
//
// Nothing here is a lookalike. The HTTP tree is the real server.NewHandler -- the same
// function `bakery serve` runs, middleware and the /cache and /v2 catch-alls included --
// carrying the real oci.Backend over the real blob.Service against a real, ephemeral
// Postgres. The upstream client is the real oci.NewRegistry (go-containerregistry's
// Puller, the same code path that talks to Docker Hub in production); it just happens to
// be pointed at an httptest registry on loopback, which is what makes the gate hermetic.
//
// The org, the projects, the oci backend rows and the API keys are all created by
// driving the production HTTP API, exactly as a human would in the console.
type env struct {
	// httpBase is the public listener's URL: http://127.0.0.1:PORT. It is known BEFORE
	// the handler is built (see newEnv) because oci.Config.ExternalURL needs it: the
	// Docker Bearer realm must be an absolute URL, and a realm the clients cannot
	// resolve is the one M5 bug that reproduces only in a deployment.
	httpBase string

	// up is the fake upstream registry. Its request log is the anti-bypass assertion:
	// every client in the ecosystem silently falls back to the real registry on any
	// mirror failure, so "the pull succeeded" proves nothing on its own. "The pull
	// succeeded AND the upstream saw zero requests" proves Bakery served it.
	up *fakeRegistry

	// rec wraps the public handler and records every request a client makes, method,
	// path and query alike -- which is what lets a test assert containerd sent ?ns= and
	// took the HEAD fast path, against the CLIENT rather than against a doc.
	rec *recorder

	// keys holds one write-scoped `bkry_` token per project. It is ONE OPAQUE TOKEN,
	// not an id:secret pair, so it goes verbatim into a Basic password, a Basic
	// username, a Bearer header or containerd's OAuth2 refresh_token field alike.
	keys map[string]string
}

// newEnv boots the server, starts the fake upstream, and seeds it.
//
// It must NOT be called before the require* guards in the binary-driven tests:
// dbtest.New spawns a Postgres (or clones a template on TEST_DB_URL), and `just race`/
// `just coverage` glob ./... -- so on a runner with no skopeo this package is compiled
// and run, and the binary-guarded tests have to cost nothing when they cannot prove
// anything. The library-driven tests need no external binary and call newEnv directly.
func newEnv(t *testing.T) *env {
	t.Helper()

	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The upstream first: its host goes into every backend's config, so it has to exist
	// before the control-plane calls at the bottom of this function.
	up := newFakeRegistry(t)

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

	// Bind the public listener BEFORE building the handler. oci.Config.ExternalURL has
	// to be an absolute URL that clients can actually reach, and it is baked into the
	// Bearer realm every ping and every 401 advertises -- so the address has to be known
	// at construction time, not at Start() time. httptest.NewUnstartedServer allocates
	// its own listener, which we swap for this one.
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind public listener: %v", err)
	}

	base := "http://" + ln.Addr().String()

	// The REAL upstream client: go-containerregistry's shared Puller behind Bakery's
	// own keychain, exactly as boot.go builds it. No credentials are configured, so the
	// fake upstream is fetched anonymously -- which is what a public registry is.
	ociUp, err := oci.NewRegistry(oci.Config{ExternalURL: base}, m)
	if err != nil {
		t.Fatalf("oci.NewRegistry: %v", err)
	}

	backends := []cache.Backend{
		oci.New(deps, httpblob.NewCachedResolver(store, log), ociAuth{svc: authSvc}, ociUp,
			oci.Config{ExternalURL: base}),
	}

	rec := &recorder{next: server.NewHandler(server.Config{
		API:           apiSrv.Handler(),
		CacheBackends: backends,
		Headless:      true, // no SPA: there is no embedded dist in a test binary.
		Pool:          pool,
	})}

	srv := httptest.NewUnstartedServer(rec)

	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the throwaway httptest listener: %v", err)
	}

	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	e := &env{httpBase: base, up: up, rec: rec, keys: map[string]string{}}

	c := newAPIClient(t, base)
	c.devLogin(t)

	for _, p := range []struct {
		slug     string
		create   bool
		readAuth bool
		ttl      string
	}{
		{projMain, false, true, "1h"},
		{projOpen, true, false, "1h"},
		{projSWR, true, true, swrTagTTL},
	} {
		if p.create {
			c.createProject(t, p.slug)
		}

		c.createOCIBackend(t, p.slug, p.readAuth, up.host, p.ttl)
		e.keys[p.slug] = c.createKey(t, p.slug)
	}

	// The control-plane calls above all hit /api/v1/... and are recorded too. Drop them
	// so a test's recorder assertions see only the registry client's own traffic.
	rec.reset()

	return e
}

// host is the public listener's authority -- "127.0.0.1:PORT" -- which is what a
// registry client uses as the registry name.
//
// 127.0.0.1 is not incidental: go-containerregistry's name parser resolves the loopback
// address (and only the loopback address, localhost and RFC1918) to the http scheme, so
// crane and Bakery's own upstream client both reach these servers in cleartext with no
// insecure-registry flag anywhere.
func (e *env) host() string { return strings.TrimPrefix(e.httpBase, "http://") }

// key is the write-scoped token minted for one project.
func (e *env) key(project string) string { return e.keys[project] }

// mirrorPrefix is what an operator writes in a hosts.toml `server`/`[host]` entry or a
// daemon.json registry-mirror: the tenant's mount, with NO /v2 on the end. Both clients
// add that themselves -- containerd's config loader appends /v2 to any host that does
// not already end in it, and Docker Engine's URL builder merges /v2/... onto the
// mirror's base path -- which is why it must not be baked in here.
func (e *env) mirrorPrefix(project string) string {
	return "/cache/" + envOrg + "/" + project + "/docker"
}

// containerdPath is the mirror path AFTER the client has appended /v2: the prefix every
// request on the first route family actually carries.
func (e *env) containerdPath(project string) string {
	return e.mirrorPrefix(project) + "/v2"
}

// family1 is a full URL on the containerd / Docker Engine route family.
func (e *env) family1(project, rest string) string {
	return e.httpBase + e.containerdPath(project) + "/" + rest
}

// family2 is a full URL on the BuildKit / podman route family, where the tenant prefix
// lands in the repository position AFTER /v2 rather than before it.
func (e *env) family2(project, rest string) string {
	return e.httpBase + "/v2/" + envOrg + "/" + project + "/" + rest
}

// repo2 is the repository name a reference-rewriting client (podman, skopeo, crane)
// uses against the second route family: the tenant prefix is simply the first two path
// components of the repository.
func (e *env) repo2(project, name string) string {
	return envOrg + "/" + project + "/" + name
}

// ---------------------------------------------------------------------------
// The widening adapter boot.go declares, replicated here so the harness wires the same
// auth surface production does.
// ---------------------------------------------------------------------------

// ociAuth widens *auth.Service to oci.Authenticator. It takes a bare TOKEN, not an
// *http.Request, and that asymmetry with httpblob's Authenticator is a security
// property: Docker Engine forwards the user's real Docker Hub credentials to its mirror
// on every pull, and the shape gate inside the oci package means those never reach a
// database probe, a metric or a log line.
type ociAuth struct{ svc *auth.Service }

func (a ociAuth) AuthenticateToken(ctx context.Context, token string) (oci.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// ---------------------------------------------------------------------------
// The recorder: the network truth every client assertion rests on.
// ---------------------------------------------------------------------------

type recordedReq struct {
	method string
	path   string
	query  string
	status int
}

// recorder wraps the public handler and captures method, path, RAW QUERY and status.
//
// The query matters here in a way it did not for M4: containerd appends ?ns=<upstream>
// to every mirror request, and a mirror that silently ignores it serves one tenant's
// docker.io content for another registry's images. The recorder is how a test asserts
// the client really sent it.
type recorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []recordedReq
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	rec.next.ServeHTTP(sw, r)

	rec.mu.Lock()
	rec.reqs = append(rec.reqs, recordedReq{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, status: sw.statusCode(),
	})
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

// count returns how many recorded requests match a predicate.
func (rec *recorder) count(match func(recordedReq) bool) int {
	n := 0

	for _, r := range rec.snapshot() {
		if match(r) {
			n++
		}
	}

	return n
}

// statusWriter captures the status code without disturbing the response. It
// deliberately does not forward ReadFrom, so the sendfile fast path is bypassed on blob
// reads -- correctness is unaffected and a conformance gate does not need zero-copy.
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

func (c *apiClient) createProject(t *testing.T, project string) {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects", api.Prefix, envOrg)
	body := fmt.Sprintf(`{"slug":%q,"name":%q}`, project, project)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create project %s: status = %d, body %s", project, status, raw)
	}
}

// createOCIBackend writes the oci row, with the upstream ALLOWLIST that is the SSRF
// gate: ?ns= is attacker-controlled input naming a host Bakery will dial, so anything
// not on this list must 404 without a connection being made.
func (c *apiClient) createOCIBackend(t *testing.T, project string, readAuth bool, upstream, ttl string) {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/backends", api.Prefix, envOrg, project)
	config := fmt.Sprintf(`{"default_upstream":%q,"upstreams":[%q],"tag_ttl":%q}`, upstream, upstream, ttl)
	body := fmt.Sprintf(`{"kind":"oci","enabled":true,"read_auth_required":%t,"config":%s}`,
		readAuth, config)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create oci backend for %s: status = %d, body %s", project, status, raw)
	}
}

// createKey mints a key through the real minting path and returns the plaintext EXACTLY
// once -- there is no second way to read it, by design.
func (c *apiClient) createKey(t *testing.T, project string) string {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/keys", api.Prefix, envOrg, project)
	body := fmt.Sprintf(`{"name":"oci-conformance-%s","scope":"write"}`, project)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create api key for %s: status = %d, body %s", project, status, raw)
	}

	var created struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}

	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode the created key: %v (body %s)", err, raw)
	}

	if !strings.HasPrefix(created.Token, auth.TokenPrefix) || created.Scope != "write" {
		t.Fatalf("minted key = %q scope %q, want a %s token with scope write",
			created.Token, created.Scope, auth.TokenPrefix)
	}

	return created.Token
}

// ---------------------------------------------------------------------------
// The guards.
// ---------------------------------------------------------------------------

// requireBinary skips loudly -- and CHEAPLY, before dbtest.New spawns a Postgres -- when
// the named client binary is not installed. `just oci-conformance` installs skopeo and
// turns a skip into a job failure; `just race`/`just coverage` glob ./... and do not, so
// on a laptop without the client this returns before anything expensive.
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
		"SKIPPING OCI CONFORMANCE -- a real client binary is not available.\n\n" +
		"  reason: the \"" + name + "\" binary is not on PATH: " + err.Error() + "\n\n" +
		"  This suite drives the real skopeo binary (the containers/image client, the only\n" +
		"  one that hard-requires the bare-root GET /v2/ ping) against the real OCI mount.\n" +
		"  It needs skopeo plus a Postgres (docker or TEST_DB_URL). Run it with\n" +
		"  `just oci-conformance`, which installs the client and fails on a skip.\n" +
		"\n  This proof did not run.\n" +
		strings.Repeat("=", 80)
}
