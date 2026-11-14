package mediabrowser

import (
	"bytes"
	"errors"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/webclient"
)

var errNotAFile = errors.New("not a regular file")

// serveWeb answers Jellyfin Web asset requests from the verified install.
// Emby keeps today's behaviour (a connection page, or 404 for asset paths).
// While the first download runs it shows a short self-refreshing page, and
// after a failure it shows the connection page with a single explanatory line.
func serveWeb(w http.ResponseWriter, r *http.Request, emby bool, client *webclient.Installer) {
	rel := r.PathValue("path")
	if emby {
		if rel != "" {
			http.NotFound(w, r)
			return
		}
		webPage(w, r)
		return
	}
	// Relative asset URLs in index.html need the trailing slash, so a bare
	// /web (path "" with no trailing slash) redirects to /web/.
	if rel == "" && !strings.HasSuffix(r.URL.Path, "/") {
		target := "/web/"
		if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
			target = prefix + target
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if client == nil {
		webPage(w, r)
		return
	}

	status, err := client.Begin()
	switch status {
	case webclient.StatusDownloading:
		webPreparing(w)
		return
	case webclient.StatusFailed:
		webFailed(w, r, err)
		return
	}

	dir, err := client.Ensure(r.Context())
	if err != nil {
		webFailed(w, r, err)
		return
	}
	if rel == "" {
		rel = "index.html"
	}
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	info, statErr := resolveFile(full)
	if statErr != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", webContentType(rel))
	w.Header().Set("Cache-Control", webCacheControl(rel))
	if webclient.SubtitleAdapterAsset(rel) {
		raw, err := os.ReadFile(info)
		if err == nil {
			raw, err = webclient.AdaptSubtitles(rel, raw)
		}
		if err != nil {
			http.Error(w, "web player unavailable", http.StatusServiceUnavailable)
			return
		}
		// Revalidate across Ether upgrades even though upstream filenames are hashed.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, rel, time.Time{}, bytes.NewReader(raw))
		return
	}
	http.ServeFile(w, r, info)
}

// resolveFile refuses directories and anything outside the install directory
// (no listings, no traversal), returning the resolved path.
func resolveFile(full string) (string, error) {
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errNotAFile
	}
	return full, nil
}

// webContentType maps the extensions Jellyfin Web ships to explicit types,
// since the platform mime table does not know them all.
func webContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".html":
		return "text/html; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// webCacheControl gives the shell and mutable manifests no-cache so an upgrade
// lands immediately, and lets hashed bundles cache for a week.
func webCacheControl(name string) string {
	switch path.Base(name) {
	case "index.html", "config.json", "serviceworker.js":
		return "no-cache"
	}
	// Jellyfin Web fingerprints its bundles as name.<hash>.js or with a build
	// directory; treat compiled bundles and assets as immutable.
	if strings.Contains(path.Base(name), ".") && !strings.HasSuffix(name, ".html") {
		return "public, max-age=604800"
	}
	return "no-cache"
}

// webPreparing is the short page shown while the first download runs. It is
// not owner-console copy, so it stays plain English and refreshes itself.
func webPreparing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Refresh", "3")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ether</title><style>body{margin:0;background:#0e1116;color:#e7e9ee;font:16px/1.45 system-ui,sans-serif}main{max-width:32rem;margin:4rem auto;padding:0 1.25rem}h1{font-size:1.5rem;font-weight:650}p{color:#b7bdc9}</style><main><h1>Preparing the Jellyfin web client…</h1><p>This page refreshes automatically.</p></main></html>`))
}

// webFailed keeps the connection page (native apps still work) and adds one
// line explaining the web client could not be downloaded.
func webFailed(w http.ResponseWriter, r *http.Request, _ error) {
	w.Header().Set("X-Web-Client-Error", "download-failed")
	webPageWithNote(w, r, "The Jellyfin web client could not be downloaded. Native Jellyfin apps still work.")
}
