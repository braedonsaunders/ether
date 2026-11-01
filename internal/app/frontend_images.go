package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/artwork"
)

type frontendImageSource interface {
	Get(context.Context, string, int) ([]byte, string, error)
}

type frontendImages struct{ source frontendImageSource }

// ServeHTTP serves a TMDB image path requested with ?path=...&w=... . The
// frontend is responsible for authorizing the viewer's access to the item.
func (images frontendImages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Query().Get("path")
	if !artwork.ValidPath(path) {
		http.Error(w, "invalid image path", http.StatusBadRequest)
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 || width > 4000 {
		width = 500
	}
	data, kind, err := images.source.Get(r.Context(), path, width)
	if err != nil {
		if errors.Is(err, artwork.ErrInvalidPath) {
			http.Error(w, "invalid image path", http.StatusBadRequest)
			return
		}
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
