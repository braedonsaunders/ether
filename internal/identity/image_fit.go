package identity

import (
	"bytes"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"

	"github.com/braedonsaunders/ether/internal/core"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

// Legacy image sources return CDN buckets. Apply the same source-clamped fit
// used by Artwork when serving an explicit Vela image size from that backend.
func fitGrantedImage(data []byte, contentType string, width, height int) ([]byte, string, error) {
	if width == 0 && height == 0 {
		return data, contentType, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return nil, "", core.ErrNotAvailable
	}
	w, h := cfg.Width, cfg.Height
	if width > 0 && w > width {
		h = max(1, h*width/w)
		w = width
	}
	if height > 0 && h > height {
		w = max(1, w*height/h)
		h = height
	}
	if w == cfg.Width && h == cfg.Height {
		return data, contentType, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	resized, err := kitimages.Resize(src, w, h)
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	if contentType == "image/png" {
		err = png.Encode(&out, resized)
	} else {
		contentType = "image/jpeg"
		err = jpeg.Encode(&out, resized, &jpeg.Options{Quality: 90})
	}
	return out.Bytes(), contentType, err
}
