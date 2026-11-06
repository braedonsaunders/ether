package experience

import "github.com/braedonsaunders/ether/internal/core"

// ShowOnDeck selects the resumed or next regular episode in the viewer's order.
func ShowOnDeck(eps []core.Title, st map[core.RatingKey]core.WatchStatus, unavailable func(*core.Title) bool) *core.Title {
	lastWatched := -1
	resume := -1
	for i, e := range eps {
		ws := st[e.Key]
		if ws.ViewOffset > 0 && ws.ViewCount == 0 {
			if resume < 0 || ws.LastViewedAt.After(st[eps[resume].Key].LastViewedAt) {
				resume = i
			}
		}
		if ws.ViewCount > 0 && (lastWatched < 0 || ws.LastViewedAt.After(st[eps[lastWatched].Key].LastViewedAt) || ws.LastViewedAt.Equal(st[eps[lastWatched].Key].LastViewedAt)) {
			lastWatched = i
		}
	}
	if resume >= 0 {
		return &eps[resume]
	}
	for i := lastWatched + 1; i < len(eps); i++ {
		if eps[i].Season > 0 && eps[i].Episode > 0 && st[eps[i].Key].ViewCount == 0 && !unavailable(&eps[i]) {
			return &eps[i]
		}
	}
	if lastWatched < 0 {
		for i := range eps {
			if eps[i].Season > 0 && eps[i].Episode > 0 && !unavailable(&eps[i]) {
				return &eps[i]
			}
		}
	}
	return nil
}
