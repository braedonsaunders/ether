// Phase 2 sample harness: renders real posters through the production cache/compositor.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/braedonsaunders/ether/internal/overlays"
	xdraw "golang.org/x/image/draw"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", ".", "sample PNG directory")
	dark := flag.String("dark", "", "dark cached poster file (read only)")
	darkAlt := flag.String("dark-alt", "", "second dark cached poster file")
	light := flag.String("light", "", "light cached poster file (read only)")
	lightAlt := flag.String("light-alt", "", "second light cached poster file")
	flag.Parse()
	renderer := overlays.New(nil, overlays.Options{MemoryCacheBytes: -1})
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

			for _, wide := range []bool{true, false} {
				encoded, _, err := renderer.Ranked(context.Background(), raw, rank, wide)
				must(err)
				card, _, err := image.Decode(bytes.NewReader(encoded))
				must(err)
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
