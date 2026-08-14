package ociconf

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestSkopeoInspect drives the REAL skopeo binary -- the containers/image stack, which
// is also podman's and CRI-O's -- at Bakery.
//
// It is the only client in the ecosystem that HARD-REQUIRES the bare-root `GET /v2/`
// ping: containers/image pings the host root (outside any tenant prefix), errors on
// anything but 200 or 401, and harvests its authentication challenges from that response
// AND NOWHERE ELSE. It is also the only one that never sends ?ns= -- mirrors are
// reference rewrites there, so the tenant prefix lands in the repository position and
// Bakery has to fall back to the backend's default_upstream.
//
// requireBinary runs FIRST, before dbtest spawns a Postgres: `just race` and
// `just coverage` glob ./..., and a skip on a laptop without skopeo must cost nothing.
// `just oci-conformance` installs skopeo and turns a skip into a job failure.
func TestSkopeoInspect(t *testing.T) {
	skopeo := requireBinary(t, "skopeo")

	e := newEnv(t)
	key := e.key(projOpen)

	// --raw returns the manifest bytes VERBATIM, which is the assertion worth making
	// against a multi-arch index: skopeo without --raw would pick a platform and chase a
	// child manifest, and the digest of what came back would no longer be the digest of
	// what Bakery served.
	indexRef := "docker://" + e.host() + "/" + e.repo2(projOpen, repoIndex) + ":" + tagIndex

	raw := runSkopeo(t, skopeo, key, indexRef, "inspect", "--raw")

	if digestOf(raw) != indexDigest {
		t.Errorf("skopeo inspect --raw returned bytes hashing to %s, want %s -- skopeo is the "+
			"client that would notice a re-serialized manifest last, because it does not "+
			"verify tag fetches", digestOf(raw), indexDigest)
	}

	// A complete image, so the config blob is fetched too: `skopeo inspect` without
	// --raw resolves the manifest, then GETs the config blob and parses it. That is the
	// blob path, driven by a real binary.
	out := runSkopeo(t, skopeo, key,
		"docker://"+e.host()+"/"+e.repo2(projOpen, repoProbe)+":"+tagProbe, "inspect")

	var inspected struct {
		Digest string `json:"Digest"`
		Layers []string
	}

	if err := json.Unmarshal(out, &inspected); err != nil {
		t.Fatalf("decode skopeo inspect: %v (output %s)", err, out)
	}

	if !strings.HasPrefix(inspected.Digest, "sha256:") {
		t.Errorf("skopeo reported digest %q", inspected.Digest)
	}

	if len(inspected.Layers) == 0 {
		t.Error("skopeo reported no layers -- the config blob was not read")
	}

	// The anti-bypass half. skopeo falls back to nothing here (the reference names
	// Bakery, not Docker Hub), but the upstream counter still proves the second inspect
	// was answered out of Bakery's own store rather than re-fetched.
	e.up.reset()

	again := runSkopeo(t, skopeo, key, indexRef, "inspect", "--raw")

	if !bytes.Equal(again, raw) {
		t.Error("the second skopeo inspect returned different bytes from the first")
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("the warm skopeo inspect made %d upstream request(s): %v", n, e.up.requests())
	}
}

// TestSkopeoListTags pins the tags/list CONTENT with the real binary. The plain
// `skopeo inspect` in TestSkopeoInspect already hard-requires the endpoint to exist
// (containers/image lists tags on every inspect and fatals on a 404 -- the failure
// that broke CI the first time skopeo actually ran); this test additionally asserts
// WHAT it lists: exactly the tags this cache holds for the repository, scoped so a
// sibling repository's tags do not leak in.
func TestSkopeoListTags(t *testing.T) {
	skopeo := requireBinary(t, "skopeo")

	e := newEnv(t)
	key := e.key(projOpen)

	// Warm two repositories through the cache, so the listing has content AND an
	// adjacent repo whose tags must not appear.
	runSkopeo(t, skopeo, key,
		"docker://"+e.host()+"/"+e.repo2(projOpen, repoProbe)+":"+tagProbe, "inspect", "--raw")
	runSkopeo(t, skopeo, key,
		"docker://"+e.host()+"/"+e.repo2(projOpen, repoIndex)+":"+tagIndex, "inspect", "--raw")

	out := runSkopeo(t, skopeo, key,
		"docker://"+e.host()+"/"+e.repo2(projOpen, repoProbe), "list-tags")

	var listed struct {
		Tags []string
	}

	if err := json.Unmarshal(out, &listed); err != nil {
		t.Fatalf("decode skopeo list-tags: %v (output %s)", err, out)
	}

	if len(listed.Tags) != 1 || listed.Tags[0] != tagProbe {
		t.Errorf("skopeo list-tags = %v, want exactly [%s]: the listing is the cached tags "+
			"of THIS repository, not its neighbours'", listed.Tags, tagProbe)
	}
}

// runSkopeo executes skopeo against a cleartext loopback registry.
//
// --tls-verify=false is not laziness: containers/image pings https FIRST and only falls
// back to http when insecure verification is permitted, so without it skopeo never
// reaches the server at all. --creds carries the `bkry_` token as the PASSWORD half of a
// Basic credential; a Bakery credential is one opaque token with no id:secret halves, so
// the username is filler and the token authenticates from either field.
//
// The argument ORDER is fixed rather than free-form: --insecure-policy is a GLOBAL flag
// and has to precede the subcommand, the per-command flags follow it, and the image
// reference goes last. Getting that wrong produces a usage error that looks like a
// Bakery failure.
func runSkopeo(t *testing.T, bin, token, ref string, args ...string) []byte {
	t.Helper()

	full := append([]string{"--insecure-policy"}, args...)
	full = append(full, "--tls-verify=false", "--creds=bakery:"+token, ref)

	cmd := exec.CommandContext(t.Context(), bin, full...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("skopeo %s: %v\nstderr: %s", strings.Join(full, " "), err, stderr.String())
	}

	return out
}
