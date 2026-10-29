package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// TrackPreferenceSource supplies persisted viewer rules and show choices.
type TrackPreferenceSource interface {
	Select(context.Context, int64, core.RatingKey, core.MediaInfo, string) (trackprefs.Selection, error)
	GetRules(context.Context, int64) (trackprefs.Rules, error)
}

// TrackSelection holds positions in MediaInfo.Streams, not StreamInfo.Index.
// Set flags distinguish an omitted selection from position zero, and an
// explicit subtitle-off selection (-1) from an omitted subtitle choice.
type TrackSelection struct {
	Audio, Subtitle       int
	AudioSet, SubtitleSet bool
}

// TrackPreferences applies persisted choices beneath explicit client choices.
// GetTitle is optional; an episode with no original language uses its show's
// language when it can be loaded, and otherwise retains the empty language.
type TrackPreferences struct {
	Source   TrackPreferenceSource
	GetTitle func(context.Context, core.RatingKey) (*core.Title, error)
}

// Select returns the original selection unchanged if preferences are absent
// or their lookup fails. A lookup error is returned for the caller to log.
func (p TrackPreferences) Select(ctx context.Context, viewer *core.User, key core.RatingKey, info core.MediaInfo, original string, selected TrackSelection) (TrackSelection, error) {
	if p.Source == nil || viewer == nil {
		return selected, nil
	}
	if original == "" && key.Kind() == core.KindEpisode && p.GetTitle != nil {
		if show, err := p.GetTitle(ctx, core.ShowKey(key.TMDB())); err == nil && show != nil {
			original = show.Language
		}
	}
	preferred, err := p.Source.Select(ctx, viewer.ID, key, info, original)
	if err != nil {
		return selected, err
	}
	if !selected.AudioSet {
		selected.Audio, selected.AudioSet = preferred.Audio, true
	}
	if !selected.SubtitleSet {
		selected.Subtitle, selected.SubtitleSet = preferred.Subtitle, true
	}
	return selected, nil
}

// AudioFilters applies the viewer's rules for the selected stream's channel
// count. Missing preferences return no filters; lookup failures are reported.
func (p TrackPreferences) AudioFilters(ctx context.Context, viewer *core.User, info core.MediaInfo, selected TrackSelection) ([]string, error) {
	if p.Source == nil || viewer == nil {
		return nil, nil
	}
	rules, err := p.Source.GetRules(ctx, viewer.ID)
	if err != nil {
		return nil, err
	}
	channels := 0
	if selected.Audio >= 0 && selected.Audio < len(info.Streams) {
		channels = info.Streams[selected.Audio].Channels
	}
	return rules.AudioFilters(channels), nil
}
