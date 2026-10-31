package personalize

import "github.com/braedonsaunders/ether/internal/core"

// Assert extension contracts at the producer so a removed method cannot
// silently make downstream consumers select their fallback.
var (
	_ core.HomeCandidateSource     = (*Engine)(nil)
	_ core.SectionCandidateSource  = (*Engine)(nil)
	_ core.HomeLayoutSource        = (*Engine)(nil)
	_ core.HubItemSource           = (*Engine)(nil)
	_ core.CollectionArtworkSource = (*reviewCollections)(nil)
)
