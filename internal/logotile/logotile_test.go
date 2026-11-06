package logotile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// fill paints a rectangle of img with c, clipped to its bounds.
func fill(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	b := img.Bounds()
	for y := max(y0, b.Min.Y); y < min(y1, b.Max.Y); y++ {
		for x := max(x0, b.Min.X); x < min(x1, b.Max.X); x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func logo(w, h int) *image.NRGBA {
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

var (
	white = color.NRGBA{255, 255, 255, 255}
	trans = color.NRGBA{0, 0, 0, 0}
)

func TestDecide(t *testing.T) {
	// Sky News: light "sky" wordmark on the left, red plate with white "news"
	// on the right. The red plate outnumbers the white ink, so the white
	// wordmark has to force the dark tile.
	skyNews := logo(512, 124)
	fill(skyNews, 0, 20, 220, 104, color.NRGBA{240, 240, 240, 255})
	fill(skyNews, 220, 10, 512, 114, color.NRGBA{200, 16, 46, 255})
	fill(skyNews, 300, 60, 470, 90, white)

	// Red mark only, on transparency.
	redOnly := logo(64, 64)
	fill(redOnly, 8, 20, 56, 44, color.NRGBA{200, 16, 46, 255})

	// Black mark only, on transparency.
	blackOnly := logo(64, 64)
	fill(blackOnly, 8, 20, 56, 44, color.NRGBA{0, 0, 0, 255})

	// The same white text with half coverage: still invisible on white.
	semi := logo(64, 64)
	fill(semi, 8, 20, 56, 44, color.NRGBA{255, 255, 255, 128})

	empty := logo(64, 64)
	// A single low-alpha fleck is not visible ink.
	fill(empty, 0, 0, 2, 2, color.NRGBA{255, 255, 255, 60})

	grey := logo(64, 64)
	fill(grey, 8, 20, 56, 44, color.NRGBA{118, 118, 118, 255})

	// A white wordmark alone is the failure the scorer must prevent.
	whiteText := logo(512, 124)
	fill(whiteText, 40, 30, 470, 94, white)

	// A coloured mark that loses nothing on either tile keeps the white card.
	greenOnly := logo(64, 64)
	fill(greenOnly, 8, 20, 56, 44, color.NRGBA{0, 150, 60, 255})

	cases := []struct {
		name string
		img  *image.NRGBA
		want Background
	}{
		{"sky news pattern", skyNews, Dark},
		{"white wordmark", whiteText, Dark},
		{"red only", redOnly, Light},
		{"green only", greenOnly, Light},
		{"black only", blackOnly, Light},
		{"semi transparent white", semi, Dark},
		{"mid grey", grey, Light},
		{"empty", empty, Dark},
		{"transparent", logo(64, 64), Dark},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Decide(tc.img)
			if ev.Background != tc.want {
				t.Fatalf("background = %s, want %s (ink %.1f, light %.1f, dark %.1f)",
					ev.Background, tc.want, ev.Ink, ev.LightCost, ev.DarkCost)
			}
			if tc.want == Light && ev.LightCost > ev.DarkCost {
				t.Fatalf("light chosen with a worse cost: light %.1f, dark %.1f", ev.LightCost, ev.DarkCost)
			}
			if tc.want == Dark && ev.Ink >= emptyInk && ev.DarkCost >= ev.LightCost {
				t.Fatalf("dark chosen with a worse cost: light %.1f, dark %.1f", ev.LightCost, ev.DarkCost)
			}
		})
	}
}

func TestDecideScoresExposedInkRatherThanPlateInterior(t *testing.T) {
	img := logo(512, 128)
	// A small white wordmark is exposed beside a much larger dark plate.
	// The plate's white lettering already contrasts with the plate itself.
	fill(img, 20, 40, 100, 88, white)
	fill(img, 140, 10, 500, 118, color.NRGBA{20, 25, 55, 255})
	fill(img, 190, 45, 450, 80, white)
	if ev := Decide(img); ev.Background != Dark {
		t.Fatalf("exposed white ink needs dark tile: %+v", ev)
	}
	// The inverse composition must keep a light tile for exposed black ink.
	img = logo(512, 128)
	fill(img, 20, 40, 100, 88, color.NRGBA{0, 0, 0, 255})
	fill(img, 140, 10, 500, 118, white)
	fill(img, 190, 45, 450, 80, color.NRGBA{0, 0, 0, 255})
	if ev := Decide(img); ev.Background != Light {
		t.Fatalf("exposed black ink needs light tile: %+v", ev)
	}
	// A solid dominant mark is not a plate, even with a separate accent.
	img = logo(512, 128)
	fill(img, 20, 40, 100, 88, white)
	fill(img, 140, 10, 500, 118, color.NRGBA{0, 0, 0, 255})
	if ev := Decide(img); ev.Background != Light {
		t.Fatalf("solid black mark needs light tile: %+v", ev)
	}
}

func TestDecideIgnoresTransparentPadding(t *testing.T) {
	small := logo(64, 64)
	fill(small, 16, 16, 48, 48, color.NRGBA{0, 0, 0, 255})
	padded := logo(640, 640)
	fill(padded, 160, 160, 480, 480, color.NRGBA{0, 0, 0, 255})
	a, b := Decide(small), Decide(padded)
	if a.Background != b.Background || math.Abs(a.Ink-b.Ink) > 0.01 {
		t.Fatalf("padding changed the decision: %+v vs %+v", a, b)
	}
}

// oversizedPNG writes a valid PNG header with no image data. DecodeConfig only
// needs IHDR, which lets the guard test reject the dimensions before a full
// decode would allocate the 8000x8000 buffer.
func oversizedPNG(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	chunk := func(kind string, data []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(data)))
		buf.Write(length[:])
		buf.WriteString(kind)
		buf.Write(data)
		crc := crc32.NewIEEE()
		_, _ = crc.Write([]byte(kind))
		_, _ = crc.Write(data)
		var sum [4]byte
		binary.BigEndian.PutUint32(sum[:], crc.Sum32())
		buf.Write(sum[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // RGBA
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return buf.Bytes()
}

func TestDecideBytesRejectsOversizedImages(t *testing.T) {
	data := oversizedPNG(t, 8000, 8000)
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("test fixture is not a readable PNG header: %v", err)
	}
	if _, err := DecideBytes(data); err == nil {
		t.Fatal("oversized image decoded")
	}
}

func TestDecideBytes(t *testing.T) {
	var buf bytes.Buffer
	img := logo(64, 64)
	fill(img, 8, 8, 56, 56, color.NRGBA{0, 0, 0, 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	ev, err := DecideBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Background != Light {
		t.Fatalf("black logo decoded to %s", ev.Background)
	}
	if _, err := DecideBytes([]byte("not an image")); err == nil {
		t.Fatal("decoding junk succeeded")
	}
	// SVG is served by the upstream fetch but cannot be scored; the caller
	// must fall back to dark rather than guess.
	if _, err := DecideBytes([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)); err == nil {
		t.Fatal("decoding SVG succeeded")
	}
}

// Every source pixel contributes to the area average, including a fractional
// edge when the source width is not divisible by the sampling grid.
func TestFractionalAreaWeights(t *testing.T) {
	weights := axisWeights(65, 0)
	if len(weights) != 2 || math.Abs(weights[0].w-64.0/65) > 1e-12 || math.Abs(weights[1].w-1.0/65) > 1e-12 {
		t.Fatalf("weights: %+v", weights)
	}
	for n := 65; n < 200; n++ {
		for i := range sampleSize {
			sum := 0.0
			for _, w := range axisWeights(n, i) {
				sum += w.w
			}
			if math.Abs(sum-1) > 1e-12 {
				t.Fatalf("n=%d i=%d sum=%g", n, i, sum)
			}
		}
	}
}

func BenchmarkDecideLogo(b *testing.B) {
	for _, n := range []int{256, 2048} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			img := image.NewNRGBA(image.Rect(0, 0, n, n))
			fill(img, 0, 0, n, n, color.NRGBA{R: 20, A: 255})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				Decide(img)
			}
		})
	}
}
