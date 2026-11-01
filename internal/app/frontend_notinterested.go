package app

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/personalize"
)

func (a *App) notInterestedSignals(ctx context.Context, u *core.User) ([]personalize.NotInterestedSignal, error) {
	if a.notInterested == nil {
		return nil, nil
	}
	entries, err := a.notInterested.List(ctx, u, 0)
	if errors.Is(err, notinterested.ErrForbidden) {
		return nil, nil // no server access: nothing hidden to weigh
	}
	if err != nil {
		return nil, err
	}
	result := make([]personalize.NotInterestedSignal, 0, len(entries))
	for _, entry := range entries {
		result = append(result, personalize.NotInterestedSignal{Key: entry.Key, AddedAt: entry.AddedAt})
	}
	return result, nil
}

func (a *App) notInterestedSnapshot(ctx context.Context, u *core.User) ([]personalize.NotInterestedSignal, uint64, error) {
	if a.notInterested == nil {
		return nil, 0, nil
	}
	entries, revision, err := a.notInterested.Snapshot(ctx, u)
	if errors.Is(err, notinterested.ErrForbidden) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	result := make([]personalize.NotInterestedSignal, 0, len(entries))
	for _, entry := range entries {
		result = append(result, personalize.NotInterestedSignal{Key: entry.Key, AddedAt: entry.AddedAt})
	}
	return result, revision, nil
}
func (a *App) notInterestedChanged(ctx context.Context, id int64, key core.RatingKey) {
	if a.personalize != nil {
		a.personalize.NotInterestedChanged(id)
	}
	if a.sessionEvents != nil {
		event := frontend.Event{Type: "UserDataChanged", ViewerID: id, Item: key}
		if err := a.sessionEvents.Publish(ctx, event); err != nil {
			a.log.Warn("not interested: publish failed", "err", err)
		}
	}
}

type notInterestedPortal struct{ app *App }

func (p notInterestedPortal) ListNotInterested(ctx context.Context, u *core.User) ([]admin.NotInterestedEntry, error) {
	entries, err := p.app.notInterested.ListUndo(ctx, u)
	if err != nil {
		return nil, err
	}
	result := make([]admin.NotInterestedEntry, 0, len(entries))
	for _, entry := range entries {
		out := admin.NotInterestedEntry{Handle: entry.Handle}
		if entry.Key != 0 {
			title, err := p.app.catalog.Get(ctx, entry.Key)
			allowed, e := p.app.notInterested.Has(ctx, u, entry.Key)
			if err == nil && title != nil && title.Key == entry.Key && e == nil && allowed {
				out.Title = title.Title
				out.Key = entry.Key.String()
			}
		}
		result = append(result, out)
	}
	return result, nil
}
func (p notInterestedPortal) RestoreNotInterested(ctx context.Context, u *core.User, handle string) (bool, error) {
	return p.app.notInterested.Undo(ctx, u, handle)
}
