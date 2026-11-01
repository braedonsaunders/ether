package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type playlistExperience struct {
	frontendtest.Experience
	playlist core.Playlist
	removed  int
	movedTo  int
}

func (p *playlistExperience) Playlists(context.Context, *frontend.Viewer, frontend.PageQuery) (frontend.PlaylistPage, error) {
	return frontend.PlaylistPage{Items: []core.Playlist{p.playlist}, Total: 1}, nil
}
func (p *playlistExperience) CreatePlaylist(_ context.Context, v *frontend.Viewer, name string, items []core.RatingKey) (*core.Playlist, error) {
	p.playlist = core.Playlist{Key: core.PlaylistKey(9), UserID: v.ID, Title: name, Items: items}
	return &p.playlist, nil
}
func (p *playlistExperience) AddPlaylistItems(_ context.Context, _ *frontend.Viewer, _ core.RatingKey, items []core.RatingKey) error {
	p.playlist.Items = append(p.playlist.Items, items...)
	return nil
}
func (p *playlistExperience) RemovePlaylistItem(_ context.Context, _ *frontend.Viewer, _ core.RatingKey, index int) error {
	p.removed = index
	return nil
}
func (p *playlistExperience) MovePlaylistItem(_ context.Context, _ *frontend.Viewer, _ core.RatingKey, _, to int) error {
	p.movedTo = to
	return nil
}
func (p *playlistExperience) DeletePlaylist(context.Context, *frontend.Viewer, core.RatingKey) error {
	return nil
}

func TestJellyfinPlaylistMutationsAndFutureEpisodes(t *testing.T) {
	movie := frontend.Title{Key: core.MovieKey(1), Type: "movie", Title: "One"}
	show := core.ShowKey(4)
	future := frontend.Title{Key: core.EpisodeKey(4, 1, 8), Type: "episode", Title: "Later", Season: 1, Episode: 8, Released: time.Now().Add(48 * time.Hour), ShowKey: show}
	exp := &playlistExperience{playlist: core.Playlist{Key: core.PlaylistKey(9), UserID: 1, Title: "Queue", Items: []core.RatingKey{movie.Key}}}
	exp.ItemFunc = func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key == exp.playlist.Key {
			copy := exp.playlist
			return frontend.ItemDetail{Playlist: &copy}, nil
		}
		return frontend.ItemDetail{}, frontend.ErrNotFound
	}
	exp.ChildrenFunc = func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key core.RatingKey, _ frontend.ItemQuery) (frontend.Page, error) {
		if key == show {
			return frontend.Page{Items: []frontend.Title{future}, Total: 1}, nil
		}
		return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
	}
	exp.UserDataFunc = func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}
	exp.ViewsFunc = func(context.Context, *frontend.Viewer) ([]frontend.View, error) { return nil, nil }
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 1}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"Name":"Queue","Ids":["` + ids.RatingKeyGUID(movie.Key) + `"],"UserId":"1"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Playlists", strings.NewReader(body))
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("Content-Type", "application/json")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Type":"Playlist"`) || !strings.Contains(w.Body.String(), `"Name":"Queue"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	id := ids.RatingKeyGUID(exp.playlist.Key)
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/Playlists/"+id+"/Items?Ids="+ids.RatingKeyGUID(movie.Key), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Playlists/"+id+"/Items", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"PlaylistItemId"`) || !strings.Contains(w.Body.String(), `"Name":"One"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	entry := ids.RatingKeyGUID(movie.Key)
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodDelete, "/Playlists/"+id+"/Items?EntryIds="+entry, nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || exp.removed != 0 {
		t.Fatal(w.Code, exp.removed)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/Playlists/"+id+"/Items/"+entry+"/Move/0", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || exp.movedTo != 0 {
		t.Fatal(w.Code, exp.movedTo)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/Playlists", strings.NewReader(`{"Name":"Other","UserId":"2"}`))
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("Content-Type", "application/json")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Shows/"+ids.RatingKeyGUID(show)+"/Episodes", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var episodes struct {
		Items []struct {
			Name          string
			IsPlaceHolder bool
			PremiereDate  string
			MediaSources  []any
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &episodes) != nil || len(episodes.Items) != 1 || !episodes.Items[0].IsPlaceHolder || episodes.Items[0].PremiereDate == "" || len(episodes.Items[0].MediaSources) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}
