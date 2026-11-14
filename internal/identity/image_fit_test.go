package identity

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type velaImagePixels []byte

func (p velaImagePixels) Get(context.Context, string, int) ([]byte, string, error) {
	return p, "image/jpeg", nil
}

func TestVelaGrantedImagePixels(t *testing.T) {
	var pixels bytes.Buffer
	if err := jpeg.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 640, 360)), nil); err != nil {
		t.Fatal(err)
	}
	service := &ImageGrantService{surface: "vela", images: velaImagePixels(pixels.Bytes())}
	for _, tc := range []struct {
		query         string
		width, height int
	}{{"", 640, 360}, {"?w=0", 640, 360}, {"?w=320&h=180", 320, 180}, {"?w=200", 200, 112}, {"?h=90", 160, 90}, {"?w=1920&h=1080", 640, 360}} {
		req := httptest.NewRequest("GET", "/"+tc.query, nil)
		w := httptest.NewRecorder()
		service.writeGrantedImage(w, req, &core.Viewer{ID: 1}, core.Device{}, core.MovieKey(1), "", frontend.Image{Kind: frontend.ImageBackdrop}, "", "", "")
		cfg, _, err := image.DecodeConfig(w.Body)
		if err != nil || w.Code != 200 || cfg.Width != tc.width || cfg.Height != tc.height {
			t.Fatalf("%s: status%d pixels%dx%d error%v", tc.query, w.Code, cfg.Width, cfg.Height, err)
		}
	}
}
