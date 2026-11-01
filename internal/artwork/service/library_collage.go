package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const libraryCollageSize = 640

// LibraryCollageCache stores generated library-view JPEGs. The file name is
// the identity of the view and the child posters, so a content change stops
// serving the previous image.
type LibraryCollageCache struct {
	Dir string
	mu  sync.Mutex
}

type libraryCollageMeta struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	BlurHash string `json:"blurHash"`
}

type libraryPoster struct {
	key  core.RatingKey
	path string
}

// LibraryCollage builds a square tile of the first few visible child posters.
// It is a generated image, not one child's poster. An empty selection is an error.
func (a Source) LibraryCollage(ctx context.Context, viewer *core.User, _ core.Device, viewID string, children []core.Title) (frontend.Image, error) {
	if a.Collages == nil || a.Collages.Dir == "" || viewer == nil || viewID == "" {
		return frontend.Image{}, core.ErrNotAvailable
	}
	selected := a.libraryPosters(ctx, viewer, children)
	if len(selected) == 0 {
		return frontend.Image{}, core.ErrNotFound
	}
	sum := sha256.Sum256([]byte(libraryCollageIdentity(viewID, selected)))
	id := hex.EncodeToString(sum[:])
	ref := frontend.LibraryCollagePrefix + id
	a.Collages.mu.Lock()
	defer a.Collages.mu.Unlock()
	if img, ok := a.Collages.cached(ref); ok {
		img.Subject = viewID
		return img, nil
	}
	posters := make([]image.Image, 0, len(selected))
	for _, poster := range selected {
		decoded, err := a.decodePoster(ctx, poster.path)
		if err != nil {
			return frontend.Image{}, err
		}
		posters = append(posters, decoded)
	}
	tile := drawLibraryCollage(posters)
	hash, err := artwork.BlurHash(tile, 4, 3)
	if err != nil {
		return frontend.Image{}, err
	}
	if err = a.Collages.store(id, tile, hash); err != nil {
		return frontend.Image{}, err
	}
	return libraryCollageImage(ref, viewID, hash), nil
}

func (a Source) libraryPosters(ctx context.Context, viewer *core.User, children []core.Title) []libraryPoster {
	out := make([]libraryPoster, 0, 4)
	for i := range children {
		title := children[i]
		if !artwork.ValidPath(title.PosterPath) || !a.visible(ctx, viewer, &title) {
			continue
		}
		out = append(out, libraryPoster{key: title.Key, path: title.PosterPath})
		if len(out) == 4 {
			break
		}
	}
	return out
}

func libraryCollageIdentity(viewID string, posters []libraryPoster) string {
	var b strings.Builder
	b.WriteString("library-collage-v1\n")
	b.WriteString(viewID)
	for _, poster := range posters {
		b.WriteByte('\n')
		b.WriteString(poster.key.String())
		b.WriteByte('\t')
		b.WriteString(poster.path)
	}
	return b.String()
}

func (c *LibraryCollageCache) cached(ref string) (frontend.Image, bool) {
	file, ok := libraryCollageFile(c.Dir, ref)
	if !ok {
		return frontend.Image{}, false
	}
	raw, err := os.ReadFile(file + ".json")
	if err != nil {
		return frontend.Image{}, false
	}
	var meta libraryCollageMeta
	if json.Unmarshal(raw, &meta) != nil || meta.Width <= 0 || meta.Height <= 0 || meta.BlurHash == "" {
		return frontend.Image{}, false
	}
	if _, err = os.Stat(file + ".jpg"); err != nil {
		return frontend.Image{}, false
	}
	return libraryCollageImage(ref, "", meta.BlurHash), true
}

func libraryCollageImage(ref, viewID, hash string) frontend.Image {
	return frontend.Image{
		Kind: frontend.ImagePoster, Path: ref, Tag: ref, Subject: viewID,
		Width: libraryCollageSize, Height: libraryCollageSize, BlurHash: hash,
	}
}

func (c *LibraryCollageCache) store(id string, tile image.Image, hash string) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(c.Dir, id)
	jpegFile, err := os.OpenFile(file+".jpg", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = jpeg.Encode(jpegFile, tile, &jpeg.Options{Quality: 85})
	closeErr := jpegFile.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	meta, err := json.Marshal(libraryCollageMeta{Width: libraryCollageSize, Height: libraryCollageSize, BlurHash: hash})
	if err != nil {
		return err
	}
	return os.WriteFile(file+".json", meta, 0o600)
}

func (a Source) readLibraryCollage(ref string) ([]byte, error) {
	if a.Collages == nil || a.Collages.Dir == "" {
		return nil, core.ErrNotFound
	}
	file, ok := libraryCollageFile(a.Collages.Dir, ref)
	if !ok {
		return nil, core.ErrNotFound
	}
	data, err := os.ReadFile(file + ".jpg")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, core.ErrNotFound
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, core.ErrNotFound
	}
	return data, nil
}

func libraryCollageFile(dir, ref string) (string, bool) {
	id := strings.TrimPrefix(ref, frontend.LibraryCollagePrefix)
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", false
	}
	return filepath.Join(dir, id), true
}

func (a Source) decodePoster(ctx context.Context, path string) (image.Image, error) {
	data, _, err := a.remote(ctx, path, 500)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func drawLibraryCollage(posters []image.Image) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, libraryCollageSize, libraryCollageSize))
	fill := color.NRGBA{R: 24, G: 24, B: 28, A: 255}
	for i := range dst.Pix {
		switch i % 4 {
		case 0:
			dst.Pix[i] = fill.R
		case 1:
			dst.Pix[i] = fill.G
		case 2:
			dst.Pix[i] = fill.B
		default:
			dst.Pix[i] = fill.A
		}
	}
	cell := libraryCollageSize / 2
	for n, poster := range posters {
		if n >= 4 || poster == nil {
			break
		}
		x := (n % 2) * cell
		y := (n / 2) * cell
		coverInto(dst, image.Rect(x, y, x+cell, y+cell), poster)
	}
	return dst
}

func coverInto(dst *image.NRGBA, cell image.Rectangle, src image.Image) {
	bounds := src.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	cw, ch := cell.Dx(), cell.Dy()
	if sw <= 0 || sh <= 0 || cw <= 0 || ch <= 0 {
		return
	}
	// Scale so the source covers the cell, then center-crop.
	scaleW := cw * sh
	scaleH := ch * sw
	cropX, cropY := 0, 0
	usedW, usedH := sw, sh
	if scaleW > scaleH {
		usedH = scaleH / cw
		if usedH < 1 {
			usedH = 1
		}
		cropY = (sh - usedH) / 2
	} else if scaleH > scaleW {
		usedW = scaleW / ch
		if usedW < 1 {
			usedW = 1
		}
		cropX = (sw - usedW) / 2
	}
	for y := 0; y < ch; y++ {
		sy := bounds.Min.Y + cropY + y*usedH/ch
		if sy >= bounds.Max.Y {
			sy = bounds.Max.Y - 1
		}
		for x := 0; x < cw; x++ {
			sx := bounds.Min.X + cropX + x*usedW/cw
			if sx >= bounds.Max.X {
				sx = bounds.Max.X - 1
			}
			dst.SetNRGBA(cell.Min.X+x, cell.Min.Y+y, color.NRGBAModel.Convert(src.At(sx, sy)).(color.NRGBA))
		}
	}
}
