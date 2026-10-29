package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// selectBrokerTracks translates wire stream indexes to the experience
// service's slice positions, then translates its result back for the encoder.
func selectBrokerTracks(ctx context.Context, prefs experience.TrackPreferences, viewer *core.Viewer, title *core.Title, info core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool, onError func(error)) (core.DecideOptions, error) {
	position := func(kind string, index int) (int, bool) {
		for i, stream := range info.Streams {
			if stream.Type == kind && stream.Index == index {
				return i, true
			}
		}
		return -1, false
	}
	selection := experience.TrackSelection{Audio: -1, Subtitle: -1, AudioSet: audioSet, SubtitleSet: subtitleSet}
	if audioSet && opts.AudioStream >= 0 {
		var ok bool
		selection.Audio, ok = position("audio", opts.AudioStream)
		if !ok {
			return opts, core.ErrNotAvailable
		}
	}
	if subtitleSet && opts.SubtitleStream >= 0 {
		var ok bool
		selection.Subtitle, ok = position("subtitle", opts.SubtitleStream)
		if !ok {
			return opts, core.ErrNotAvailable
		}
	}
	selected, err := prefs.Select(ctx, viewer, title.Key, info, title.Language, selection)
	if err != nil && onError != nil {
		onError(err)
	}
	if selected.AudioSet {
		if selected.Audio < 0 {
			opts.AudioStream = -1
		} else if selected.Audio >= len(info.Streams) || info.Streams[selected.Audio].Type != "audio" {
			return opts, core.ErrNotAvailable
		} else {
			opts.AudioStream = info.Streams[selected.Audio].Index
		}
	}
	if selected.SubtitleSet {
		if selected.Subtitle < 0 {
			opts.SubtitleStream = -1
		} else if selected.Subtitle >= len(info.Streams) || info.Streams[selected.Subtitle].Type != "subtitle" {
			return opts, core.ErrNotAvailable
		} else {
			opts.SubtitleStream = info.Streams[selected.Subtitle].Index
		}
	}
	filters, err := prefs.AudioFilters(ctx, viewer, info, selected)
	if err != nil {
		if onError != nil {
			onError(err)
		}
	} else {
		opts.AudioFilters = filters
	}
	return opts, nil
}
