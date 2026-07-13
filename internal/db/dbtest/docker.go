package dbtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// image is pinned; a floating tag would make test failures irreproducible.
	image = "postgres:18-alpine"

	// ownerLabel tags every container we start so a crashed run can be reaped
	// by the next one. The value is the PID of the `go test` process.
	ownerLabel = "bakery.dbtest.owner"

	dockerProbeTimeout = 15 * time.Second
	dockerCmdTimeout   = 60 * time.Second
	startupTimeout     = 90 * time.Second
)

// container is a running postgres we started and are responsible for killing.
type container struct {
	name string
	dsn  string
}

// dockerAvailable reports whether a usable Docker daemon is reachable.
// `docker` being on PATH is not enough -- the daemon has to answer.
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

// startContainer runs postgres and blocks until it answers a real query.
func startContainer(ctx context.Context) (*container, error) {
	reapOrphans(ctx)

	name := "bakery-dbtest-" + randSuffix()

	// -p 127.0.0.1::5432 lets the DAEMON pick and BIND the host port, then we
	// read it back. We deliberately do NOT bind :0 in Go and pass the number to
	// docker: closing our listener to hand the port over opens a window in
	// which any other process can take it. Docker never lets go of the socket,
	// so there is no window.
	args := []string{
		"run", "--detach",
		"--name", name,
		"--label", ownerLabel + "=" + strconv.Itoa(os.Getpid()),
		"--publish", "127.0.0.1::5432",
		"--env", "POSTGRES_USER=postgres",
		"--env", "POSTGRES_PASSWORD=postgres",
		"--env", "POSTGRES_DB=postgres",
		// The image declares VOLUME /var/lib/postgresql and sets
		// PGDATA=/var/lib/postgresql/18/docker. Mounting a tmpfs at the VOLUME
		// path puts the whole cluster in RAM. Note this is NOT
		// /var/lib/postgresql/data -- that path is a postgres:<=17 habit and
		// tmpfs-ing it here would silently do nothing.
		"--tmpfs", "/var/lib/postgresql:rw,size=1g",
		image,
		// Durability is pointless for a cluster that dies with the test.
		"-c", "fsync=off",
		"-c", "full_page_writes=off",
		"-c", "synchronous_commit=off",
		// Each test holds its own pool against its own database; the default
		// 100 runs out fast under t.Parallel().
		"-c", "max_connections=500",
	}

	runCtx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	if out, err := docker(runCtx, args...); err != nil {
		return nil, fmt.Errorf("docker run %s: %w (%s)", image, err, out)
	}

	c := &container{name: name, dsn: ""}

	port, err := c.hostPort(ctx)
	if err != nil {
		c.remove()

		return nil, err
	}

	c.dsn = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%s/postgres?sslmode=disable", port)

	if err := waitReady(ctx, c); err != nil {
		logs := c.logs()
		c.remove()

		return nil, fmt.Errorf("%w\n--- container logs ---\n%s", err, logs)
	}

	return c, nil
}

// hostPort reads back the ephemeral port the daemon bound for us.
func (c *container) hostPort(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	const tmpl = `{{ (index .NetworkSettings.Ports "5432/tcp" 0).HostPort }}`

	out, err := docker(ctx, "inspect", "--format", tmpl, c.name)
	if err != nil {
		return "", fmt.Errorf("read back published port: %w (%s)", err, out)
	}

	port := strings.TrimSpace(out)
	if port == "" || port == "<no value>" {
		return "", fmt.Errorf("daemon published no host port for 5432/tcp on %s", c.name)
	}

	return port, nil
}

func (c *container) running(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	out, err := docker(ctx, "inspect", "--format", "{{.State.Running}}", c.name)

	return err == nil && strings.TrimSpace(out) == "true"
}

func (c *container) logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	out, _ := docker(ctx, "logs", "--tail", "50", c.name)

	return out
}

// remove is idempotent and never returns an error: it runs from cleanup paths
// where there is nothing useful left to do with one.
func (c *container) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	_, _ = docker(ctx, "rm", "--force", "--volumes", c.name)
}

// reapOrphans removes containers left behind by a `go test` process that was
// killed hard enough to skip its cleanup (SIGKILL, IDE stop button, CI
// cancel). t.Cleanup and TestMain cover panics and failures; this covers the
// case where no Go code got to run at all.
func reapOrphans(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	// No --quiet here: it overrides --format and prints bare IDs, which would
	// make the parse below silently skip every container.
	out, err := docker(ctx, "ps", "--all",
		"--filter", "label="+ownerLabel,
		"--format", "{{.Names}}\t{{.Label \""+ownerLabel+"\"}}")
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
