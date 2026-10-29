package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// PickAudio selects an audio stream position, or -1 when none exists.
func PickAudio(info core.MediaInfo, langs []string) int {
	for _, lang := range langs {
		// Within one wanted language the best tier wins (exact before
		// generic-want×regional before regional-want×generic); preference
		// order across languages still dominates.
		best, bestTier := -1, 0
		for i, st := range info.Streams {
			if st.Type != "audio" {
				continue
			}
			if tier := langtag.MatchTier(lang, st.Language); tier > bestTier {
				best, bestTier = i, tier
			}
		}
		if best >= 0 {
			return best
		}
	}
	first := -1
	for i, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		if st.Default {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}

// ResolveSubtitle maps a stored choice onto a stream position, or -1.
func ResolveSubtitle(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	if want == nil {
		if pos >= 0 && pos < len(info.Streams) && info.Streams[pos].Type == "subtitle" {
			return pos
		}
		return -1
	}
	if !want.External && pos >= 0 && pos < len(info.Streams) {
		st := info.Streams[pos]
		if st.Type == "subtitle" && !st.External && st.Index == want.Index && (want.Language == "" || sameTrackLanguage(st.Language, want.Language)) {
			return pos
		}
	}
	if want.External && want.ExternalURL != "" {
		for i, st := range info.Streams {
			if st.Type == "subtitle" && st.External && st.ExternalURL == want.ExternalURL {
				return i
			}
		}
	}
	best, bestScore := -1, 0
	for i, st := range info.Streams {
		if st.Type != "subtitle" || !sameTrackLanguage(st.Language, want.Language) {
			continue
		}
		n := 1
		if st.Forced == want.Forced {
			n += 4
		}
		if st.External == want.External {
			n += 2
		}
		if n > bestScore {
			best, bestScore = i, n
		}
	}
	return best
}

func sameTrackLanguage(a, b string) bool { return a != "" && b != "" && langtag.MatchTier(a, b) > 0 }
