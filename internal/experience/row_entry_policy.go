package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RowEntryPolicy shares admission between rows and persisted row interactions.
type RowEntryPolicy struct {
	Visibility Visibility
	Favorite   func(context.Context, *core.User, FavoriteSubject) bool
	Playlist   func(context.Context, *core.User, core.RatingKey) bool
}

func (p RowEntryPolicy) Subject(ctx context.Context, u *core.User, kind, id string) bool {
	switch frontend.RowEntryKind(kind) {
	case frontend.RowEntryTitle:
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindTitle, ID: id})
	case frontend.RowEntryCollection:
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindCollection, ID: id})
	case frontend.RowEntryLiveChannel:
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindChannel, ID: id})
	case frontend.RowEntryPlaylist:
		key, err := core.ParseRatingKey(id)
		return err == nil && p.Playlist(ctx, u, key)
	default:
		return false
	}
}
func (p RowEntryPolicy) Row(ctx context.Context, u *core.User, row frontend.Row) bool {
	return row.Collection == 0 || p.Subject(ctx, u, string(frontend.RowEntryCollection), row.Collection.String())
}
func (p RowEntryPolicy) Entry(ctx context.Context, u *core.User, e frontend.RowEntry) bool {
	switch e.Kind {
	case frontend.RowEntryTitle:
		return e.Title != nil && p.Visibility.Visible(ctx, u, e.Title)
	case frontend.RowEntryCollection:
		return e.Collection != nil && p.Subject(ctx, u, string(e.Kind), e.Collection.Key.String())
	case frontend.RowEntryPlaylist:
		return e.Playlist != nil && p.Playlist(ctx, u, e.Playlist.Key)
	case frontend.RowEntryLiveChannel:
		return e.LiveChannel != nil && p.Subject(ctx, u, string(e.Kind), e.LiveChannel.ID)
	default:
		return false
	}
}
