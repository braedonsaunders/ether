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

// HomeAssembly owns row inclusion, deduplication and ordering while adapters
// supply rendered projections and preserve their exact producer sidecars.
type HomeAssembly[P any] struct {
	Render      func(core.Hub) P
	Identity    func(P) string
	Continue    func() (core.Hub, P)
	Playlists   func() (P, bool)
	Discovery   func() []core.Hub
	Live        func() (P, bool)
	InLibraries func(core.Hub) bool
	Select      func([]P) []P
	Layout      func([]P) []P
}

func (a HomeAssembly[P]) Rows(hubs []core.Hub, includeContinue, promotedOnly, includePlaylists bool, purpose func(core.Hub) core.RowPurpose) ([]P, map[string]core.Hub) {
	var out []P
	served := map[string]core.Hub{}
	candidates := make([]HomeCandidate, len(hubs))
	for i, hub := range hubs {
		candidates[i] = HomeCandidate{Index: i, Purpose: purpose(hub), Promoted: hub.Promoted, InLibraries: a.InLibraries(hub)}
	}
	var projected P
	AssembleHome(candidates, includeContinue, promotedOnly, "home.continue", func() {
		hub, row := a.Continue()
		out = append(out, row)
		served[a.Identity(row)] = hub
	}, func(candidate HomeCandidate) string {
		projected = a.Render(hubs[candidate.Index])
		return a.Identity(projected)
	}, func(candidate HomeCandidate) {
		out = append(out, projected)
		served[a.Identity(projected)] = hubs[candidate.Index]
	})
	if includePlaylists {
		if row, ok := a.Playlists(); ok {
			out = append(out, row)
		}
	}
	for _, hub := range a.Discovery() {
		out = append(out, a.Render(hub))
		served[hub.ID] = hub
	}
	if row, ok := a.Live(); ok {
		out = append(out, row)
	}
	out = a.Select(out)
	out = a.Layout(out)
	return out, served
}
