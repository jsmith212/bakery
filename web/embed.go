// Package web embeds the compiled SvelteKit single-page application.
package web

import (
	"embed"
	"io/fs"
)

// The `all:` prefix is mandatory and load-bearing. Without it, go:embed applies
// its default walk rules and silently drops every file and directory whose name
// begins with `_` or `.` -- which is precisely SvelteKit's `_app/` tree, where
// every hashed JS and CSS chunk lives. The binary still links, `/` still returns
// index.html, and the app renders a white page in production. embed_test.go
// exists to fail loudly if this prefix is ever dropped.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the SvelteKit build output rooted at the build directory, so that
// index.html is at the root of the returned filesystem.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
