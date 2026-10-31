package resolver

import (
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
)

// referenceHeight is the best resolution on offer for this play: the
// highest ranked height among the candidates and the releases a failover
// excludes (those were on offer before they failed).
func (s *Service) referenceHeight(cands []candidate, exclude []string) int {
	best := 0
	for _, c := range cands {
		best = max(best, c.quality.height)
	}
	for _, id := range exclude {
		if rel, ok := s.LookupRelease(id); ok {
			best = max(best, rel.Info.Height)
		}
	}
	return best
}

// drasticDowngrade reports a candidate far below the reference: SD or an
// unknown resolution when 1080p or better was on offer.
func drasticDowngrade(c candidate, reference int) bool {
	return reference >= 1080 && c.quality.height < 720
}

// splitDowngrades keeps rank order and separates the drastic downgrades.
func splitDowngrades(cands []candidate, reference int) (primary, downgrades []candidate) {
	for _, c := range cands {
		if drasticDowngrade(c, reference) {
			downgrades = append(downgrades, c)
		} else {
			primary = append(primary, c)
		}
	}
	return primary, downgrades
}

// downgradeNote says why a much lower quality plays.
func downgradeNote(reference int, rel core.Release, triedHere bool) string {
	what := "an unknown resolution"
	if rel.Info.Height > 0 {
		what = fmt.Sprintf("%dp", rel.Info.Height)
	}
	if triedHere {
		return fmt.Sprintf("Every %dp-class release failed its check and a rescue search found no comparable one, so %s plays instead.", reference, what)
	}
	return fmt.Sprintf("The %dp release that was tried before failed and a rescue search found no comparable one, so %s plays instead.", reference, what)
}
