package server

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// withStaticFiles serves the pre-built web client from the same origin as the
// API, falling back to the router for anything that is not a file.
//
// Same origin matters: the browser client calls the API at a relative
// "/api/v2" path, so hosting it elsewhere would need CORS on every request.
func (a *App) withStaticFiles(api http.Handler) http.Handler {
	root := a.cfg.WebDir
	fileServer := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			api.ServeHTTP(w, r)
			return
		}

		cleaned := path.Clean("/" + r.URL.Path)
		target := filepath.Join(root, filepath.FromSlash(cleaned))
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// A directory request resolves to its index page, which is how the
		// static export writes routes ("/login/" -> login/index.html).
		if info, err := os.Stat(filepath.Join(target, "index.html")); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Unknown paths go to the client so it can render its own 404.
		if strings.Contains(path.Base(cleaned), ".") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}

// isAPIPath reports whether the router owns this path.
func isAPIPath(p string) bool {
	if p == "/healthz" {
		return true
	}
	for _, prefix := range []string{"/api/", "/.well-known/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return p == "/api"
}
