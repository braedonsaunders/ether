package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// CinemaCandidate is one movie already authorized for the viewer.
type CinemaCandidate struct {
	Title   core.Title
	Extras  []frontend.Extra
	Watched bool
}

// SelectCinemaTrailers picks up to limit trailers of other unwatched movies.
// It checks at most twelve candidates, matching cinema-trailer selection:
// the feature itself, non-movies, and watched titles are skipped, and each
// movie contributes its first playable trailer or teaser.
func SelectCinemaTrailers(feature core.RatingKey, candidates []CinemaCandidate, limit int) []frontend.Extra {
	if limit <= 0 {
		return nil
	}
	if limit > extras.MaxExtras {
		limit = extras.MaxExtras
	}
	var out []frontend.Extra
	checked := 0
	for _, candidate := range candidates {
		if len(out) >= limit || checked >= 12 {
			break
		}
		title := candidate.Title
		if title.Key == feature || title.Type != core.TypeMovie || candidate.Watched {
			continue
		}
		checked++
		for _, clip := range candidate.Extras {
			if clip.Key == 0 || (clip.Kind != "trailer" && clip.Kind != "teaser") {
				continue
			}
			out = append(out, clip)
			break
		}
	}
	return out
}
