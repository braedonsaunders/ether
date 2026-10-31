package app

import (
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/llm"
)

// collectionOptions wires the collection engine: TMDB for sources and
// lookups, the image store and poster renderer, catalogue/Trakt settings,
// and an LLM client for the natural-language generator that follows
// config edits.
func (a *App) collectionOptions(cfg config.Config) []collections.Option {
	dataDir := a.opts.DataDir
	a.images = artwork.NewImages(artwork.ImagesOptions{
		Dir:      filepath.Join(dataDir, "cache", "images"),
		ImageURL: a.tmdb.ImageURLFunc(),
		Logger:   a.log.With("component", "images"),
		Posters:  func() config.Posters { return a.cfg.Read().Posters },
	})
	renderer := artwork.NewRenderer(a.images, filepath.Join(dataDir, "cache", "posters"), a.log.With("component", "artwork"))
	chat := llm.NewClient(llm.ClientOptions{Config: cfg.LLM, Logger: a.log.With("component", "llm")})
	a.cfg.Subscribe(func(c config.Config) { chat.UpdateConfig(c.LLM) })
	return append([]collections.Option{
		collections.WithTMDB(a.tmdb),
		collections.WithArtwork(renderer, a.images),
		collections.WithCatalogConfig(func() config.Catalog { return a.cfg.Read().Catalog }),
		collections.WithTraktConfig(func() config.Trakt { return a.cfg.Read().Trakt }),
		collections.WithLLM(chat),
		collections.WithUserAgent("Ether/" + a.opts.Version + " (+https://github.com/braedonsaunders/ether)"),
	}, parityCollectionOptions()...)
}
