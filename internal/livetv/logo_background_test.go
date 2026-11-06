package livetv

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/logotile"
)

type pngLogoFetcher struct {
	data []byte
	ct   string
}

func (f *pngLogoFetcher) GetUntrustedURL(context.Context, string) ([]byte, string, error) {
	return f.data, f.ct, nil
}

func logoPNG(t *testing.T, ink color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			img.SetNRGBA(x, y, ink)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mixedLogoPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 512, 128))
	for y := 10; y < 118; y++ {
		for x := 140; x < 500; x++ {
			img.SetNRGBA(x, y, color.NRGBA{20, 25, 55, 255})
		}
	}
	for y := 40; y < 88; y++ {
		for x := 20; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	for y := 45; y < 80; y++ {
		for x := 190; x < 450; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The console picks the tile from this header, so it must be present on every
// logo response: the decision when the image decodes, dark when it does not.
func TestOwnerChannelLogoBackgroundHeader(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		ct   string
		want logotile.Background
	}{
		{"white ink needs the dark tile", logoPNG(t, color.NRGBA{255, 255, 255, 255}), "image/png", logotile.Dark},
		{"black ink needs the light tile", logoPNG(t, color.NRGBA{0, 0, 0, 255}), "image/png", logotile.Light},
		{"separate white ink beside dark plate", mixedLogoPNG(t), "image/png", logotile.Dark},

		{"broken bytes keep the dark default", []byte("logo"), "image/png", logotile.Dark},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t, Options{})
			c := testChannel()
			c.Logo = "https://images.example/logo.png"
			if _, err := s.SaveChannel(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			h := s.ownerHandler(&pngLogoFetcher{data: tc.data, ct: tc.ct})
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/channels/test/logo", nil))
			if r.Code != http.StatusOK {
				t.Fatalf("status %d", r.Code)
			}
			if got := r.Header().Get(logotile.Header); got != string(tc.want) {
				t.Fatalf("%s header = %q, want %q", logotile.Header, got, tc.want)
			}
			if got := r.Header().Get("Content-Type"); got != tc.ct {
				t.Fatalf("content type = %q, want %q", got, tc.ct)
			}
			if !bytes.Equal(r.Body.Bytes(), tc.data) {
				t.Fatal("body changed")
			}
		})
	}
}

func TestOwnerSVGIsRasterizedAndScored(t *testing.T) {
	s := testService(t, Options{})
	c := testChannel()
	c.Logo = "https://images.example/logo.svg"
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="black"/></svg>`,
		`<svg><image href="http://127.0.0.1/secret"/></svg>`,
		`<svg viewBox="0 0 10 10"><path d="M1e100 0"/></svg>`,
	} {
		h := s.ownerHandler(&pngLogoFetcher{data: []byte(body), ct: "image/svg+xml"})
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/channels/test/logo", nil))
		if bytes.Contains([]byte(body), []byte("fill=")) {
			if r.Code != 200 || r.Header().Get("Content-Type") != "image/png" || r.Header().Get(logotile.Header) != "light" {
				t.Fatalf("raster response: %d %v", r.Code, r.Header())
			}
			if _, err := png.Decode(r.Body); err != nil {
				t.Fatal(err)
			}
		} else if r.Code != 404 {
			t.Fatalf("hostile SVG status: %d", r.Code)
		}
	}
}
