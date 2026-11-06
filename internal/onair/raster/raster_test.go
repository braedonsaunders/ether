package raster_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestGoldenAndDeterminism(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total >= 1_000_000 {
		t.Fatalf("raster fixtures exceed 1 MB budget: %d bytes", total)
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		type entry struct {
			Family   string `json:"family"`
			Moment   string `json:"moment"`
			Bindings any    `json:"bindings"`
		}
		var entries []entry
		for family, moments := range families.Names() {
			for _, moment := range moments {
				d, err := families.Get(family, moment)
				if err != nil {
					t.Fatal(err)
				}
				entries = append(entries, entry{family, d.Moment, families.Defaults(family)})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Family != entries[j].Family {
				return entries[i].Family < entries[j].Family
			}
			return entries[i].Moment < entries[j].Moment
		})
		data, _ := json.MarshalIndent(entries, "", "  ")
		if err := os.WriteFile("testdata/manifest.json", data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	for family, moments := range families.Names() {
		for _, moment := range moments {
			times := []float64{.5, 3, 5.5}
			if family == "signal" {
				times = []float64{1.5}
			}
			for _, at := range times {
				t.Run(fmt.Sprintf("%s-%s-%.1f", family, moment, at), func(t *testing.T) {
					d, e := families.Get(family, moment)
					if e != nil {
						t.Fatal(e)
					}
					b := raster.Bindings{Bindings: families.Defaults(family)}
					im := raster.Render(d, b, at, 480, 270)
					other := raster.Render(d, b, at, 480, 270)
					if !bytes.Equal(im.Pix, other.Pix) {
						t.Fatal("non-deterministic")
					}
					name := filepath.Join("testdata", fmt.Sprintf("%s-%s-%.1f.json", family, moment, at))
					got := compact(im)
					if os.Getenv("UPDATE_GOLDEN") == "1" || (os.Getenv("UPDATE_MISSING_GOLDEN") == "1" && func() bool { _, err := os.Stat(name); return os.IsNotExist(err) }()) {
						data, err := json.Marshal(got)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(name, data, 0644); err != nil {
							t.Fatal(err)
						}
					}
					data, err := os.ReadFile(name)
					if err != nil {
						t.Fatal(err)
					}
					var gold compactGolden
					if err := json.Unmarshal(data, &gold); err != nil {
						t.Fatal(err)
					}
					if err := compareGolden(got, gold); err != nil {
						if os.Getenv("ONAIR_GOLDEN_DEBUG") == "1" {
							dumpGolden(t, im, gold)
						}
						t.Fatal(err)
					}
				})
			}
		}
	}
}
func BenchmarkRender(b *testing.B) {
	for family, moments := range families.Names() {
		for _, moment := range moments {
			for _, w := range []int{1920, 960} {
				b.Run(fmt.Sprintf("%s/%s/%dx%d", family, moment, w, w*9/16), func(b *testing.B) {
					d, e := families.Get(family, moment)
					if e != nil {
						b.Fatal(e)
					}
					bindings := raster.Bindings{Bindings: families.Defaults(family)}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						raster.Render(d, bindings, 4.8, w, w*9/16)
					}
				})
			}
		}
	}
}

// TestParity runs unchanged natively and under GOOS=js GOARCH=wasm, using the
// same scene/bindings/t/dimensions as the Chrome loader smoke measurement.
func TestParity(t *testing.T) {
	d, e := families.Get("late-night", "ident")
	if e != nil {
		t.Fatal(e)
	}
	im := raster.Render(d, raster.Bindings{Bindings: families.Defaults("late-night")}, 3, 960, 540)
	got := fmt.Sprintf("%x", sha256.Sum256(im.Pix))
	if got != "79a1ac849149cd8c5290d3f5be1cfd4e6aac3fe6a95b30f751873e3af2789c5f" {
		t.Fatalf("parity hash: %s", got)
	}
}
