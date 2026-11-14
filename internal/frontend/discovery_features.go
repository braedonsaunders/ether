package frontend

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// DiscoveryArtworkSelection contains shared selection facts; adapters own image URLs.
type DiscoveryArtworkSelection struct {
	Title                                       *core.Title
	Enabled                                     bool
	PosterVersion, BackdropVersion, LogoVersion string
}

// DiscoveryRatingBadge is a normalized external rating badge for a title.
type DiscoveryRatingBadge struct {
	Source string
	Value  float64
	Image  string
}

// DiscoveryRatingsSettings exposes active rating provider preferences.
type DiscoveryRatingsSettings struct {
	Enabled          bool
	Critic, Audience string
	Missing          string
}

type DiscoveryWatchlist interface {
	Hubs(ctx context.Context, viewer *core.User, limit int) ([]core.Hub, error)
	RecapsEnabled(ctx context.Context) bool
	NaturalSearchEnabled(ctx context.Context) bool
}

type DiscoveryRatings interface {
	Cached(ctx context.Context, t *core.Title) ([]DiscoveryRatingBadge, error)
	Settings(ctx context.Context) (DiscoveryRatingsSettings, error)
	Queue(t core.Title)
	WithSettingsSnapshot(ctx context.Context) context.Context
}

type DiscoveryOverlays interface {
	CustomPostersEnabled() bool
	// Ranked decorates already authorized source bytes with a Top 10 rank.
	Ranked(ctx context.Context, source []byte, rank int, wide bool) ([]byte, string, error)
	Paths(ctx context.Context, viewer *core.User, t *core.Title, lang string) (string, string)
	WithSettingsSnapshot(ctx context.Context) context.Context
	WithArtworkPage(ctx context.Context, keys []core.RatingKey) (context.Context, error)
}

type DiscoveryPersonal interface {
	Hubs(ctx context.Context, viewer *core.User, limit int) ([]core.Hub, error)
	RemoveContinue(ctx context.Context, uid int64, key core.RatingKey) error
	RestoreContinue(ctx context.Context, uid int64, key core.RatingKey) error
	ContinueAllowed(ctx context.Context, uid int64, t *core.Title) bool
	FilterContinue(ctx context.Context, viewer *core.User, titles []core.Title) []core.Title
}

type DiscoveryLLM interface {
	NaturalSearch(ctx context.Context, viewer *core.User, query string, limit int) ([]core.Title, error)
}

// DiscoveryFeatures exposes optional discovery capabilities to frontends and
// shared presentation services without importing domain implementations.
type DiscoveryFeatures struct {
	Watchlist DiscoveryWatchlist
	Ratings   DiscoveryRatings
	Overlays  DiscoveryOverlays
	Personal  DiscoveryPersonal
	LLM       DiscoveryLLM
	// OverviewTranslate is optional. detail is true only when a viewer opened
	// the item. A library row passes false and must not call a model.
	OverviewTranslate func(ctx context.Context, t *core.Title, lang string, detail bool) (summary, tagline string, ok bool)
	SelectArtworkFn   func(ctx context.Context, viewer *core.User, t *core.Title, lang string) DiscoveryArtworkSelection
	PrepareArtworkFn  func(ctx context.Context, viewer *core.User, t *core.Title, lang string) (*core.Title, string, error)
	RenderArtworkFn   func(ctx context.Context, viewer *core.User, t *core.Title, kind, lang string) ([]byte, string, error)
	CachedRecapFn     func(viewer *core.User, t *core.Title, state core.State, now time.Time) string
}

func (d *DiscoveryFeatures) SelectArtwork(ctx context.Context, viewer *core.User, t *core.Title, lang string) DiscoveryArtworkSelection {
	if d == nil || d.SelectArtworkFn == nil {
		return DiscoveryArtworkSelection{Title: t}
	}
	return d.SelectArtworkFn(ctx, viewer, t, lang)
}

func (d *DiscoveryFeatures) PrepareArtwork(ctx context.Context, viewer *core.User, t *core.Title, lang string) (*core.Title, string, error) {
	if d == nil || d.PrepareArtworkFn == nil {
		return nil, "", core.ErrNotFound
	}
	return d.PrepareArtworkFn(ctx, viewer, t, lang)
}

func (d *DiscoveryFeatures) RenderArtwork(ctx context.Context, viewer *core.User, t *core.Title, kind, lang string) ([]byte, string, error) {
	if d == nil || d.RenderArtworkFn == nil {
		return nil, "", core.ErrNotFound
	}
	return d.RenderArtworkFn(ctx, viewer, t, kind, lang)
}

func (d *DiscoveryFeatures) CachedRecap(viewer *core.User, t *core.Title, state core.State, now time.Time) string {
	if d == nil || d.CachedRecapFn == nil {
		return ""
	}
	return d.CachedRecapFn(viewer, t, state, now)
}
