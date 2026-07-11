package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

// immutablePrefix is where SvelteKit writes its content-hashed assets. Their
// names change whenever their contents change, so they can be cached forever.
const immutablePrefix = "_app/immutable/"

// SPAHandler serves the embedded SvelteKit build.
//
// A request that maps to a real file is served as that file, with a content type
// inferred from its extension. Anything else falls back to index.html so that
// client-side routing works on a cold load or a refresh -- deep links must reach
// the app, not a 404.
func SPAHandler(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		if !isFile(dist, name) {
			serveIndex(w, r, dist)

			return
		}

		if strings.HasPrefix(name, immutablePrefix) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		files.ServeHTTP(w, r)
	})
}

func isFile(dist fs.FS, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}

	info, err := fs.Stat(dist, name)
	if err != nil {
		return false
	}

	return !info.IsDir()
}

func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	f, err := dist.Open("index.html")
	if err != nil {
		slog.Error("index.html missing from the embedded assets: frontend not built", "error", err)
		http.Error(w, "frontend not built", http.StatusInternalServerError)

		return
	}
	defer f.Close()

	// embed.FS files implement io.ReadSeeker, which lets ServeContent set
	// Content-Length and handle range requests.
	content, ok := f.(io.ReadSeeker)
	if !ok {
		slog.Error("embedded index.html is not seekable")
		http.Error(w, "frontend not built", http.StatusInternalServerError)

		return
	}

	// The HTML shell names the hashed assets, so it must never be cached; the
	// assets it points at are immutable and cached hard.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	http.ServeContent(w, r, "index.html", time.Time{}, content)
}
