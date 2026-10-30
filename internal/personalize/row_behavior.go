package personalize

import "github.com/braedonsaunders/ether/internal/core"

// rowBehavior separates utility-row semantics from the protocol that provided
// a legacy row. Historical "plex" provenance is retained as an alias.
type rowBehavior struct {
	countsInBudget      bool
	claimsFirst         bool
	participatesInDraft bool
	requiresMinimum     bool
	participatesInLead  bool
	learnsKind          bool
}

func behaviorForRowSource(source string) rowBehavior {
	precomposed := source == core.RowSourcePlex || source == core.RowSourcePrecomposed
	return rowBehavior{countsInBudget: !precomposed, claimsFirst: precomposed, participatesInDraft: !precomposed, requiresMinimum: !precomposed, participatesInLead: !precomposed, learnsKind: !precomposed}
}
