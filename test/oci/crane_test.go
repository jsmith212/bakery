package ociconf

import (
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/validate"
)

// TestCranePullsThroughBothRouteFamilies drives go-containerregistry -- crane's engine,
// and the same library Bakery uses to talk upstream -- at BOTH of Bakery's route
// families, by tag and by digest.
//
// The two families exist because the four clients disagree about where a mirror's path
// prefix goes, and the disagreement is not a matter of taste:
//
//	/cache/{org}/{project}/docker/v2/...   containerd and Docker Engine: prefix BEFORE /v2
//	/v2/{org}/{project}/...                BuildKit and podman: prefix AFTER /v2
//
// crane reaches the second family natively -- the tenant prefix is simply the first two
// components of the repository name, which is exactly what a reference-rewriting mirror
// (podman's registries.conf) produces. It reaches the first through a RoundTripper that
// prepends the mirror path, which is precisely what containerd's RegistryHost.Path and
// Docker Engine's mirror base URL do at the same layer.
//
// PULL BY TAG AND PULL BY DIGEST ARE DIFFERENT CODE PATHS IN BAKERY and must both work:
// a tag is mutable and goes through the whole stale-while-revalidate machine, a digest
// is immutable and must never touch the upstream once cached.
func TestCranePullsThroughBothRouteFamilies(t *testing.T) {
	e := newEnv(t)

	families := []struct {
		name string
		repo string
		opts []remote.Option
	}{
		{
			name: "buildkit/podman: /v2/{org}/{project}/...",
			repo: e.host() + "/" + e.repo2(projMain, repoIndex),
			opts: craneOptions(e, projMain, ""),
		},
		{
			name: "containerd/docker-engine: /cache/{org}/{project}/docker/v2/...",
			repo: e.host() + "/" + repoIndex,
			opts: craneOptions(e, projMain, e.mirrorPrefix(projMain)),
		},
	}

	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			tagRef, err := name.ParseReference(f.repo + ":" + tagIndex)
			if err != nil {
				t.Fatalf("parse tag reference: %v", err)
			}

			byTag, err := remote.Get(tagRef, f.opts...)
			if err != nil {
				t.Fatalf("remote.Get by tag: %v", err)
			}

			if byTag.Digest.String() != indexDigest {
				t.Errorf("by-tag digest = %s, want %s", byTag.Digest, indexDigest)
			}

			if string(byTag.MediaType) != indexMediaType {
				t.Errorf("by-tag media type = %q, want %q", byTag.MediaType, indexMediaType)
			}

			if digestOf(byTag.Manifest) != indexDigest {
				t.Errorf("the by-tag manifest body hashes to %s, want %s -- the bytes were rewritten",
					digestOf(byTag.Manifest), indexDigest)
			}

			// By digest. go-containerregistry VERIFIES a digest-addressed manifest against
			// the digest it asked for (it deliberately does not for tags -- "Do nothing for
			// tags; I give up" -- because too many registries get Docker-Content-Digest
			// wrong), so this call failing at all is a byte-fidelity failure.
			digestRef, err := name.ParseReference(f.repo + "@" + indexDigest)
			if err != nil {
				t.Fatalf("parse digest reference: %v", err)
			}

			byDigest, err := remote.Get(digestRef, f.opts...)
			if err != nil {
				t.Fatalf("remote.Get by digest: %v", err)
			}

			if string(byDigest.Manifest) != string(byTag.Manifest) {
				t.Error("the by-digest and by-tag manifests differ -- one of them is not the stored bytes")
			}
		})
	}
}

// TestCraneValidatesAWholeImage pulls a COMPLETE image through Bakery -- manifest,
// config blob and every layer blob -- and runs go-containerregistry's own validator over
// it.
//
// It is the only test here that exercises the blob path end to end under a client that
// verifies. validate.Image re-hashes every layer, checks the config's diffIDs against
// the uncompressed layers, and checks the manifest's descriptors against the blobs it
// actually received; a single wrong byte, a truncated stream or a swapped Content-Length
// anywhere in Bakery's blob ingest fails it.
func TestCraneValidatesAWholeImage(t *testing.T) {
	e := newEnv(t)

	ref, err := name.ParseReference(e.host() + "/" + e.repo2(projMain, repoProbe) + ":" + tagProbe)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}

	opts := craneOptions(e, projMain, "")

	img, err := remote.Image(ref, opts...)
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}

	if err := validate.Image(img); err != nil {
		t.Fatalf("validate.Image through Bakery: %v", err)
	}

	// ---- and now the anti-bypass half: pull the whole thing again and prove the
	// upstream saw nothing. Manifests and blobs are content-addressed and the tag is
	// inside its TTL, so a correct mirror makes zero upstream requests here.
	if e.up.count() == 0 {
		t.Fatal("the cold pull contacted the upstream zero times -- the fixture proves nothing")
	}

	e.up.reset()

	warm, err := remote.Image(ref, craneOptions(e, projMain, "")...)
	if err != nil {
		t.Fatalf("warm remote.Image: %v", err)
	}

	if err := validate.Image(warm); err != nil {
		t.Fatalf("validate.Image on the warm pull: %v", err)
	}

	if n := e.up.count(); n != 0 {
		t.Errorf("the warm image pull made %d upstream request(s): %v.\nEvery client falls back to "+
			"the real registry in silence, so a pull that succeeds without this assertion "+
			"proves nothing about who served it.", n, e.up.requests())
	}
}

// craneOptions builds the go-containerregistry options for one tenant.
//
// The credential is Basic with the `bkry_` token in the PASSWORD field, which is one of
// the four places auth accepts it; go-containerregistry turns that into the Bearer token
// dance against the realm Bakery advertises, which is the whole point -- the dance is
// what the four real clients do, and the realm has to be an absolute URL they can reach.
//
// prefix, when non-empty, is a mirror path prepended to every /v2/... request. See
// prefixTransport.
func craneOptions(e *env, project, prefix string) []remote.Option {
	opts := []remote.Option{
		remote.WithAuth(&authn.Basic{Username: "bakery", Password: e.key(project)}),
	}

	if prefix == "" {
		return append(opts, remote.WithTransport(http.DefaultTransport))
	}

	return append(opts, remote.WithTransport(
		prefixTransport{base: http.DefaultTransport, prefix: prefix}))
}
