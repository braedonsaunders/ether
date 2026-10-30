package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// rowViewRevision reads shared preference and layout versions, so snapshots
// created by another node fail closed after a relevant change.
func (a *App) rowViewRevision(ctx context.Context, user *core.User) (string, error) {
	var layout string
	var favorites, exclusions int64
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT document FROM personalize_home_layout WHERE id=1),''), COALESCE((SELECT updated_at FROM favorites_mirror WHERE viewer_id=?),0), COALESCE((SELECT revision FROM notinterested_revisions WHERE viewer_id=?),0)`, user.ID, user.ID).Scan(&layout, &favorites, &exclusions)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal([]any{user.ID, user.IsOwner, user.MaxContentRating, user.AllowedSections, layout, favorites, exclusions})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (a *App) newRowViews(protocol core.Protocol, rows *experience.RowsService, receipts *experience.Receipts, visibility experience.Visibility, libraries []experience.Library) (*experience.RowViewService, error) {
	// A row snapshot is already drawn from the local catalogue. Revalidation
	// must never trigger a per-title network refresh while a client pages it.
	cacheVisibility := visibility
	cacheVisibility.GetTitle = a.catalog.CachedGet
	getCached := func(ctx context.Context, user *core.User, key core.RatingKey) (*core.Title, error) {
		title, err := a.catalog.CachedGet(ctx, key)
		if errors.Is(err, core.ErrNotFound) && a.episodeorder != nil && key.Kind() == core.KindSeason {
			return a.episodeorder.CachedVirtualSeason(ctx, user, key)
		}
		return title, err
	}
	options := experience.RowViewOptions{
		DB: a.db, TTL: 15 * time.Minute, Active: a.users.ActiveUser, Receipts: receipts,
		Build: func(ctx context.Context, request frontend.RowViewRequest) ([]experience.RowViewSnapshot, error) {
			if request.Protocol != protocol {
				return nil, core.ErrUnauthorized
			}
			before, err := a.rowViewRevision(ctx, request.Viewer)
			if err != nil {
				return nil, err
			}
			r := frontend.RowRequest{Viewer: request.Viewer, Device: request.Device, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Limit: 50}
			situation := situationFor(r)
			result, err := rows.SnapshotRows(context.WithValue(ctx, experienceSituationKey{}, situation), r, protocol)
			if err != nil {
				return nil, err
			}
			after, err := a.rowViewRevision(ctx, request.Viewer)
			if err != nil {
				return nil, err
			}
			if before != after {
				return nil, experience.ErrRowViewStale
			}
			out := make([]experience.RowViewSnapshot, 0, len(result))
			for _, row := range result {
				s := experience.RowViewSnapshot{Row: row, Situation: situation, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Revision: after, Truncated: row.More || len(row.Entries) >= 50}
				for _, entry := range row.Entries {
					s.Entries = append(s.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: experience.RowEntryIdentity(entry)})
				}
				out = append(out, s)
			}
			return out, nil
		},
		ValidRow: func(ctx context.Context, u *core.User, _ core.Device, p core.Protocol, s experience.RowViewSnapshot) bool {
			if p != protocol {
				return false
			}
			revision, err := a.rowViewRevision(ctx, u)
			if err != nil || revision != s.Revision {
				return false
			}
			switch s.Placement {
			case frontend.PlacementHome:
			case frontend.PlacementLibrary:
				id, err := strconv.Atoi(s.ViewID)
				valid := false
				for _, lib := range libraries {
					if lib.ID == id {
						valid = true
						break
					}
				}
				if err != nil || !valid || !experience.SectionAllowed(u, id) {
					return false
				}
			case frontend.PlacementDetail:
				if s.Item == 0 {
					return false
				}
				title, err := getCached(ctx, u, s.Item)
				if err != nil || !cacheVisibility.Visible(ctx, u, title) {
					return false
				}
			default:
				return false
			}
			if s.Row.Collection != 0 {
				if a.collections == nil {
					return false
				}
				c, err := a.collections.Get(ctx, u, s.Row.Collection)
				if err != nil || c == nil || !visibility.CollectionAllowed(u, c) {
					return false
				}
				if !rowViewCollectionMembers(ctx, a.collections, u, s.Row.Collection, s.Entries) {
					return false
				}
			}
			return true
		},
		ResolveEntry: func(ctx context.Context, u *core.User, _ core.Device, p core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
			if p != protocol {
				return frontend.RowEntry{}, core.ErrNotFound
			}
			switch frontend.RowEntryKind(ref.Kind) {
			case frontend.RowEntryTitle:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				t, err := getCached(ctx, u, key)
				if err != nil || t == nil || t.Key != key || !cacheVisibility.Visible(ctx, u, t) {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: t}, nil
			case frontend.RowEntryCollection:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil || a.collections == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				c, err := a.collections.Get(ctx, u, key)
				if err != nil || c == nil || c.Key != key || !visibility.CollectionAllowed(u, c) {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				return frontend.RowEntry{Kind: frontend.RowEntryCollection, Collection: c}, nil
			case frontend.RowEntryPlaylist:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil || a.playlists == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				p, err := a.playlists.Get(ctx, u.ID, key)
				if err != nil || p == nil || p.UserID != u.ID || p.Key != key {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				copy := *p
				copy.Items = nil
				return frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &copy}, nil
			case frontend.RowEntryLiveChannel:
				if a.livetv == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				now := time.Now()
				entries, err := experience.LiveEntries(ctx, u, a.livetv, now, now.Add(24*time.Hour), nil)
				if err != nil {
					return frontend.RowEntry{}, err
				}
				for _, entry := range entries {
					if entry.LiveChannel != nil && entry.LiveChannel.ID == ref.ID {
						return entry, nil
					}
				}
			}
			return frontend.RowEntry{}, core.ErrNotFound
		},
	}
	options.ResolveEntries = func(ctx context.Context, u *core.User, device core.Device, p core.Protocol, refs []core.NeutralEntryRef) ([]frontend.RowEntry, error) {
		if p != protocol {
			return nil, core.ErrNotFound
		}
		return rowViewResolveBatch(ctx, u, device, p, refs, options.ResolveEntry, func(ctx context.Context, viewer *core.User) ([]frontend.RowEntry, error) {
			if a.livetv == nil {
				return nil, core.ErrNotFound
			}
			now := time.Now()
			return experience.LiveEntries(ctx, viewer, a.livetv, now, now.Add(24*time.Hour), nil)
		})
	}
	return experience.NewRowViewService(options)
}

// rowViewResolveBatch loads the viewer's live guide at most once per policy
// pass. Calls are independent so final revalidation observes current policy.
func rowViewResolveBatch(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef, one func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), live func(context.Context, *core.User) ([]frontend.RowEntry, error)) ([]frontend.RowEntry, error) {
	var liveRows map[string]frontend.RowEntry
	out := make([]frontend.RowEntry, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind != string(frontend.RowEntryLiveChannel) {
			entry, err := one(ctx, viewer, device, protocol, ref)
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
			continue
		}
		if liveRows == nil {
			entries, err := live(ctx, viewer)
			if err != nil {
				return nil, err
			}
			liveRows = make(map[string]frontend.RowEntry, len(entries))
			for _, entry := range entries {
				if entry.Kind == frontend.RowEntryLiveChannel && entry.LiveChannel != nil {
					liveRows[entry.LiveChannel.ID] = entry
				}
			}
		}
		entry, ok := liveRows[ref.ID]
		if !ok {
			return nil, core.ErrNotFound
		}
		out = append(out, entry)
	}
	return out, nil
}

type rowViewCollectionSource interface {
	Items(context.Context, *core.User, core.RatingKey, int, int) (core.Page, error)
}

// Snapshot members must still belong to the viewer-visible collection.
// Stop at a bounded scan and fail closed if membership cannot be established.
func rowViewCollectionMembers(ctx context.Context, source rowViewCollectionSource, user *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	if source == nil || user == nil {
		return false
	}
	want := make(map[core.RatingKey]bool)
	for _, ref := range refs {
		if ref.Kind != string(frontend.RowEntryTitle) {
			return false
		}
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return false
		}
		want[key] = true
	}
	if len(want) == 0 {
		return false
	}
	for offset := 0; offset < 10000; {
		page, err := source.Items(ctx, user, collection, offset, 100)
		if err != nil {
			return false
		}
		for _, item := range page.Items {
			delete(want, item.Key)
		}
		if len(want) == 0 {
			return true
		}
		if len(page.Items) == 0 {
			return false
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return false
		}
	}
	return false
}
