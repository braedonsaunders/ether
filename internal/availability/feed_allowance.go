package availability

import (
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// feedAllowance counts the scheduled head polls the selected feeds need each
// UTC day. A split indexer exposes one feed per media side, and each side
// polls independently, so both count.
func feedAllowance(feeds []core.ReleaseFeed, selectedIDs []string, cfg config.Availability) (required, allowance, shortfall int) {
	allowance = cfg.FeedRequests
	if !cfg.Monitor || allowance <= 0 {
		return 0, allowance, 0
	}
	interval := max(5, cfg.PollMinutes)
	polls := (24*60 + interval - 1) / interval
	for _, f := range feeds {
		if !f.Supported || !selected(selectedIDs, f.ID) {
			continue
		}
		required += polls
	}
	shortfall = max(0, required-allowance)
	return required, allowance, shortfall
}

// feedSideMatches reports whether a media-side feed (id#tv or id#movie) can
// hold a release of this media. A mixed feed has no suffix and matches both;
// an unrecorded media type matches the first side so playback evidence is
// never dropped.
func feedSideMatches(feedID string, media core.MediaType) bool {
	_, side, ok := strings.Cut(feedID, "#")
	if !ok {
		return true
	}
	switch side {
	case "tv":
		return media != core.TypeMovie
	case "movie":
		return media != core.TypeEpisode && media != core.TypeShow
	}
	return true
}

// feedAllowanceWarning is the owner-facing text for a shortfall.
func feedAllowanceWarning(required, allowance int) string {
	return fmt.Sprintf(
		"The selected feeds need %d requests a day, more than the %d-request allowance; reduce the poll frequency, pages, or selected indexers.",
		required, allowance)
}
