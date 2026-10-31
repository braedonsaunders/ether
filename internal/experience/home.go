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
// Report records layout facts at paint time. Layout receives those facts and
// must not read the wire value again.
type HomeAssembly[P any] struct {
	Render      func(core.Hub) P
	Identity    func(P) string
	Report      func(P) (core.Hub, core.NeutralHomeRow)
	Continue    func() (core.Hub, P)
	Playlists   func() (P, bool)
	Discovery   func() []core.Hub
	Live        func() (P, bool)
	InLibraries func(core.Hub) bool
	Keep        func(P) bool
	Layout      func([]P, []core.Hub, []core.NeutralHomeRow) []P
}

func (a HomeAssembly[P]) Rows(hubs []core.Hub, includeContinue, promotedOnly, includePlaylists bool, purpose func(core.Hub) core.RowPurpose) ([]P, map[string]core.Hub) {
	var out []P
	var layoutHubs []core.Hub
	var neutral []core.NeutralHomeRow
	served := map[string]core.Hub{}
	appendRow := func(row P, source core.Hub, key string, record bool) {
		out = append(out, row)
		var hub core.Hub
		var fact core.NeutralHomeRow
		if a.Report != nil {
			hub, fact = a.Report(row)
		}
		layoutHubs = append(layoutHubs, hub)
		neutral = append(neutral, fact)
		if record {
			served[key] = source
		}
	}
	candidates := make([]HomeCandidate, len(hubs))
	for i, hub := range hubs {
		candidates[i] = HomeCandidate{Index: i, Purpose: purpose(hub), Promoted: hub.Promoted, InLibraries: a.InLibraries(hub)}
	}
	var projected P
	AssembleHome(candidates, includeContinue, promotedOnly, "home.continue", func() {
		hub, row := a.Continue()
		appendRow(row, hub, a.Identity(row), true)
	}, func(candidate HomeCandidate) string {
		projected = a.Render(hubs[candidate.Index])
		return a.Identity(projected)
	}, func(candidate HomeCandidate) {
		appendRow(projected, hubs[candidate.Index], a.Identity(projected), true)
	})
	if includePlaylists {
		if row, ok := a.Playlists(); ok {
			appendRow(row, core.Hub{}, "", false)
		}
	}
	for _, hub := range a.Discovery() {
		appendRow(a.Render(hub), hub, hub.ID, true)
	}
	if row, ok := a.Live(); ok {
		appendRow(row, core.Hub{}, "", false)
	}
	if a.Keep != nil {
		kept := make([]P, 0, len(out))
		keptHubs := make([]core.Hub, 0, len(out))
		keptNeutral := make([]core.NeutralHomeRow, 0, len(out))
		for i, row := range out {
			if !a.Keep(row) {
				continue
			}
			kept = append(kept, row)
			keptHubs = append(keptHubs, layoutHubs[i])
			keptNeutral = append(keptNeutral, neutral[i])
		}
		out, layoutHubs, neutral = kept, keptHubs, keptNeutral
	}
	if a.Layout != nil {
		out = a.Layout(out, layoutHubs, neutral)
	}
	return out, served
}
