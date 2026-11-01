package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type storedPlaylists struct {
	list    core.Playlist
	removed int
	marked  *core.Playlist
}

func (p *storedPlaylists) List(context.Context, int64) ([]core.Playlist, error) {
	return []core.Playlist{p.list}, nil
}
func (p *storedPlaylists) Get(context.Context, int64, core.RatingKey) (*core.Playlist, error) {
	copy := p.list
	return &copy, nil
}
func (p *storedPlaylists) Create(context.Context, int64, string, []core.RatingKey) (*core.Playlist, error) {
	return &p.list, nil
}
func (p *storedPlaylists) Add(context.Context, int64, core.RatingKey, []core.RatingKey) error {
	return nil
}
func (p *storedPlaylists) Remove(_ context.Context, _ int64, _ core.RatingKey, index int) error {
	p.removed = index
	return nil
}
func (p *storedPlaylists) Move(context.Context, int64, core.RatingKey, int, int) error { return nil }
func (p *storedPlaylists) Delete(context.Context, int64, core.RatingKey) error         { return nil }
func (p *storedPlaylists) MarkedPlaylist(context.Context, int64, string) (*core.Playlist, error) {
	if p.marked == nil {
		return nil, core.ErrNotFound
	}
	return p.marked, nil
}
func (p *storedPlaylists) EnsureMarkedPlaylist(context.Context, int64, string, string) (*core.Playlist, error) {
	return p.marked, nil
}

func TestPlaylistLibraryHidesTitlesAndKeepsVisibleIndexes(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	visible := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Seen"}
	hidden := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Hidden", Adult: true}
	pk := core.PlaylistKey(4)
	store := &storedPlaylists{list: core.Playlist{Key: pk, UserID: 1, Title: "Mine", Items: []core.RatingKey{hidden.Key, visible.Key}}}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		switch k {
		case visible.Key:
			return &visible, nil
		case hidden.Key:
			return &hidden, nil
		default:
			return nil, core.ErrNotFound
		}
	}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: get, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Playlists: store}}
	page, err := s.Playlists(ctx, v, frontend.PageQuery{})
	if err != nil || page.Total != 1 || len(page.Items[0].Items) != 1 || page.Items[0].Items[0] != visible.Key {
		t.Fatalf("list %+v %v", page, err)
	}
	if err = s.RemovePlaylistItem(ctx, v, pk, 0); err != nil || store.removed != 1 {
		t.Fatalf("remove visible index mapped to %d: %v", store.removed, err)
	}
	if _, err = s.CreatePlaylist(ctx, v, "Nope", []core.RatingKey{hidden.Key}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	store.list.Smart = true
	if err = s.DeletePlaylist(ctx, v, pk); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	store.list.Smart = false
	store.marked = &core.Playlist{Key: pk, UserID: 1}
	if err = s.DeletePlaylist(ctx, v, pk); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}
