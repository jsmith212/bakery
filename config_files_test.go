package main

// These tests guard the build- and docs-layer invariants that let a fresh clone
// actually start: the CI pipeline must exercise the race detector, the documented
// compose stack must boot without crash-looping, the database port must not be
// exposed to the world, and the documented `just run` must invoke a real server
// verb. They read the repository's own config files, so they run with no database
// and no docker.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory to the directory holding
// go.mod. `go test .` runs with cwd at the module root already, but resolving it
// explicitly keeps the tests honest if that ever stops being true.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}

		dir = parent
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	return string(b)
}

// TestCI_RunsRaceDetector fails if the Build workflow stops invoking the race
// detector. TestLRU_ConcurrentAccessIsRaceFree has no assertions and
// TestRefcountRace_IncrefDecrefDelete's whole value is what -race reports, so a
// pipeline that never passes -race checks neither. `just check`, `just test-db`
// and `just coverage` all run without it; only a step that runs `just race` (or a
// bare `go test -race`) does.
func TestCI_RunsRaceDetector(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/build.yml")

	if !strings.Contains(ci, "just race") && !strings.Contains(ci, "go test -race") {
		t.Error("the Build workflow runs neither `just race` nor `go test -race`; " +
			"the concurrency tests are only race-checked when a developer remembers to type the command")
	}
}

// TestJustfile_RunInvokesServe fails if the documented `just run` recipe stops
// passing the `serve` verb. The binary is also the API client, so `go run .` with
// no verb is a Kong usage error, not a server -- which is exactly how the README
// quickstart used to dead-end.
func TestJustfile_RunInvokesServe(t *testing.T) {
	justfile := readRepoFile(t, "justfile")

	recipe, ok := justRecipeBody(justfile, "run")
	if !ok {
		t.Fatal("no `run` recipe found in the justfile")
	}

	if !strings.Contains(recipe, "go run . serve") {
		t.Errorf("the `run` recipe does not `go run . serve`; a bare `go run .` is a usage error, not a server.\nrecipe body:\n%s", recipe)
	}
}

// justRecipeBody returns the indented body lines of a just recipe named `name`.
// A recipe header is a line starting at column zero that begins with the name
// followed by args/deps and a colon; its body is the following lines that are
// indented (or blank). Comment lines above the header are ignored.
func justRecipeBody(justfile, name string) (string, bool) {
	lines := strings.Split(justfile, "\n")

	for i, line := range lines {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}

		head := line
		if !strings.HasPrefix(head, name) {
			continue
		}

		// Require a word boundary after the name so `run` does not match `runx`.
		rest := head[len(name):]
		if rest == "" || (rest[0] != ':' && rest[0] != ' ') {
			continue
		}

		var body []string

		for _, bl := range lines[i+1:] {
			if bl == "" || bl[0] == ' ' || bl[0] == '\t' {
				body = append(body, bl)

				continue
			}

			break
		}

		return strings.Join(body, "\n"), true
	}

	return "", false
}

// TestStackEnvTmpl_GroupMapDoesNotCrashLoopCompose fails if stack.env.tmpl ships an
// active GROUP_MAP_FILE while docker-compose.yaml does not mount that file. Boot
// hard-fails on a missing group-map file, and `restart: unless-stopped` turns that
// into an infinite crash loop, so the only documented way to bring the stack up
// (cp stack.env.tmpl stack.env; just start) must not point at a path the compose
// file leaves unmounted.
func TestStackEnvTmpl_GroupMapDoesNotCrashLoopCompose(t *testing.T) {
	groupMap := activeEnvValue(readRepoFile(t, "stack.env.tmpl"), "GROUP_MAP_FILE")
	if groupMap == "" {
		return // Unset/commented: boot skips the group map entirely. Safe.
	}

	compose := readRepoFile(t, "docker-compose.yaml")
	if !composeMountsContainerPath(compose, groupMap) {
		t.Errorf("stack.env.tmpl sets GROUP_MAP_FILE=%s but docker-compose.yaml does not mount that path; "+
			"a fresh `cp stack.env.tmpl stack.env && just start` crash-loops on the missing file. "+
			"Comment GROUP_MAP_FILE out, or uncomment the matching bind mount.", groupMap)
	}
}

// activeEnvValue returns the value of the last uncommented `KEY=value` assignment
// for key in a dotenv-style file, or "" if there is none (or it is commented out
// or empty).
func activeEnvValue(env, key string) string {
	var value string

	prefix := key + "="

	for _, line := range strings.Split(env, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, prefix) {
			value = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}

	return value
}

// composeMountsContainerPath reports whether the compose file has an uncommented
// volume entry mounting something to the given container path (the ":<path>" or
// ":<path>:ro" target of a `- host:container` bind mount).
func composeMountsContainerPath(compose, containerPath string) bool {
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		spec := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))

		fields := strings.Split(spec, ":")
		for i := 1; i < len(fields); i++ {
			if fields[i] == containerPath {
				return true
			}
		}
	}

	return false
}

// TestCompose_DBPortNotPublishedToAllInterfaces fails if any published port maps a
// database container port (5432) without binding loopback. A bare "5432:5432"
// publishes on 0.0.0.0, exposing Postgres -- every session, api_key hash and
// org/project row -- to any host that can reach the machine.
func TestCompose_DBPortNotPublishedToAllInterfaces(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yaml")

	for _, entry := range publishedPorts(compose) {
		fields := strings.Split(entry, ":")

		container := fields[len(fields)-1]
		if container != "5432" {
			continue
		}

		// A loopback-bound publish has an explicit host IP as the first field:
		// "127.0.0.1:5432:5432". Anything shorter binds every interface.
		if len(fields) < 3 || (fields[0] != "127.0.0.1" && fields[0] != "::1") {
			t.Errorf("docker-compose.yaml publishes the database port as %q, which binds 0.0.0.0; "+
				"bind it to 127.0.0.1 (or drop the ports stanza -- the stack reaches db over the compose network)", entry)
		}
	}
}

// publishedPorts extracts the quoted/unquoted entries of every uncommented `ports:`
// list item in a compose file, e.g. "8080:8080" and "127.0.0.1:5432:5432".
func publishedPorts(compose string) []string {
	var out []string

	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		item = strings.Trim(item, `"'`)

		// A port mapping is digits and colons (optionally a leading host IP). This
		// filters out volume entries and other list items that share the `- ` shape.
		if !looksLikePortMapping(item) {
			continue
		}

		out = append(out, item)
	}

	return out
}

func looksLikePortMapping(s string) bool {
	if !strings.Contains(s, ":") {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && r != ':' && r != '.' {
			return false
		}
	}

	return true
}
