package silo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type siloPlaylists struct {
	frontendtest.Experience
	playlist core.Playlist
}

func (p *siloPlaylists) Playlists(context.Context, *frontend.Viewer, frontend.PageQuery) (frontend.PlaylistPage, error) {
	if p.playlist.Key == 0 {
		return frontend.PlaylistPage{}, nil
	}
	return frontend.PlaylistPage{Items: []core.Playlist{p.playlist}, Total: 1}, nil
}
func (p *siloPlaylists) CreatePlaylist(_ context.Context, v *frontend.Viewer, name string, items []core.RatingKey) (*core.Playlist, error) {
	p.playlist = core.Playlist{Key: core.PlaylistKey(4), UserID: v.ID, Title: name, Items: items}
	return &p.playlist, nil
}
func (p *siloPlaylists) AddPlaylistItems(context.Context, *frontend.Viewer, core.RatingKey, []core.RatingKey) error {
	return nil
}
func (p *siloPlaylists) RemovePlaylistItem(context.Context, *frontend.Viewer, core.RatingKey, int) error {
	return nil
}
func (p *siloPlaylists) MovePlaylistItem(context.Context, *frontend.Viewer, core.RatingKey, int, int) error {
	return nil
}
func (p *siloPlaylists) DeletePlaylist(context.Context, *frontend.Viewer, core.RatingKey) error {
	return nil
}

func TestSiloPlaylists(t *testing.T) {
	movie := core.MovieKey(1)
	exp := &siloPlaylists{}
	exp.ItemFunc = func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key == exp.playlist.Key {
			copy := exp.playlist
			return frontend.ItemDetail{Playlist: &copy}, nil
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Type: "movie", Title: "One"}}, nil
	}
	exp.ChildrenFunc = func(context.Context, *frontend.Viewer, frontend.Device, core.RatingKey, frontend.ItemQuery) (frontend.Page, error) {
		return frontend.Page{Items: []frontend.Title{{Key: movie, Type: "movie", Title: "One"}}, Total: 1}, nil
	}
	exp.UserDataFunc = func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 7}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v2/playlists", strings.NewReader(`{"name":"Queue","content_ids":["`+ids.RatingKeyGUID(movie)+`"]}`))
	r.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Queue"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	id := ids.RatingKeyGUID(exp.playlist.Key)
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/v2/playlists/"+id+"/items", nil)
	r.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"title":"One"`) {
		t.Fatal(w.Code, w.Body.String())
	}

}
