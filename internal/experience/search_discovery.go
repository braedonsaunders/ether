package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
)

// DiscoverySearch uses natural search only when enabled and successful.
func DiscoverySearch(source *discovery.Features) func(context.Context, *core.User, string, int) ([]core.Title, bool) {
	return func(ctx context.Context, viewer *core.User, query string, limit int) ([]core.Title, bool) {
		if source == nil || source.LLM == nil || source.Watchlist == nil {
			return nil, false
		}
		cfg, err := source.Watchlist.Settings(ctx)
		if err != nil || !cfg.NaturalSearch {
			return nil, false
		}
		items, err := source.LLM.NaturalSearch(ctx, viewer, query, limit)
		if err != nil {
			return nil, false
		}
		return items, true
	}
}
