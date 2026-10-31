package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// ResolveAudio maps an issued audio identity onto a position in the current
// MediaInfo.Streams. Callers needing an encoder index must translate that
// position through the selected StreamInfo.Index. The caller must supply a
// non-nil issued stream identity.
func ResolveAudio(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	best, bestScore := -1, -1
	for i, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		if want.Language == "" {
			if i == pos && sameAudioIdentity(*want, st) {
				return i
			}
			continue
		}
		tier := langtag.MatchTier(want.Language, st.Language)
		if tier == 0 {
			continue
		}
		score := tier * 32
		if st.Codec == want.Codec {
			score += 8
		}
		if st.Channels == want.Channels {
			score += 4
		}
		if st.Title == want.Title {
			score += 2
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

func sameAudioIdentity(a, b core.StreamInfo) bool {
	return a.Type == b.Type && a.Codec == b.Codec && a.Language == b.Language &&
		a.Channels == b.Channels && a.Title == b.Title && a.Forced == b.Forced &&
		a.External == b.External && a.ExternalURL == b.ExternalURL
}
