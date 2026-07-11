package web

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// TestEmbedIncludesHiddenEntries is the tripwire for the `all:` prefix on the
// embed directive in embed.go.
//
// Without `all:`, go:embed silently skips every entry whose name begins with `_`
// or `.`. That drops SvelteKit's entire `_app/` tree -- every hashed JS and CSS
// chunk the page loads -- while still compiling cleanly and still serving
// index.html. The failure mode is a white page in production and a green test
// suite, which is why this assertion exists.
//
// It holds in both tree states:
//
//   - fresh clone, frontend never built: dist holds only the .gitkeep placeholder,
//     which `all:` matches and the default rules do not. (Drop `all:` here and the
//     package does not even compile -- the pattern matches no files.)
//   - after a real frontend build: dist holds `_app/`.
//
// Do not delete this test, and do not "simplify" the directive.
func TestEmbedIncludesHiddenEntries(t *testing.T) {
	var hidden []string

	err := fs.WalkDir(dist, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == "." {
			return nil
		}

		if base := path.Base(p); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
			hidden = append(hidden, p)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded dist: %v", err)
	}

	if len(hidden) == 0 {
		t.Fatal(
			"embedded filesystem contains no dot- or underscore-prefixed entries: " +
				"the //go:embed directive in embed.go has lost its `all:` prefix, so " +
				"SvelteKit's _app/ assets are missing and the app will serve a white page",
		)
	}
}

// TestEmbeddedBuildIsServable asserts the embedded tree is a real SvelteKit
// build: index.html plus the _app/ asset tree. It skips on a tree where the
// frontend has never been built, because a fresh clone legitimately carries only
// the placeholder; TestEmbedIncludesHiddenEntries covers the `all:` prefix in
// that state.
func TestEmbeddedBuildIsServable(t *testing.T) {
	dist, err := Dist()
	if err != nil {
		t.Fatalf("Dist(): %v", err)
	}

	if _, err := fs.Stat(dist, "index.html"); err != nil {
		t.Skip("frontend not built (web/dist has no index.html); run the frontend build first")
	}

	tests := []struct {
		name string
		path string
		dir  bool
	}{
		{name: "spa shell", path: "index.html"},
		{name: "hashed asset tree", path: "_app", dir: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := fs.Stat(dist, tt.path)
			if err != nil {
				t.Fatalf("stat %q in embedded dist: %v", tt.path, err)
			}

			if info.IsDir() != tt.dir {
				t.Fatalf("%q: got IsDir()=%v, want %v", tt.path, info.IsDir(), tt.dir)
			}
		})
	}

	// Counted with a walk rather than fs.Glob: Go's glob is path.Match, which has
	// no `**` operator, so a pattern like "_app/immutable/**/*.js" would silently
	// depend on how deeply SvelteKit happens to nest its chunks today.
	var chunks int

	err = fs.WalkDir(dist, "_app", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(p, ".js") {
			chunks++
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded _app: %v", err)
	}

	if chunks == 0 {
		t.Error("embedded _app/ contains no JS chunks; the SPA has no code to run")
	}
}
