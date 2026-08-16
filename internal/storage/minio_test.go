package storage

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The minio harness. Modeled 1:1 on internal/db/dbtest/docker.go, and for the
// same reason: the S3 driver's contract is S3's SEMANTICS -- read-after-write,
// a DELETE of an absent key succeeding, a server-side copy, HeadObject's bodiless
// 404 -- and none of that is provable against a hand-written fake. A fake would
// pass every test in this file while the real driver failed on the first PUT.
//
// Resolution order, mirroring dbtest:
//
//  1. TEST_S3_ENDPOINT is set -- use that server (CI's own minio, or a
//     long-lived local container). Nothing is started or stopped.
//  2. otherwise -- `docker run` the pinned minio on a daemon-assigned port and
//     kill it when the package's tests finish.
//
// With neither, the S3 tests Skip with instructions. `just storage-conformance`
// then FAILS on that skip: a skipped S3 suite means not one assertion ran
// against real S3 semantics, which is exactly the trap every other conformance
// recipe documents.
const (
	// Pinned; a floating tag would make failures irreproducible.
	minioImage = "minio/minio:RELEASE.2025-04-22T22-12-26Z"

	// Tags every container we start so a crashed run is reaped by the next one.
	// The value is the PID of the `go test` process.
	minioOwnerLabel = "bakery.storagetest.owner"

	// minio refuses a root user under 3 characters or a password under 8.
	minioAccessKey = "bakerytest"
	minioSecretKey = "bakerytestsecret" //nolint:gosec // a throwaway credential for a container that dies with the test
	minioRegion    = "us-east-1"

	dockerProbeTimeout  = 15 * time.Second
	dockerCmdTimeout    = 60 * time.Second
	minioStartupTimeout = 90 * time.Second
)

// The escape hatch, mirroring dbtest's TEST_DB_URL. Set the endpoint and the
// credentials and nothing is started or stopped.
const (
	endpointEnv = "TEST_S3_ENDPOINT"
	accessEnv   = "TEST_S3_ACCESS_KEY"
	secretEnv   = "TEST_S3_SECRET_KEY"
	regionEnv   = "TEST_S3_REGION"
	bucketEnv   = "TEST_S3_BUCKET"
)

// errNoMinio means "no S3-compatible server, and that is not the test's fault"
// -- the signal to Skip. Every other error is a real failure.
var errNoMinio = errors.New("no s3-compatible server available")

// minioServer is the process-wide server every S3 test in this package shares.
// One BUCKET per process; isolation between tests is a unique key PREFIX, which
// also means every test exercises the S3Prefix config path rather than leaving
// it dead.
type minioServer struct {
	endpoint string
	region   string
	bucket   string
	ctr      *minioContainer // non-nil only when we started it ourselves
}

var (
	minioOnce sync.Once
	minioSrv  *minioServer
	minioErr  error
)

// TestMain installs the package-wide container lifecycle. TestMain is the only
// hook Go gives us that runs after the last test in a package, so it is the only
// correct place to stop a container shared by all of them.
func TestMain(m *testing.M) {
	code := m.Run()

	if minioSrv != nil && minioSrv.ctr != nil {
		minioSrv.ctr.remove()
	}

	os.Exit(code)
}

// ensureMinio resolves the server exactly once, and Skips (never fails) when
// there is nothing to resolve.
func ensureMinio(t *testing.T) *minioServer {
	t.Helper()

	minioOnce.Do(func() { minioSrv, minioErr = setupMinio() })

	if minioErr != nil {
		if errors.Is(minioErr, errNoMinio) {
			t.Skip(minioSkipMessage(minioErr))
		}

		t.Fatalf("minio harness: %v", minioErr)
	}

	return minioSrv
}

func setupMinio() (*minioServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), minioStartupTimeout)
	defer cancel()

	srv := &minioServer{endpoint: "", region: minioRegion, bucket: "", ctr: nil}

	access, secret := minioAccessKey, minioSecretKey

	switch endpoint := strings.TrimSpace(os.Getenv(endpointEnv)); {
	case endpoint != "":
		srv.endpoint = endpoint

		if v := strings.TrimSpace(os.Getenv(accessEnv)); v != "" {
			access = v
		}

		if v := strings.TrimSpace(os.Getenv(secretEnv)); v != "" {
			secret = v
		}

		if v := strings.TrimSpace(os.Getenv(regionEnv)); v != "" {
			srv.region = v
		}

		srv.bucket = strings.TrimSpace(os.Getenv(bucketEnv))

	default:
		if err := dockerAvailable(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", errNoMinio, err)
		}

		ctr, err := startMinio(ctx)
		if err != nil {
			return nil, err
		}

		srv.ctr = ctr
		srv.endpoint = ctr.endpoint
	}

	// THE CREDENTIALS GO IN THE ENVIRONMENT ON PURPOSE. S3Config carries no
	// credential fields -- production resolves them through the standard AWS
	// chain, and a harness that injected a static provider would leave that chain
	// completely untested. This runs before any test constructs a store (the
	// sync.Once above is the barrier), and this test binary is a process of its
	// own.
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID":     access,
		"AWS_SECRET_ACCESS_KEY": secret,
		"AWS_REGION":            srv.region,
	} {
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
	}

	if srv.bucket == "" {
		srv.bucket = "bk-storage-" + randSuffix()

		if err := createBucket(ctx, srv); err != nil {
			if srv.ctr != nil {
				srv.ctr.remove()
			}

			return nil, err
		}
	}

	return srv, nil
}

// createBucket makes the one bucket the package's tests share.
func createBucket(ctx context.Context, srv *minioServer) error {
	client, err := srv.client(ctx)
	if err != nil {
		return err
	}

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(srv.bucket)}); err != nil {
		return fmt.Errorf("create bucket %s: %w", srv.bucket, err)
	}

	return nil
}

// client is a raw SDK client on the harness's server, for the setup and
// assertions that reach past the Store interface (creating the bucket, proving
// a staging key is gone).
func (s *minioServer) client(ctx context.Context) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(s.region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s.endpoint)
		o.UsePathStyle = true
	}), nil
}

// minioContainer is a running minio we started and are responsible for killing.
type minioContainer struct {
	name     string
	endpoint string
}

func dockerAvailable(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not on PATH: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()

	if out, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon not reachable: %w (%s)", err, out)
	}

	return nil
}

func startMinio(ctx context.Context) (*minioContainer, error) {
	reapOrphans(ctx)

	name := "bakery-storagetest-" + randSuffix()

	// -p 127.0.0.1::9000 lets the DAEMON pick and BIND the host port, then we
	// read it back. We deliberately do NOT bind :0 in Go and pass the number to
	// docker: closing our listener to hand the port over opens a window in which
	// any other process can take it. Docker never lets go of the socket, so there
	// is no window.
	args := []string{
		"run", "--detach",
		"--name", name,
		"--label", minioOwnerLabel + "=" + strconv.Itoa(os.Getpid()),
		"--publish", "127.0.0.1::9000",
		"--env", "MINIO_ROOT_USER=" + minioAccessKey,
		"--env", "MINIO_ROOT_PASSWORD=" + minioSecretKey,
		// The whole object store in RAM: it dies with the test, and durability
		// is exactly what we are NOT testing here.
		"--tmpfs", "/data:rw,size=1g",
		minioImage,
		"server", "/data",
	}

	runCtx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	if out, err := docker(runCtx, args...); err != nil {
		return nil, fmt.Errorf("docker run %s: %w (%s)", minioImage, err, out)
	}

	c := &minioContainer{name: name, endpoint: ""}

	port, err := c.hostPort(ctx)
	if err != nil {
		c.remove()

		return nil, err
	}

	c.endpoint = "http://127.0.0.1:" + port

	if err := waitMinioReady(ctx, c); err != nil {
		logs := c.logs()
		c.remove()

		return nil, fmt.Errorf("%w\n--- container logs ---\n%s", err, logs)
	}

	return c, nil
}

func (c *minioContainer) hostPort(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	const tmpl = `{{ (index .NetworkSettings.Ports "9000/tcp" 0).HostPort }}`

	out, err := docker(ctx, "inspect", "--format", tmpl, c.name)
	if err != nil {
		return "", fmt.Errorf("read back published port: %w (%s)", err, out)
	}

	port := strings.TrimSpace(out)
	if port == "" || port == "<no value>" {
		return "", fmt.Errorf("daemon published no host port for 9000/tcp on %s", c.name)
	}

	return port, nil
}

func (c *minioContainer) running(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	out, err := docker(ctx, "inspect", "--format", "{{.State.Running}}", c.name)

	return err == nil && strings.TrimSpace(out) == "true"
}

func (c *minioContainer) logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	out, _ := docker(ctx, "logs", "--tail", "50", c.name)

	return out
}

// remove is idempotent and never returns an error: it runs from cleanup paths
// where there is nothing useful left to do with one.
func (c *minioContainer) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	_, _ = docker(ctx, "rm", "--force", "--volumes", c.name)
}

// waitMinioReady polls minio's own liveness endpoint, and fails FAST with logs
// if the container dies rather than burning the whole timeout.
//
// A TCP dial is NOT readiness: `docker run -p` binds the host port through
// docker-proxy the instant the container is created, so a dial succeeds long
// before minio is listening -- and then the connection resets.
func waitMinioReady(ctx context.Context, c *minioContainer) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(minioStartupTimeout)
	backoff := 25 * time.Millisecond

	var last error

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/minio/health/live", nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return nil
			}

			err = fmt.Errorf("health returned %s", resp.Status)
		}

		last = err

		if !c.running(ctx) {
			return fmt.Errorf("container %s exited during startup (last attempt: %w)", c.name, last)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s: %w", c.name, ctx.Err())
		case <-time.After(backoff):
		}

		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}

	return fmt.Errorf("container %s never became ready (last attempt: %w)", c.name, last)
}

// reapOrphans removes containers left behind by a `go test` process that was
// killed hard enough to skip its cleanup (SIGKILL, IDE stop button, CI cancel).
// TestMain covers panics and failures; this covers the case where no Go code got
// to run at all.
func reapOrphans(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	// No --quiet here: it overrides --format and prints bare IDs, which would
	// make the parse below silently skip every container.
	out, err := docker(ctx, "ps", "--all",
		"--filter", "label="+minioOwnerLabel,
		"--format", "{{.Names}}\t{{.Label \""+minioOwnerLabel+"\"}}")
	if err != nil {
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, pidStr, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name == "" {
			continue
		}

		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == os.Getpid() || processAlive(pid) {
			continue
		}

		_, _ = docker(ctx, "rm", "--force", "--volumes", name)
	}
}

// processAlive reports whether pid still names a live process. Signal 0 is the
// standard existence probe: it performs error checking but sends nothing.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return p.Signal(syscall.Signal(0)) == nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}

	return string(out), nil
}

func randSuffix() string {
	return fmt.Sprintf("%012x", rand.Uint64()&0xffffffffffff)
}

func minioSkipMessage(err error) string {
	return fmt.Sprintf(`
================================================================================
SKIPPING S3 STORAGE TESTS -- no S3-compatible server available.

  reason: %v

  Fix by doing ONE of these:

  1. Start Docker. The harness will run %s itself, on a free
     port, and tear it down afterwards. Nothing else to configure.

  2. Point %s at an S3-compatible server you can create buckets in:

       export %s='http://127.0.0.1:9000'
       export %s='...'
       export %s='...'

  These tests did not run. They did not pass. `+"`just storage-conformance`"+` fails
  on this skip for exactly that reason.
================================================================================`,
		err, minioImage, endpointEnv, endpointEnv, accessEnv, secretEnv)
}
