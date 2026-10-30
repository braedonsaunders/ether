package plex

import (
	"context"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"image"
)

const (
	ultraBlurWorkers  = 2
	ultraBlurQueueLen = 512
)

func wirePalette(p artworkservice.Palette) model.UltraBlurColors {
	return model.UltraBlurColors{TopLeft: p.TopLeft, TopRight: p.TopRight, BottomLeft: p.BottomLeft, BottomRight: p.BottomRight}
}
func ultraBlurFromImage(img image.Image) (model.UltraBlurColors, error) {
	p, err := artworkservice.PaletteFromImage(img)
	return wirePalette(p), err
}
func (s *Server) ultraBlurFor(key core.RatingKey, backdrop string) *model.UltraBlurColors {
	if s.ultra == nil || backdrop == "" {
		return nil
	}
	p := s.ultra.Lookup(key, backdrop)
	if p == nil {
		return nil
	}
	result := wirePalette(*p)
	return &result
}
func (s *Server) preloadUltraBlur(ctx context.Context, titles []core.Title) {
	if s.ultra == nil {
		return
	}
	wants := make([]artworkservice.PaletteWant, 0, len(titles))
	for _, t := range titles {
		if ultraBlurType(t.Type) {
			wants = append(wants, artworkservice.PaletteWant{Key: t.Key, Path: t.BackdropPath})
		}
	}
	s.ultra.Preload(ctx, wants)
}
func ultraBlurType(t core.MediaType) bool { return t == core.TypeMovie || t == core.TypeShow }
