package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// RuntimeRowViews binds persistence, snapshot construction and current policy
// revalidation inside Experience. The app supplies source capabilities only.
type RuntimeRowViews struct {
	DB                  *sql.DB
	Protocol            core.Protocol
	Receipts            *Receipts
	Visibility          Visibility
	Libraries           []Library
	Active              func(context.Context, int64) (*core.User, error)
	CachedGet           func(context.Context, core.RatingKey) (*core.Title, error)
	CachedGetMany       func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	CachedVirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	Collections         core.Collections
	Playlists           core.Playlists
	Live                LiveGuideSource
	Situation           func(frontend.RowRequest) core.HomeSituation
	Snapshot            func(context.Context, frontend.RowRequest, core.HomeSituation) ([]frontend.Row, error)
}

// RowViewRevision reads shared preference and layout versions, so snapshots
// created by another node fail closed after a relevant change.
func RowViewRevision(ctx context.Context, db *sql.DB, user *core.User) (string, error) {
	epoch, err := viewerpolicy.Read(ctx, db)
	if err != nil {
		return "", err
	}
	var layout string
	var favorites, exclusions int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT document FROM personalize_home_layout WHERE id=1),''), COALESCE((SELECT updated_at FROM favorites_mirror WHERE viewer_id=?),0), COALESCE((SELECT revision FROM notinterested_revisions WHERE viewer_id=?),0)`, user.ID, user.ID).Scan(&layout, &favorites, &exclusions)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal([]any{user.ID, user.IsOwner, user.MaxContentRating, user.AllowedSections, epoch, layout, favorites, exclusions})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func NewRuntimeRowViews(d RuntimeRowViews) (*RowViewService, error) {
	protocol, receipts, visibility, libraries := d.Protocol, d.Receipts, d.Visibility, d.Libraries
	// A row snapshot is already drawn from the local catalogue. Revalidation
	// must never trigger a per-title network refresh while a client pages it.
	cacheVisibility := visibility
	readTitle := func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		if d.CachedGetMany == nil {
			return d.CachedGet(ctx, key)
		}
		titles, err := d.CachedGetMany(ctx, []core.RatingKey{key})
		if err != nil {
			return nil, err
		}
		title, ok := titles[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}
	cacheVisibility.GetTitle = readTitle
	getCached := func(ctx context.Context, user *core.User, key core.RatingKey) (*core.Title, error) {
		title, err := readTitle(ctx, key)
		if errors.Is(err, core.ErrNotFound) && d.CachedVirtualSeason != nil && key.Kind() == core.KindSeason {
			return d.CachedVirtualSeason(ctx, user, key)
		}
		return title, err
	}
	options := RowViewOptions{
		DB: d.DB, TTL: 15 * time.Minute, Active: d.Active, Receipts: receipts,
		Build: func(ctx context.Context, request frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			if request.Protocol != protocol {
				return nil, core.ErrUnauthorized
			}
			before, err := RowViewRevision(ctx, d.DB, request.Viewer)
			if err != nil {
				return nil, err
			}
			fresh, err := d.Active(ctx, request.Viewer.ID)
			if err != nil || fresh == nil {
				return nil, ErrRowViewStale
			}
			request.Viewer = fresh
			r := frontend.RowRequest{Viewer: request.Viewer, Device: request.Device, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Limit: 50}
			situation := d.Situation(r)
			result, err := d.Snapshot(ctx, r, situation)
			if err != nil {
				return nil, err
			}
			after, err := RowViewRevision(ctx, d.DB, request.Viewer)
			if err != nil {
				return nil, err
			}
			if before != after {
				return nil, ErrRowViewStale
			}
			out := make([]RowViewSnapshot, 0, len(result))
			for _, row := range result {
				s := RowViewSnapshot{Row: row, Situation: situation, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Revision: after, Truncated: row.More || len(row.Entries) >= 50}
				for _, entry := range row.Entries {
					s.Entries = append(s.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: RowEntryIdentity(entry)})
				}
				out = append(out, s)
			}
			return out, nil
		},
		ValidRow: func(ctx context.Context, u *core.User, _ core.Device, p core.Protocol, s RowViewSnapshot) bool {
			if p != protocol {
				return false
			}
			revision, err := RowViewRevision(ctx, d.DB, u)
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
				if err != nil || !valid || !SectionAllowed(u, id) {
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
				if d.Collections == nil {
					return false
				}
				c, err := d.Collections.Get(ctx, u, s.Row.Collection)
				if err != nil || c == nil || !visibility.CollectionAllowed(u, c) {
					return false
				}
				if !RowViewCollectionMembers(ctx, d.Collections, u, s.Row.Collection, s.Entries) {
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
				if err != nil || d.Collections == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				c, err := d.Collections.Get(ctx, u, key)
				if err != nil || c == nil || c.Key != key || !visibility.CollectionAllowed(u, c) {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				return frontend.RowEntry{Kind: frontend.RowEntryCollection, Collection: c}, nil
			case frontend.RowEntryPlaylist:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil || d.Playlists == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				p, err := d.Playlists.Get(ctx, u.ID, key)
				if err != nil || p == nil || p.UserID != u.ID || p.Key != key {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				copy := *p
				copy.Items = nil
				return frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &copy}, nil
			case frontend.RowEntryLiveChannel:
				if d.Live == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				now := time.Now()
				entries, err := LiveEntries(ctx, u, d.Live, now, now.Add(24*time.Hour), nil)
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
		return RowViewResolveBatchTitles(ctx, u, device, p, refs, options.ResolveEntry, d.CachedGetMany, getCached, cacheVisibility, func(ctx context.Context, viewer *core.User) ([]frontend.RowEntry, error) {
			if d.Live == nil {
				return nil, core.ErrNotFound
			}
			now := time.Now()
			return LiveEntries(ctx, viewer, d.Live, now, now.Add(24*time.Hour), nil)
		})
	}
	return NewRowViewService(options)
}

type RowViewCollectionSource interface {
	Items(context.Context, *core.User, core.RatingKey, int, int) (core.Page, error)
}

// RowViewCollectionMembers adapts one call's collection source into neutral
// member keys. A nil source stays a nil port and fails closed.
func RowViewCollectionMembers(ctx context.Context, source RowViewCollectionSource, user *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	if source == nil {
		return RowViewMembers(ctx, nil, user, collection, refs)
	}
	return RowViewMembers(ctx, func(ctx context.Context, viewer *core.User, key core.RatingKey, offset, limit int) (RowViewCollectionPage, error) {
		page, err := source.Items(ctx, viewer, key, offset, limit)
		if err != nil {
			return RowViewCollectionPage{}, err
		}
		keys := make([]core.RatingKey, len(page.Items))
		for i := range page.Items {
			keys[i] = page.Items[i].Key
		}
		return RowViewCollectionPage{Keys: keys, Total: page.Total}, nil
	}, user, collection, refs)
}

// RowViewResolveBatchTitles binds this call's viewer, device and protocol into
// the neutral title hydrator. Nothing in the adapter is reused for a later pass.
func RowViewResolveBatchTitles(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef, one func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), batch func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error), get func(context.Context, *core.User, core.RatingKey) (*core.Title, error), visibility Visibility, live func(context.Context, *core.User) ([]frontend.RowEntry, error)) ([]frontend.RowEntry, error) {
	return RowViewHydrateEntries(ctx, viewer, refs, func(ctx context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		return one(ctx, viewer, device, protocol, ref)
	}, batch, func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		return get(ctx, viewer, key)
	}, visibility, func(ctx context.Context) ([]frontend.RowEntry, error) {
		return live(ctx, viewer)
	})
}
