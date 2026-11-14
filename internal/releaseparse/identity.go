package releaseparse

import (
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// CompareTitleIDs compares only shared identifier namespaces. A matching
// identifier is positive evidence. TMDB/TVDB conflicts always win; an IMDb
// conflict is tolerated only when both TMDB and TVDB independently agree.
// Missing identifiers carry no evidence and never become a match merely
// because the indexer was queried with the expected identifiers.
func CompareTitleIDs(reported, expected core.IDs) (matched, conflicting bool) {
	for _, pair := range [][2]int64{{reported.TMDB, expected.TMDB}, {reported.TVDB, expected.TVDB}} {
		if pair[0] > 0 && pair[1] > 0 {
			matched = matched || pair[0] == pair[1]
			conflicting = conflicting || pair[0] != pair[1]
		}
	}
	normalize := func(id string) string {
		id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "tt")
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 || len(id) > 10 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	a, b := normalize(reported.IMDB), normalize(expected.IMDB)
	if a != "" && b != "" {
		matched = matched || a == b
		// Episode/related-entry IMDb IDs occur in anime indexer results.
		// Require corroboration from both series namespaces before ignoring
		// that discrepancy; one matching ID alone cannot override it.
		corroborated := reported.TMDB > 0 && reported.TMDB == expected.TMDB &&
			reported.TVDB > 0 && reported.TVDB == expected.TVDB
		conflicting = conflicting || (a != b && !corroborated)
	}
	return matched && !conflicting, conflicting
}
