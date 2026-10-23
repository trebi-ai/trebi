package ui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler serves fsys at the root and delegates the daemon's API surfaces to
// api. A path that is not a file falls back to index.html so a client-side
// route survives a hard refresh. Hashed assets under /assets are cached
// immutably; everything else (index.html, manifest.json, the fallback) is
// no-store so a new release is picked up on the next load.
func Handler(fsys fs.FS, api http.Handler) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiPath(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		if !exists(fsys, r.URL.Path) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// apiPath reports whether p belongs to the daemon and must not fall back to
// the SPA. Keep this list equal to the API surfaces served by internal/api.
func apiPath(p string) bool {
	for _, prefix := range []string{"/v1", "/mcp", "/healthz", "/login/done"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// exists maps a URL path onto fsys. "." is the fs root, which FileServerFS
// answers with index.html.
func exists(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "."
	}
	_, err := fs.Stat(fsys, name)
	return err == nil
}
