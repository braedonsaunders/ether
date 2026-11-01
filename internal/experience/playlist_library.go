package experience

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Playlists lists the viewer's own playlists. Membership is the titles this
// viewer is allowed to see.
func (s *Service) Playlists(ctx context.Context, viewer *core.Viewer, q frontend.PageQuery) (frontend.PlaylistPage, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.PlaylistPage{}, err
	}
	if s.d.Playlists == nil {
		return frontend.PlaylistPage{}, nil
	}
	lists, err := s.d.Playlists.List(ctx, v.ID)
	if err != nil {
		return frontend.PlaylistPage{}, err
	}
	out := make([]core.Playlist, 0, len(lists))
	for _, playlist := range lists {
		if playlist.UserID != v.ID || playlist.Key == 0 {
			continue
		}
		visible, _, err := s.visiblePlaylistIndexes(ctx, v, playlist.Items)
		if err != nil {
			return frontend.PlaylistPage{}, err
		}
		playlist.Items = visible
		out = append(out, playlist)
	}
	start, end := playlistPage(len(out), q.Offset, q.Limit)
	return frontend.PlaylistPage{Items: out[start:end], Total: len(out)}, nil
}

func (s *Service) CreatePlaylist(ctx context.Context, viewer *core.Viewer, name string, items []core.RatingKey) (*core.Playlist, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if s.d.Playlists == nil || name == "" || len(name) > 200 {
		return nil, frontend.ErrBadPlaylist
	}
	if err := s.playlistTitles(ctx, v, items); err != nil {
		return nil, err
	}
	return s.d.Playlists.Create(ctx, v.ID, name, items)
}

func (s *Service) AddPlaylistItems(ctx context.Context, viewer *core.Viewer, key core.RatingKey, items []core.RatingKey) error {
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return frontend.ErrBadPlaylist
	}
	if err := s.playlistTitles(ctx, v, items); err != nil {
		return err
	}
	return s.d.Playlists.Add(ctx, v.ID, playlist.Key, items)
}

func (s *Service) RemovePlaylistItem(ctx context.Context, viewer *core.Viewer, key core.RatingKey, index int) error {
	v, playlist, raw, err := s.visiblePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(raw) {
		return frontend.ErrBadPlaylist
	}
	return s.d.Playlists.Remove(ctx, v.ID, playlist.Key, raw[index])
}

func (s *Service) MovePlaylistItem(ctx context.Context, viewer *core.Viewer, key core.RatingKey, from, to int) error {
	v, playlist, raw, err := s.visiblePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	if from < 0 || to < 0 || from >= len(raw) || to >= len(raw) {
		return frontend.ErrBadPlaylist
	}
	if from == to {
		return nil
	}
	return s.d.Playlists.Move(ctx, v.ID, playlist.Key, raw[from], raw[to])
}

func (s *Service) DeletePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error {
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return err
	}
	return s.d.Playlists.Delete(ctx, v.ID, playlist.Key)
}

func (s *Service) mutablePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.User, *core.Playlist, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, nil, err
	}
	playlist, err := s.ownedPlaylist(ctx, v, key)
	if err != nil {
		return nil, nil, err
	}
	if s.playlistProtected(ctx, v, playlist) {
		return nil, nil, core.ErrUnauthorized
	}
	return v, playlist, nil
}

func (s *Service) visiblePlaylist(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.User, *core.Playlist, []int, error) {
	v, playlist, err := s.mutablePlaylist(ctx, viewer, key)
	if err != nil {
		return nil, nil, nil, err
	}
	_, raw, err := s.visiblePlaylistIndexes(ctx, v, playlist.Items)
	if err != nil {
		return nil, nil, nil, err
	}
	return v, playlist, raw, nil
}

func (s *Service) ownedPlaylist(ctx context.Context, v *core.User, key core.RatingKey) (*core.Playlist, error) {
	if s.d.Playlists == nil || key.Kind() != core.KindPlaylist {
		return nil, core.ErrNotFound
	}
	playlist, err := s.d.Playlists.Get(ctx, v.ID, key)
	if err != nil || playlist == nil || playlist.UserID != v.ID || playlist.Key != key {
		return nil, core.ErrNotFound
	}
	return playlist, nil
}

func (s *Service) playlistProtected(ctx context.Context, v *core.User, playlist *core.Playlist) bool {
	if playlist == nil || playlist.Smart {
		return true
	}
	marked, ok := s.d.Playlists.(core.MarkedPlaylists)
	if !ok {
		return false
	}
	favorite, err := marked.MarkedPlaylist(ctx, v.ID, "favorites")
	return err == nil && favorite != nil && favorite.Key == playlist.Key
}

func (s *Service) playlistTitles(ctx context.Context, v *core.User, items []core.RatingKey) error {
	for _, key := range items {
		if _, err := s.item(ctx, v, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) visiblePlaylistIndexes(ctx context.Context, v *core.User, items []core.RatingKey) ([]core.RatingKey, []int, error) {
	visible := make([]core.RatingKey, 0, len(items))
	raw := make([]int, 0, len(items))
	for i, key := range items {
		_, err := s.item(ctx, v, key)
		if err == nil {
			visible = append(visible, key)
			raw = append(raw, i)
			continue
		}
		if err != core.ErrNotFound {
			return nil, nil, err
		}
	}
	return visible, raw, nil
}

func playlistPage(n, offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset > n {
		offset = n
	}
	end := offset + limit
	if end > n {
		end = n
	}
	return offset, end
}
