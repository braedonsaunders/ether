package sessions

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// bindSourceTracks translates only against the pinned opened generation.
// SelectScopedTracks consumes absolute indices, not positions in the slice.
func bindSourceTracks(intent *frontend.PlayIntent, info core.MediaInfo) error {
	if intent.AudioOrdinal == nil && intent.SubtitleStreamIndex == nil {
		return nil
	}
	if info.Estimated || len(info.Streams) == 0 {
		return frontend.ErrTrackSourceUnavailable
	}
	unique := func(index int, kind string) bool {
		if index < 0 {
			return false
		}
		count := 0
		matching := false
		for _, stream := range info.Streams {
			if stream.Index == index {
				count++
				// External sidecars are not FFmpeg stream indices of the
				// opened source and need their own scoped authorization.
				matching = stream.Type == kind && !stream.External && stream.ExternalURL == ""
			}
		}
		return count == 1 && matching
	}
	if intent.AudioOrdinal != nil {
		if intent.AudioSet || *intent.AudioOrdinal < 0 {
			return frontend.ErrTrackNotFound
		}
		ordinal, index := 0, -1
		for _, stream := range info.Streams {
			if stream.Type == "audio" {
				if ordinal == *intent.AudioOrdinal {
					index = stream.Index
					break
				}
				ordinal++
			}
		}
		if !unique(index, "audio") {
			return frontend.ErrTrackNotFound
		}
		intent.Options.AudioStream, intent.AudioSet = index, true
	}
	if intent.SubtitleStreamIndex != nil {
		index := *intent.SubtitleStreamIndex
		if intent.SubtitleSet || index < -1 || (index != -1 && !unique(index, "subtitle")) {
			return frontend.ErrTrackNotFound
		}
		intent.Options.SubtitleStream, intent.SubtitleSet = index, true
	}
	return nil
}
