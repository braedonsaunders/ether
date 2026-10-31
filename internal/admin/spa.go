package admin

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// handleSPA serves the embedded React build. Real files are served
// directly (fingerprinted assets get immutable caching); any other GET
// falls back to index.html so client-side routes survive reloads.
func (h *Handler) handleSPA(w http.ResponseWriter, r *http.Request) { serveSPA(w, r) }

// Shell serves the console's static files without its API, for a server
// whose settings and database are not open yet (internal/startup's status
// page). The owner's frame allowlist is not loaded then, so framing is
// refused outright.
func Shell() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/ether")
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'; "+consoleScriptPolicy())
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		serveSPA(w, r2)
	})
}

func serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported here")
		return
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "embedded UI unavailable")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if name == "index.html" || !fileExists(sub, name) {
		serveIndex(w, r, sub)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFileFS(w, r, sub, name)
}

func fileExists(fsys fs.FS, name string) bool {
	fi, err := fs.Stat(fsys, name)
	return err == nil && !fi.IsDir()
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	if !fileExists(sub, "index.html") {
		// `go build` from a checkout without `pnpm build`: the API still
		// works; tell the owner how to get the UI rather than 404ing mutely.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<!doctype html><title>Ether</title>` +
			`<body style="font-family:system-ui;background:#0b0e14;color:#e6edf3;padding:2rem">` +
			`<h1>Ether admin UI is not built</h1><p>The API is running at <code>/ether/api/</code>. ` +
			`Build the web UI with <code>cd web &amp;&amp; pnpm install &amp;&amp; pnpm build</code>, then restart Ether.</p></body>`))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, sub, "index.html")
}
