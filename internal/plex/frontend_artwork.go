package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/artwork"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// FrontendArtwork is a compatibility adapter for callers already holding a Plex
// server. Shared runtime constructs artworkservice.Source directly.
type FrontendArtwork struct {
	Server        *Server
	Variants      func(*core.Title, string) (*tmdb.Images, bool)
	Metadata      func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image
	VirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	URLImages     interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	Raster *artwork.SVGRasterizer
}

func (a FrontendArtwork) source() artworkservice.Source {
	if a.Server == nil {
		return artworkservice.Source{}
	}
	s := a.Server
	stored, _ := s.d.Catalog.(core.StoredTitleLookup)
	source := artworkservice.Source{Catalog: stored, Collections: s.d.Collections, Playlists: s.d.Playlists, Visibility: s.visibility(), Discovery: s.d.Discovery, Images: s.images, ImageURL: s.d.ImageURL, Palettes: s.ultra, Now: s.now, Variants: a.Variants, Metadata: a.Metadata, VirtualSeason: a.VirtualSeason, URLImages: a.URLImages, Raster: a.Raster}
	if s.d.LiveTV != nil {
		source.LiveTV = s.d.LiveTV
	}
	return source
}
func (a FrontendArtwork) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	if a.Server == nil {
		return nil, false, core.ErrUnauthorized
	}
	return a.source().List(ctx, req)
}
func (a FrontendArtwork) Read(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image, tr frontend.ArtworkTransform) ([]byte, string, error) {
	if a.Server == nil {
		return nil, "", core.ErrUnauthorized
	}
	return a.source().Read(ctx, req, img, tr)
}
func (a FrontendArtwork) Warm(t core.Title, d core.Device) { a.source().Warm(t, d) }
func (a FrontendArtwork) ChannelImage(v *core.Viewer, c livetv.Channel) frontend.Image {
	return a.source().ChannelImage(v, c)
}

type storedPolicyKey struct{}

// withStoredPolicy tells a hub list to read the parent show from the mirror.
// A provider refresh of one series must not hold Recommended.
func withStoredPolicy(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, storedPolicyKey{}, true)
}

// Ordinary Plex routes retain their existing catalogue Get refresh behavior.
// Hub lists use the mirrored show for the playable check.
func (s *Server) artworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if ctx != nil && ctx.Value(storedPolicyKey{}) != nil {
		if cached, ok := s.d.Catalog.(interface {
			CachedGet(context.Context, core.RatingKey) (*core.Title, error)
		}); ok {
			return cached.CachedGet(ctx, key)
		}
	}
	return s.d.Catalog.Get(ctx, key)
}
func renderUltraBlur(c model.UltraBlurColors, tr frontend.ArtworkTransform) ([]byte, string, error) {
	return artworkservice.RenderUltraBlur(artworkservice.Palette{TopLeft: c.TopLeft, TopRight: c.TopRight, BottomLeft: c.BottomLeft, BottomRight: c.BottomRight}, tr)
}

var _ frontend.Artwork = FrontendArtwork{}
