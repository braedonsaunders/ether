package app

import "context"

func (a *App) cachedSpecialTitles(ctx context.Context, showID int64) []string {
	if a.catalog == nil {
		return nil
	}
	return a.catalog.SpecialTitles(ctx, showID)
}
