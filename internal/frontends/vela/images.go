package vela

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Image sizes describe cached source pixels. The URL explicitly asks for the
// same aspect-preserving bounds, so a backdrop is never a default thumbnail.
// Zero source dimensions remain unknown; those URLs preserve the source.
func imageSize(img frontend.Image, secret string) object {
	width, height := img.Width, img.Height
	if width <= 0 || height <= 0 {
		width, height = 0, 0
	}
	if width > 4000 {
		height = max(1, height*4000/width)
		width = 4000
	}
	if height > 4000 {
		width = max(1, width*4000/height)
		height = 4000
	}
	query := url.Values{"w": []string{strconv.Itoa(width)}}
	if width > 0 {
		query.Set("w", strconv.Itoa(width))
	}
	if height > 0 {
		query.Set("h", strconv.Itoa(height))
	}
	path := "/vela/v0/images/" + secret
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return object{"width": width, "height": height, "url": path}
}

func (i *instance) image(w http.ResponseWriter, r *http.Request) {
	if i.host.ImageGrants == nil {
		i.unavailable(w, r)
		return
	}
	query := r.URL.Query()
	// Use one sizing vocabulary. Previously width= was silently ignored.
	for _, alias := range []string{"width", "height", "maxWidth", "fillWidth", "maxHeight", "fillHeight"} {
		if _, exists := query[alias]; exists {
			problem(w, 400, "request", "invalid_image_size")
			return
		}
	}
	for _, name := range []string{"w", "h"} {
		values, exists := query[name]
		if !exists {
			continue
		}
		if len(values) != 1 {
			problem(w, 400, "request", "invalid_image_size")
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			problem(w, 400, "request", "invalid_image_size")
			return
		}
		query.Set(name, strconv.Itoa(min(n, 4000)))
	}
	cloned := r.Clone(r.Context())
	u := *r.URL
	u.RawQuery = query.Encode()
	cloned.URL = &u
	i.host.ImageGrants.Serve(w, cloned, r.PathValue("secret"))
}
