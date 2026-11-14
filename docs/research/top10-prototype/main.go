// Phase 1 visual experiment only. No server route or catalogue work.
// Run from the repository root: go run ./docs/research/top10-prototype -out <directory>
package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strconv"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed Anton-Regular.ttf
var displayFont []byte

func main() {
	out := flag.String("out", ".", "sample PNG directory")
	dark := flag.String("dark", "", "dark cached poster file (read only)")
	darkAlt := flag.String("dark-alt", "", "second dark cached poster file")
	light := flag.String("light", "", "light cached poster file (read only)")
	lightAlt := flag.String("light-alt", "", "second light cached poster file")
	flag.Parse()
	must(os.MkdirAll(*out, 0755))
	sheet := image.NewRGBA(image.Rect(0, 0, 1440, 1290))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{22, 23, 27, 255}), image.Point{}, draw.Src)
	for themeIndex, theme := range []string{"dark", "light"} {
		paths := []string{*dark, *darkAlt, *dark}
		if theme == "light" {
			paths = []string{*light, *lightAlt, *light}
		}
		for index, rank := range []int{1, 7, 10} {
			raw, err := os.ReadFile(paths[index])
			must(err)
			poster, _, err := image.Decode(bytes.NewReader(raw))
			must(err)
			for _, wide := range []bool{true, false} {
				card := composite(poster, rank, wide)
				shape := "portrait"
				if wide {
					shape = "wide"
				}
				name := fmt.Sprintf("top10-%s-%s-%02d.png", shape, theme, rank)
				save(filepath.Join(*out, name), card)
				var rect image.Rectangle
				if wide {
					rect = image.Rect(index*480, themeIndex*645, index*480+480, themeIndex*645+270)
				} else {
					rect = image.Rect(index*480+140, themeIndex*645+285, index*480+340, themeIndex*645+585)
				}
				xdraw.CatmullRom.Scale(sheet, rect, card, card.Bounds(), draw.Over, nil)
			}
		}
	}
	save(filepath.Join(*out, "top10-contact-sheet.png"), sheet)
}

// Same image/draw, CatmullRom and per-render opentype faces as Ether's
// artwork/overlay pipeline. This experiment never reads a catalogue or provider.
func composite(poster image.Image, rank int, wide bool) *image.RGBA {
	w, h := 500, 750
	if wide {
		w, h = 960, 540
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// Transparent PNG gutters; opaque clients can flatten onto #0e0f13.
	var rect image.Rectangle
	if wide {
		rect = image.Rect(600, 0, 960, 540)
	} else {
		rect = image.Rect(110, 165, 500, 750)
	}
	if wide {
		number(dst, rank, image.Rect(20, 20, 650, 540))
	} else {
		number(dst, rank, image.Rect(4, 450, 139, 750))
	}
	// Soft shadow belongs behind the poster, not on its title treatment.
	for x := max(0, rect.Min.X-24); x < rect.Min.X; x++ {
		alpha := uint8(85 * float64(x-(rect.Min.X-24)) / 24)
		draw.Draw(dst, image.Rect(x, rect.Min.Y, rect.Min.X, rect.Max.Y), image.NewUniform(color.NRGBA{A: alpha}), image.Point{}, draw.Over)
	}
	xdraw.CatmullRom.Scale(dst, rect, poster, poster.Bounds(), draw.Over, nil)
	return dst
}

// Heavy display numerals, unslashed zero, thick outer stroke and shaded fill.
// Right alignment makes the poster occlude roughly 10% of the numeral edge.
func number(dst *image.RGBA, rank int, area image.Rectangle) {
	parsed, err := opentype.Parse(displayFont)
	must(err)
	text := strconv.Itoa(rank)
	size := float64(area.Dy()) * 1.4
	var face font.Face
	var bounds fixed.Rectangle26_6
	for {
		face, err = opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		must(err)
		bounds, _ = font.BoundString(face, text)
		if (bounds.Max.X-bounds.Min.X).Ceil() <= area.Dx()-12 && (bounds.Max.Y-bounds.Min.Y).Ceil() <= area.Dy()-12 {
			break
		}
		must(face.Close())
		size *= .97
	}
	defer face.Close()
	x := area.Max.X - 8 - bounds.Max.X.Ceil()
	y := area.Max.Y - 8 - bounds.Max.Y.Ceil()
	radius := max(4, dst.Bounds().Dx()/90)
	mask := image.NewAlpha(dst.Bounds())
	d := font.Drawer{Dst: mask, Face: face, Src: image.White}
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= radius*radius {
				d.Dot = fixed.P(x+dx, y+dy)
				d.DrawString(text)
			}
		}
	}
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(color.RGBA{205, 209, 220, 255}), image.Point{}, mask, image.Point{}, draw.Over)
	fill := image.NewRGBA(dst.Bounds())
	for y := 0; y < dst.Bounds().Dy(); y++ {
		v := uint8(12 + 27*(1-float64(y)/float64(dst.Bounds().Dy())))
		draw.Draw(fill, image.Rect(0, y, dst.Bounds().Dx(), y+1), image.NewUniform(color.RGBA{v, v + 2, v + 5, 255}), image.Point{}, draw.Src)
	}
	inner := image.NewAlpha(dst.Bounds())
	d.Dst = inner
	d.Dot = fixed.P(x, y)
	d.DrawString(text)
	draw.DrawMask(dst, dst.Bounds(), fill, image.Point{}, inner, image.Point{}, draw.Over)
}
func save(path string, img image.Image) {
	f, e := os.Create(path)
	must(e)
	must(png.Encode(f, img))
	must(f.Close())
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
