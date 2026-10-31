package app

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// newSharedArtwork composes domain services without constructing a protocol server.
func (a *App) newSharedArtwork(ctx context.Context) (*artworkservice.Source, func()) {
	now := parityClock()
	if now == nil {
		now = time.Now
	}
	imageURL := a.tmdb.ImageURLFunc()
	paletteDB := a.db
	if a.spikeLibrary != nil {
		imageURL = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
		paletteDB = nil
	}
	images := artworkservice.NewRemoteCache(128<<20, &http.Client{Timeout: 20 * time.Second})
	var workers sync.WaitGroup
	queue := artworkservice.NewQueue(ctx, &workers, 2, 512)
	collections := a.discoveryPersonal.ReviewCollections(a.collections)
	var collectionArt artworkservice.CollectionArt
	if art, ok := collections.(artworkservice.CollectionArt); ok {
		collectionArt = art
	}
	palettes := artworkservice.NewPaletteCache(paletteDB, queue.Submit, now, a.log, func(ctx context.Context, key core.RatingKey, path string) (artworkservice.Palette, error) {
		return artworkservice.ComputeItemPalette(ctx, images, imageURL, key, path, now(), collectionArt)
	})
	visibility := experienceVisibility(a.safety, a.catalog.StoredTitle, func(ctx context.Context, titles []core.Title) []core.Title {
		if a.availability == nil {
			return titles
		}
		return a.availability.FilterDiscovery(ctx, titles)
	}, func() []*core.PlaySession {
		if a.playback == nil {
			return nil
		}
		return a.playback.Sessions()
	}, []experience.Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}})
	source := &artworkservice.Source{Catalog: a.catalog, Collections: collections, Playlists: a.playlists, Visibility: visibility, Discovery: a.discoveryFeatures, Images: images, ImageURL: imageURL, Palettes: palettes, Now: now, Raster: artwork.NewSVGRasterizer()}
	if a.livetv != nil {
		source.LiveTV = a.livetv
	}
	if a.spikeLibrary != nil {
		source.Catalog, _ = a.spikeLibrary.Catalog().(core.StoredTitleLookup)
		source.Collections = a.spikeLibrary.Collections()
		source.Playlists = a.spikeLibrary.Playlists()
		source.Visibility = experienceVisibility(a.safety, nil, nil, a.spikeLibrary.Playback().Sessions, serverLibraries(a.cfg.Get()))
	}
	if a.episodeorder != nil {
		source.VirtualSeason = a.episodeorder.CachedVirtualSeason
	}
	if a.images != nil {
		source.URLImages = a.images
	}
	variants := artworkservice.NewVariantsQueue(ctx, a.tmdb.CachedArtworkVariants, a.tmdb.ArtworkVariants)
	source.Variants = variants.Get
	metadata := artworkservice.NewMetadataQueue(ctx, a.db, a.users.ActiveUser, source.Read)
	source.Metadata = metadata.Get
	source.Collages = &artworkservice.LibraryCollageCache{Dir: filepath.Join(a.opts.DataDir, "cache", "library-collages")}
	return source, func() { variants.Wait(); metadata.Wait(); workers.Wait() }
}

// serverLibraries preserves the canonical section IDs/order without a Plex server.
func serverLibraries(cfg config.Config) []experience.Library {
	out := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}}
	if !cfg.Spike.Enabled {
		out = append(out, experience.Library{ID: 2, Name: "TV Shows", Type: core.TypeShow})
	}
	return out
}
