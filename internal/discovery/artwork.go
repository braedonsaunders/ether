package discovery

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ArtworkSelection contains shared selection facts; adapters own image URLs.
type ArtworkSelection struct {
	Title                                       *core.Title
	Enabled                                     bool
	PosterVersion, BackdropVersion, LogoVersion string
}

// SelectArtwork never waits for localization or remote catalogue/image work.
func (d *Features) SelectArtwork(ctx context.Context, viewer *core.User, t *core.Title, lang string) ArtworkSelection {
	out := ArtworkSelection{Title: t}
	if d == nil || d.Overlays == nil || viewer == nil || t == nil {
		return out
	}
	cfg, _ := d.Overlays.Settings(ctx)
	out.Enabled = cfg.Enabled
	if cfg.Enabled && cfg.Localize && d.Localize != nil && lang != "" {
		if localized := d.CachedLocalized(t, lang); localized != nil {
			out.Title = localized
		}
	}
	if (cfg.Enabled && out.Title.PosterPath != "") || (d.Overlays.CustomPostersEnabled() && (out.Title.Type == core.TypeMovie || out.Title.Type == core.TypeShow)) {
		out.PosterVersion = d.Overlays.Version(ctx, viewer, out.Title, lang)
	}
	if cfg.Enabled {
		_, backdrop := d.Overlays.Paths(ctx, viewer, out.Title, lang)
		if backdrop != "" && backdrop != t.BackdropPath {
			out.BackdropVersion = d.Overlays.Version(ctx, viewer, out.Title, lang)
		}
		if out.Title.LogoPath != "" {
			out.LogoVersion = d.Overlays.Version(ctx, viewer, out.Title, lang)
		}
	}
	return out
}

// PrepareArtwork performs bounded localization for an authorized image read.
// Versioning precedes rendering, so protocol adapters can retain conditional GET.
func (d *Features) PrepareArtwork(ctx context.Context, viewer *core.User, t *core.Title, lang string) (*core.Title, string, error) {
	if d == nil || d.Overlays == nil {
		return nil, "", core.ErrNotFound
	}
	cfg, _ := d.Overlays.Settings(ctx)
	if cfg.Enabled && cfg.Localize && d.Localize != nil {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		v, err := d.Localize(bounded, t, lang)
		cancel()
		if err == nil && v != nil {
			t = v
		}
	}
	return t, d.Overlays.Version(ctx, viewer, t, lang), nil
}

func (d *Features) RenderArtwork(ctx context.Context, viewer *core.User, t *core.Title, kind, lang string) ([]byte, string, error) {
	if d == nil || d.Overlays == nil {
		return nil, "", core.ErrNotFound
	}
	switch kind {
	case "backdrop":
		return d.Overlays.Backdrop(ctx, viewer, t, lang)
	case "logo":
		return d.Overlays.Logo(ctx, t)
	default:
		return d.Overlays.Poster(ctx, viewer, t, lang)
	}
}
