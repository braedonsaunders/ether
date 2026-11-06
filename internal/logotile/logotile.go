// Package logotile decides which tile background a channel logo stays legible
// on. The console draws a live TV logo on a white or a dark (zinc-900) tile;
// ink that lands close to the tile colour disappears.
//
// The scorer samples the image into a 64x64 RGBA grid, composites every
// transparent sample over each candidate tile, and sums the alpha-weighted
// contrast deficit:
//
//	cost(tile) = Σ alpha · max(0, MinContrast − contrast(ink, tile))
//
// A large rectangular plate with contrasting internal lettering can dominate
// that sum. When separate ink accompanies such a plate, score that ink instead.
package logotile

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"

	_ "golang.org/x/image/webp"
)

// MinContrast is the contrast ratio below which ink dissolves into the tile.
// WCAG 1.4.11 asks graphical objects for 3:1; 2.5 sits just below that, so
// only ink that is already fading counts as lost.
const MinContrast = 2.5

// sampleSize is the grid the scorer samples before weighing ink coverage.
const sampleSize = 64

// emptyInk is the alpha-weighted area, in sampled pixels, below which an image
// carries no visible ink. Any real mark covers far more than this; the bound
// only catches blank or fully transparent images.
const emptyInk = 1

// maxPixels bounds an untrusted logo before a full decode. Logos are provider
// artwork fetched from arbitrary hosts, so the header is read first and a
// decompression bomb is rejected instead of allocating its RGBA buffer.
const maxPixels = 16 << 20

// Background is the tile colour a logo should be drawn on.
type Background string

// Header carries the decision to the console on the logo response.
const Header = "X-Ether-Logo-Background"

const (
	// Light is the white tile.
	Light Background = "light"
	// Dark is the zinc-900 tile, and the default for unreadable images.
	Dark Background = "dark"
)

// Evidence is a decision plus the numbers behind it, so callers and tests can
// explain a choice instead of re-deriving it.
type Evidence struct {
	Background Background
	// Ink is the alpha-weighted visible area in sampled pixels (0..4096).
	Ink float64
	// LightCost and DarkCost are the scored contrast deficits; lower wins.
	LightCost float64
	DarkCost  float64
}

// sample is one cell of the 64x64 grid: unassociated colour in 0..255 and
// coverage in 0..1.
type samplePixel struct {
	r, g, b, a float64
}

// Decide scores img against the two tiles.
func Decide(img image.Image) Evidence {
	px := sample(img)
	var ink float64
	for _, p := range px {
		ink += p.a
	}
	ev := Evidence{Ink: ink}
	plate := dominantPlate(px)
	ev.LightCost = cost(px, plate, 255, 255, 255)
	ev.DarkCost = cost(px, plate, 24, 24, 27)
	switch {
	case ink < emptyInk:
		// Nothing to read: keep the dark default instead of a white card.
		ev.Background = Dark
	case ev.LightCost <= ev.DarkCost:
		// White keeps ties: red, green and mid-grey ink lose nothing on either
		// tile, and a white card looks intentional for those marks.
		ev.Background = Light
	default:
		ev.Background = Dark
	}
	return ev
}

// DecideBytes decodes an encoded image (PNG, JPEG, GIF or WebP) and decides.
func DecideBytes(data []byte) (Evidence, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Evidence{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPixels/cfg.Height {
		return Evidence{}, fmt.Errorf("logotile: image is %dx%d", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Evidence{}, err
	}
	return Decide(img), nil
}

// cost sums the alpha-weighted contrast deficit of every sample against a tile
// colour. Coverage weights matter: anti-aliased edges have low alpha and low
// composited contrast, so they cannot dominate the score.
func cost(px []samplePixel, plate []bool, tr, tg, tb float64) float64 {
	tileLum := luminance(tr, tg, tb)
	total := 0.0
	for i, p := range px {
		if p.a == 0 || (plate != nil && plate[i]) {
			continue
		}
		r := p.a*p.r + (1-p.a)*tr
		g := p.a*p.g + (1-p.a)*tg
		b := p.a*p.b + (1-p.a)*tb
		l := luminance(r, g, b)
		contrast := (math.Max(l, tileLum) + 0.05) / (math.Min(l, tileLum) + 0.05)
		if deficit := MinContrast - contrast; deficit > 0 {
			total += p.a * deficit
		}
	}
	return total
}

// dominantPlate finds a nearly solid rectangle with contrasting internal ink,
// much larger than the other disconnected ink. Its contents contrast already.
// shortcut: touching or nonrectangular plates are scored normally; revisit if
// representative artwork shows those shapes lose separate ink.
func dominantPlate(px []samplePixel) []bool {
	seen := make([]bool, len(px))
	var largest []int
	var largestRect int
	total := 0
	for i, p := range px {
		if p.a < 0.5 || seen[i] {
			continue
		}
		seen[i] = true
		component := []int{i}
		minX, maxX, minY, maxY := i%sampleSize, i%sampleSize, i/sampleSize, i/sampleSize
		for n := 0; n < len(component); n++ {
			pos := component[n]
			x, y := pos%sampleSize, pos/sampleSize
			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= sampleSize || ny < 0 || ny >= sampleSize {
						continue
					}
					j := ny*sampleSize + nx
					if !seen[j] && px[j].a >= 0.5 {
						seen[j] = true
						component = append(component, j)
					}
				}
			}
		}
		total += len(component)
		if len(component) > len(largest) {
			largest = component
			largestRect = (maxX - minX + 1) * (maxY - minY + 1)
		}
	}
	other := total - len(largest)
	if other < 4 || len(largest) < 4*other || len(largest)*10 < largestRect*9 {
		return nil
	}
	var dark, light int
	for _, i := range largest {
		p := px[i]
		l := luminance(p.r, p.g, p.b)
		if l < 0.18 {
			dark++
		} else if l > 0.7 {
			light++
		}
	}
	if dark*50 < len(largest) || light*50 < len(largest) {
		return nil
	}
	plate := make([]bool, len(px))
	for _, i := range largest {
		plate[i] = true
	}
	return plate
}

// sample resamples the source into a 64x64 grid in premultiplied space and
// unassociates the colour, mirroring a canvas drawImage(image, 0, 0, 64, 64):
// an area average where the source is larger than the grid, bilinear where it
// is smaller, and transparent samples contribute no colour.
func sample(img image.Image) []samplePixel {
	b := img.Bounds()
	// Standard decoders expose value-returning RGBA64At. Avoid boxing a
	// colour interface (and allocating) for every source pixel.
	rgbaImage, _ := img.(image.RGBA64Image)
	w, h := b.Dx(), b.Dy()
	out := make([]samplePixel, sampleSize*sampleSize)
	if w <= 0 || h <= 0 {
		return out
	}
	xs := make([][]weight, sampleSize)
	ys := make([][]weight, sampleSize)
	for i := range sampleSize {
		xs[i] = axisWeights(w, i)
		ys[i] = axisWeights(h, i)
	}
	for dy := range sampleSize {
		for dx := range sampleSize {
			var pr, pg, pb, pa float64
			for _, yw := range ys[dy] {
				for _, xw := range xs[dx] {
					var r, g, bl, a uint32
					if rgbaImage != nil {
						pixel := rgbaImage.RGBA64At(b.Min.X+xw.i, b.Min.Y+yw.i)
						r, g, bl, a = pixel.RGBA()
					} else {
						r, g, bl, a = img.At(b.Min.X+xw.i, b.Min.Y+yw.i).RGBA()
					}
					wgt := xw.w * yw.w
					pr += wgt * float64(r)
					pg += wgt * float64(g)
					pb += wgt * float64(bl)
					pa += wgt * float64(a)
				}
			}
			if pa == 0 {
				continue
			}
			// pr and pa share the 16-bit alpha scale, so pr/pa is the
			// unassociated colour in 0..1.
			out[dy*sampleSize+dx] = samplePixel{
				r: 255 * pr / pa,
				g: 255 * pg / pa,
				b: 255 * pb / pa,
				a: pa / 65535,
			}
		}
	}
	return out
}

// weight is one source index and its share of a destination cell.
type weight struct {
	i int
	w float64
}

// axisWeights maps destination cell i of a sampleSize grid onto a source axis
// of length n: a box average when shrinking, two bilinear taps when enlarging.
func axisWeights(n, i int) []weight {
	start := float64(i) * float64(n) / sampleSize
	end := float64(i+1) * float64(n) / sampleSize
	if end-start >= 1 {
		first, last := int(math.Floor(start)), int(math.Ceil(end))-1
		out := make([]weight, 0, last-first+1)
		for j := first; j <= last; j++ {
			coverage := math.Min(end, float64(j+1)) - math.Max(start, float64(j))
			out = append(out, weight{j, coverage / (end - start)})
		}
		return out
	}
	center := (start+end)/2 - 0.5
	lo := int(math.Floor(center))
	frac := center - float64(lo)
	if lo < 0 {
		return []weight{{0, 1}}
	}
	if lo >= n-1 {
		return []weight{{n - 1, 1}}
	}
	return []weight{{lo, 1 - frac}, {lo + 1, frac}}
}

// luminance is WCAG relative luminance for sRGB channels in 0..255.
func luminance(r, g, b float64) float64 {
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
}

func linear(c float64) float64 {
	v := c / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}
