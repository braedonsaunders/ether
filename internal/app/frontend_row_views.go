package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (a *App) newRowViews(protocol core.Protocol, rows *experience.RowsService, receipts *experience.Receipts, visibility experience.Visibility, libraries []experience.Library) (*experience.RowViewService, error) {
	d := experience.RuntimeRowViews{DB: a.db, Protocol: protocol, Receipts: receipts, Visibility: visibility, Libraries: libraries, Active: a.users.PolicyUser, CachedGet: a.catalog.CachedGet, CachedGetMany: a.catalog.CachedGetMany, Playlists: a.playlists, Situation: situationFor,
		Snapshot: func(ctx context.Context, request frontend.RowRequest, situation core.HomeSituation) ([]frontend.Row, error) {
			return rows.SnapshotRows(context.WithValue(ctx, experienceSituationKey{}, situation), request, protocol)
		}}
	if a.collections != nil {
		d.Collections = a.collections
	}
	if a.episodeorder != nil {
		d.CachedVirtualSeason = a.episodeorder.CachedVirtualSeason
	}
	if a.livetv != nil {
		d.Live = a.livetv
	}
	return experience.NewRuntimeRowViews(d)
}

// rowViewResolveBatch binds this call's viewer, device and protocol into the
// neutral legacy hydrator. The adapters exist only for this call.
func rowViewResolveBatch(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef, one func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), live func(context.Context, *core.User) ([]frontend.RowEntry, error)) ([]frontend.RowEntry, error) {
	return experience.RowViewHydrateLegacy(ctx, viewer, refs, func(ctx context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		return one(ctx, viewer, device, protocol, ref)
	}, func(ctx context.Context) ([]frontend.RowEntry, error) {
		return live(ctx, viewer)
	})
}

type rowViewCollectionSource = experience.RowViewCollectionSource

func rowViewCollectionMembers(ctx context.Context, source rowViewCollectionSource, user *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	return experience.RowViewCollectionMembers(ctx, source, user, collection, refs)
}
func rowViewResolveBatchTitles(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef, one func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), batch func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error), get func(context.Context, *core.User, core.RatingKey) (*core.Title, error), visibility experience.Visibility, live func(context.Context, *core.User) ([]frontend.RowEntry, error)) ([]frontend.RowEntry, error) {
	return experience.RowViewResolveBatchTitles(ctx, viewer, device, protocol, refs, one, batch, get, visibility, live)
}
