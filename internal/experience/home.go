package experience

import "github.com/braedonsaunders/ether/internal/core"

// HomeCandidate carries neutral row placement information. Index identifies
// the producer's candidate without exposing its wire representation.
type HomeCandidate struct {
	Index       int
	Purpose     core.RowPurpose
	Promoted    bool
	InLibraries bool
}

// AssembleHome places the continue row at its ranked position and deduplicates
// rendered identifiers. The renderer returns the stable identifier it actually
// used; this can differ from the producer's candidate identifier.
func AssembleHome(candidates []HomeCandidate, includeContinue, promotedOnly bool, continueID string, renderContinue func(), render func(HomeCandidate) string, accept func(HomeCandidate)) {
	hasContinue := false
	for _, row := range candidates {
		if row.Purpose == core.RowPurpose("continue") {
			hasContinue = true
			break
		}
	}
	placed := false
	place := func() {
		if includeContinue && !placed {
			placed = true
			renderContinue()
		}
	}
	if !hasContinue {
		place()
	}
	seen := map[string]bool{continueID: true}
	for _, row := range candidates {
		if row.Purpose == core.RowPurpose("continue") {
			place()
			continue
		}
		if (promotedOnly && !row.Promoted) || !row.InLibraries {
			continue
		}
		id := render(row)
		if seen[id] {
			continue
		}
		seen[id] = true
		accept(row)
	}
}

// CompleteHome orders the remaining home producers and finalizes the response.
// The adapter owns rendered metadata and only exposes these operations here.
func CompleteHome(includePlaylists bool, playlists, discovery, live, selectRows, layout func()) {
	if includePlaylists {
		playlists()
	}
	discovery()
	live()
	selectRows()
	layout()
}
