package experience

import (
	"strings"

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
// The choice is language, codec, and source (sidecar URL, or embedded
// versus external). A matching track at a new index beats whatever now
// sits in the old slot: variant lists reorder, and that slot is not the
// track the viewer picked.
func ResolveSubtitle(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	if want == nil {
		if pos >= 0 && pos < len(info.Streams) && info.Streams[pos].Type == "subtitle" {
			return pos
		}
		return -1
	}
	if want.Language == "" && want.ExternalURL == "" {
		if pos >= 0 && pos < len(info.Streams) && info.Streams[pos].Type == "subtitle" && info.Streams[pos].Index == want.Index {
			return pos
		}
		return -1
	}
	best, bestScore := -1, 0
	for i, st := range info.Streams {
		if st.Type != "subtitle" {
			continue
		}
		score := subtitleIdentityScore(*want, st)
		if score == 0 {
			continue
		}
		if i == pos {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// subtitleIdentityScore ranks how well st is the chosen subtitle. A sidecar
// URL is the source. Otherwise language is required, and the same codec and
// the same source kind (embedded or external) outrank a forced-flag or
// position tie.
func subtitleIdentityScore(want, st core.StreamInfo) int {
	if want.ExternalURL != "" && st.External && st.ExternalURL == want.ExternalURL {
		return 1000
	}
	if !sameTrackLanguage(st.Language, want.Language) {
		return 0
	}
	n := 10
	if subtitleCodec(want.Codec) != "" && subtitleCodec(st.Codec) == subtitleCodec(want.Codec) {
		n += 40
	}
	if st.External == want.External {
		n += 20
	}
	if st.Forced == want.Forced {
		n += 4
	}
	return n
}

func subtitleCodec(c string) string {
	c = strings.ToLower(c)
	if c == "subrip" {
		return "srt"
	}
	return c
}

func sameTrackLanguage(a, b string) bool { return a != "" && b != "" && langtag.MatchTier(a, b) > 0 }
