package service

import (
	"bytes"
	"go/parser"
	"go/token"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestNeutralImplementationDoesNotImportPlex(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "github.com/braedonsaunders/ether/internal/plex" || strings.HasPrefix(name, "github.com/braedonsaunders/ether/internal/plex/") {
				t.Errorf("%s imports protocol implementation %s", entry.Name(), name)
			}
		}
	}
}

func TestNeutralPaletteRenderingAndTransform(t *testing.T) {
	// This test constructs no protocol server, wire models, or application runtime.
	data, contentType, err := RenderUltraBlur(Palette{TopLeft: "ff0000", TopRight: "00ff00", BottomLeft: "0000ff", BottomRight: "ffffff"}, frontend.ArtworkTransform{Width: 2, Height: 2, Format: "png"})
	if err != nil || contentType != "image/png" {
		t.Fatalf("palette: %s %v", contentType, err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		x, y    int
		r, g, b uint32
	}{{0, 0, 65535, 0, 0}, {1, 0, 0, 65535, 0}, {0, 1, 0, 0, 65535}, {1, 1, 65535, 65535, 65535}} {
		r, g, b, _ := img.At(tc.x, tc.y).RGBA()
		if r != tc.r || g != tc.g || b != tc.b {
			t.Fatalf("wrong palette corner: %d,%d", tc.x, tc.y)
		}
	}
	passthrough, ct, err := Transform(data, contentType, frontend.ArtworkTransform{Width: 300})
	if err != nil || ct != contentType || !bytes.Equal(passthrough, data) {
		t.Fatal("width-only transform changed source bytes")
	}
	transformed, ct, err := Transform(data, contentType, frontend.ArtworkTransform{Width: 30, Height: 20, Blur: true, Format: "png"})
	if err != nil || ct != "image/png" {
		t.Fatalf("transform: %s %v", ct, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(transformed))
	if err != nil || cfg.Width != 30 || cfg.Height != 20 {
		t.Fatalf("dimensions: %+v %v", cfg, err)
	}
}
