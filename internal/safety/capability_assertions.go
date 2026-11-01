package safety

import "github.com/braedonsaunders/ether/internal/core"

// Assert extension contracts at the producer so a removed method cannot
// silently make downstream consumers select their fallback.
var (
	_ core.PlaybackAccessPolicy = (*Service)(nil)
	_ core.BitrateAccessPolicy  = (*Service)(nil)
	_ core.ArtworkAccessPolicy  = (*Service)(nil)
	_ core.UserPolicySource     = (*Service)(nil)
	_ core.DownloadAccessPolicy = (*Service)(nil)
)
