package regconf

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestSkopeoInspectRawServesTheStoredBytes is spec §8.3: a third-party client asks the
// writable namespace for a manifest and gets back, byte for byte, what was pushed.
//
// # Why a byte comparison, and why through skopeo rather than through a Go client
//
// "Never re-serialize a manifest" is the OCI backend's oldest invariant, and its failure
// mode is invisible from inside the process: a document that is unmarshalled and
// re-marshalled still parses, still validates, still looks right in a test that decodes
// it -- and hashes differently, so every client that verifies a digest fails forever on
// that image. The only assertion that catches it is a comparison of BYTES that came back
// over a socket, and the strongest version of that is one where a real third-party stack
// did the fetching. skopeo (containers/image, which is also podman's and CRI-O's) is
// that stack, and --raw is its "give me the manifest and nothing else" mode.
//
// # What is pushed
//
// The `image-manifest=true` BuildKit cache shape: an ordinary OCI image manifest whose
// CONFIG descriptor names a vnd.buildkit.cacheconfig.v0 document. That is the shape
// config-allowlisting registries reject, and Bakery accepts it because nothing in the
// writable namespace opens a manifest. The bytes are formatted the way no encoder
// formats anything (see buildKitCacheManifest), so a round trip through any JSON codec
// is detectable rather than merely improbable.
//
// The push runs over raw HTTP rather than through buildx on purpose: this test's subject
// is the READ path through a real client, and coupling it to a docker daemon would make
// the byte-identity proof unavailable on any machine that has skopeo and not docker.
//
// requireBinary runs FIRST, before dbtest spawns a Postgres: `just race` and
// `just coverage` glob ./..., and a skip on a laptop without skopeo must cost nothing.
// `just registry-conformance` installs skopeo and turns a skip into a job failure.
func TestSkopeoInspectRawServesTheStoredBytes(t *testing.T) {
	skopeo := requireBinary(t, "skopeo")

	e := newEnv(t, loopback())

	const (
		repo = "impulse"
		tag  = "skopeo"
	)

	// Opaque to every party in this test, and that is the point: a cache config and a
	// cache layer are documents only BuildKit reads.
	config := []byte(`{"layers":[],"records":[{"digest":"sha256:0"}]}`)
	layer := []byte("buildkit cache layer bytes -- opaque to the registry and to this test")

	configDigest := e.pushBlob(t, repo, config)
	layerDigest := e.pushBlob(t, repo, layer)

	raw := buildKitCacheManifest(configDigest, len(config), layerDigest, len(layer))

	digest := e.pushManifest(t, repo, tag, raw, imageManifestType)

	// The READ-scoped key, which is also the half of spec §8.2 that says an import needs
	// no write authority.
	ref := "docker://" + e.host + "/" + envOrg + "/" + envProj + "/" + segBuildCache +
		"/" + repo + ":" + tag

	out := runSkopeo(t, skopeo, e.readKey, ref, "inspect", "--raw")

	if !bytes.Equal(out, raw) {
		t.Errorf("skopeo inspect --raw returned %d bytes, want the %d pushed, byte for byte\n"+
			"  got:  %s\n  want: %s", len(out), len(raw), out, raw)
	}

	if got := digestOf(out); got != digest {
		t.Errorf("the bytes skopeo received hash to %s, but the registry stores and reports "+
			"%s -- a manifest that is re-serialized anywhere on the read path breaks every "+
			"client that verifies a digest", got, digest)
	}

	e.assertNoUpstream(t)
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
