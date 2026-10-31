package config

import "slices"

// Prowlarr is an owner-managed source of Newznab indexers. Discovery is explicit
// and does not search or download from the indexers.
type Prowlarr struct {
	URL        string `json:"url"`
	APIKey     string `json:"apiKey"`
	DailyLimit int    `json:"dailyLimit"`
	DailyGrabs int    `json:"dailyGrabs"`
	Priority   int    `json:"priority"`
	// Tags are Prowlarr tag ids from /api/v1/tag. Empty syncs every enabled
	// Usenet and torrent indexer (the same import as before tags existed).
	// With ids set, only indexers carrying one of them are synced.
	Tags []int `json:"tags,omitempty"`
}

// NormalizeProwlarrTags returns the positive tag ids in ids, deduped and
// sorted. Non-positive ids are dropped. An empty result is nil so it
// matches the "no tag filter" config.
func NormalizeProwlarrTags(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}
