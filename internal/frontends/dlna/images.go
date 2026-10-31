package dlna

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) posterFor(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, key frontend.RatingKey) frontend.Image {
	if i.host.Artwork == nil || viewer == nil {
		return frontend.Image{}
	}
	images, _, err := i.host.Artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: key})
	if err != nil {
		return frontend.Image{}
	}
	for _, image := range images {
		if image.Kind == frontend.ImagePoster {
			return image
		}
	}
	return frontend.Image{}
}

func (i *instance) serveImage(w http.ResponseWriter, r *http.Request) {
	viewer, _ := r.Context().Value(viewerKey{}).(*frontend.Viewer)
	if viewer == nil || i.host.Artwork == nil || i.host.ClientIP == nil {
		http.NotFound(w, r)
		return
	}
	key, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || key <= 0 {
		http.NotFound(w, r)
		return
	}
	peer, ok := i.host.ClientIP(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	device := frontend.LANDevice(peer)
	ctx := frontend.WithClientIP(r.Context(), peer)
	item := frontend.RatingKey(key)
	image := i.posterFor(ctx, viewer, device, item)
	if image.Kind != frontend.ImagePoster {
		http.NotFound(w, r)
		return
	}
	etag := dlnaETag(image)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, contentType, err := i.host.Artwork.Read(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: item}, image, frontend.ArtworkTransform{})
	if err != nil || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	if contentType == "" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func dlnaETag(image frontend.Image) string {
	identity := image.Tag
	if identity == "" {
		identity = image.Path
	}
	sum := sha256.Sum256([]byte(identity))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
