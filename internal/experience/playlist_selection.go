package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// PlaylistSelection expands containers through viewer-bound metadata sources.
// URI parsing and playlist presentation belong to the requesting adapter.
type PlaylistSelection struct {
	Playlists     core.Playlists
	Collections   core.Collections
	Visibility    Visibility
	Children      func(context.Context, core.RatingKey) ([]core.Title, error)
	Grandchildren func(context.Context, core.RatingKey) ([]core.Title, error)
}

func (s PlaylistSelection) Get(ctx context.Context, viewer *core.User, key core.RatingKey) (*core.Playlist, error) {
	if s.Playlists == nil || viewer == nil {
		return nil, core.ErrNotFound
	}
	return s.Playlists.Get(ctx, viewer.ID, key)
}
func (s PlaylistSelection) Expand(ctx context.Context, viewer *core.User, key core.RatingKey) ([]core.RatingKey, error) {
	switch key.Kind() {
	case core.KindShow, core.KindSeason:
		load := s.Children
		if key.Kind() == core.KindShow {
			load = s.Grandchildren
		}
		titles, err := load(ctx, key)
		if err != nil {
			return nil, err
		}
		out := make([]core.RatingKey, 0, len(titles))
		for _, title := range titles {
			if s.Visibility.TitleAllowed(viewer, &title) {
				out = append(out, title.Key)
			}
		}
		return out, nil
	case core.KindCollection, core.KindSmart:
		if s.Collections == nil {
			return nil, core.ErrNotFound
		}
		page, err := s.Collections.Items(ctx, viewer, key, 0, 500)
		if err != nil {
			return nil, err
		}
		var out []core.RatingKey
		for _, title := range page.Items {
			if keys, err := s.Expand(ctx, viewer, title.Key); err == nil {
				out = append(out, keys...)
			}
		}
		return out, nil
	case core.KindPlaylist:
		playlist, err := s.Get(ctx, viewer, key)
		if err != nil {
			return nil, err
		}
		return append([]core.RatingKey(nil), playlist.Items...), nil
	}
	return []core.RatingKey{key}, nil
}
