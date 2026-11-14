package webclient

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// VelaReady reports an owner-installed optional build. No download is attempted
// until a pinned Vela release artifact is published and verified by the host.
func VelaReady(dir string) bool {
	if dir == "" {
		return false
	}
	s, e := os.Stat(filepath.Join(dir, "index.html"))
	return e == nil && !s.IsDir()
}

// ServeVela serves the installed build with SPA deep-link fallback. API paths
// are dispatched by the adapter before reaching this function.
func ServeVela(w http.ResponseWriter, r *http.Request, dir string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !VelaReady(dir) {
		http.Error(w, "Vela client is not installed", 503)
		return
	}
	if r.URL.Path == "/vela" {
		http.Redirect(w, r, "/vela/", 307)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/vela/")
	f, e := os.OpenRoot(dir)
	if e != nil {
		http.Error(w, "Vela client is unavailable", 503)
		return
	}
	defer f.Close()
	s, e := f.Stat(path)
	if e != nil || s.IsDir() {
		r = r.Clone(r.Context())
		u := *r.URL
		u.Path = "/"
		r.URL = &u
	} else {
		r = r.Clone(r.Context())
		u := *r.URL
		u.Path = "/" + path
		r.URL = &u
	}
	http.FileServer(http.FS(f.FS())).ServeHTTP(w, r)
}
