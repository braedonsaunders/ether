package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMediaBrowserRealExperience(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(23)
	title := core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 23}, Title: "Fixture Film", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var signed struct{ AccessToken string }
			if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil {
				t.Fatal(err)
			}
			views := call("GET", "/UserViews", "", signed.AccessToken)
			if views.Code != 200 || !strings.Contains(views.Body.String(), "Movies") {
				t.Fatalf("views %d %s", views.Code, views.Body)
			}
			view, _ := ids.GUID(ids.View, 1)
			list := call("GET", "/Items?ParentId="+view, "", signed.AccessToken)
			if list.Code != 200 || !strings.Contains(list.Body.String(), "Fixture Film") {
				t.Fatalf("list %d %s", list.Code, list.Body)
			}
			watched := call("POST", "/UserPlayedItems/"+ids.RatingKeyGUID(key), "", signed.AccessToken)
			if watched.Code != 200 || !strings.Contains(watched.Body.String(), `"Played":true`) {
				t.Fatalf("watched %d %s", watched.Code, watched.Body)
			}
		})
	}
}

var errMetadataDown = errors.New("metadata down")

// searchDown refuses every remote catalogue call. Keyword search must still answer.
type searchDown struct{}

func (searchDown) MovieDetails(context.Context, int64) (*tmdb.MovieDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) TVDetails(context.Context, int64) (*tmdb.TVDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) SeasonDetails(context.Context, int64, int) (*tmdb.SeasonDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) EpisodeExternalIDs(context.Context, int64, int, int) (*tmdb.ExternalIDs, error) {
	return nil, errMetadataDown
}
func (searchDown) CollectionDetails(context.Context, int64) (*tmdb.CollectionDetails, error) {
	return nil, errMetadataDown
}
func (searchDown) DiscoverMovies(context.Context, tmdb.DiscoverParams) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) DiscoverTV(context.Context, tmdb.DiscoverParams) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchMovies(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchTV(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) SearchMulti(context.Context, string, int) (*tmdb.Paged[tmdb.MultiResult], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingMovies(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingTV(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) TrendingAll(context.Context, string, int) (*tmdb.Paged[tmdb.MultiResult], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieList(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TVList(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieSimilar(context.Context, int64, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) MovieRecommendations(context.Context, int64, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errMetadataDown
}
func (searchDown) TVSimilar(context.Context, int64, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) TVRecommendations(context.Context, int64, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errMetadataDown
}
func (searchDown) FindByExternalID(context.Context, string, string) (*tmdb.FindResult, error) {
	return nil, errMetadataDown
}
func (searchDown) Genres(context.Context, string) ([]tmdb.Genre, error) {
	return nil, errMetadataDown
}

var _ catalog.TMDB = searchDown{}

func TestMediaBrowserSearchAnswersWhenMetadataIsDown(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	show := core.Title{Key: core.ShowKey(8), Type: core.TypeShow, Title: "Fixture Show", Released: released}
	episode := core.Title{Key: core.EpisodeKey(8, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1, Title: "Pilot Unique", Released: released}
	movie := core.Title{Key: core.MovieKey(23), Type: core.TypeMovie, IDs: core.IDs{TMDB: 23}, Title: "Fixture Film", Released: released}
	for _, title := range []core.Title{show, episode, movie} {
		if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	a.catalog.SetTMDB(searchDown{})
	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var signed struct{ AccessToken string }
			if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil {
				t.Fatal(err)
			}
			hints := call("GET", "/Search/Hints?searchTerm=Fixture&Limit=20", "", signed.AccessToken)
			if hints.Code != 200 || !strings.Contains(hints.Body.String(), "Fixture Film") {
				t.Fatalf("hints %d %s", hints.Code, hints.Body)
			}
			episodeHints := call("GET", "/Search/Hints?searchTerm=Pilot%20Unique&Limit=20", "", signed.AccessToken)
			if episodeHints.Code != 200 || !strings.Contains(episodeHints.Body.String(), "Pilot Unique") {
				t.Fatalf("episode hints %d %s", episodeHints.Code, episodeHints.Body)
			}
			miss := call("GET", "/Search/Hints?searchTerm=zzzz-not-a-title&Limit=20", "", signed.AccessToken)
			if miss.Code != 200 || !strings.Contains(miss.Body.String(), `"SearchHints":[]`) && !strings.Contains(miss.Body.String(), `"SearchHints":null`) {
				t.Fatalf("empty hints %d %s", miss.Code, miss.Body)
			}
			if surface == "jellyfin" {
				id := ids.RatingKeyGUID(movie.Key)
				before := call("GET", "/Items/"+id, "", signed.AccessToken)
				if before.Code != 200 || !strings.Contains(before.Body.String(), `"Played":false`) {
					t.Fatalf("detail %d %s", before.Code, before.Body)
				}
				played := call("POST", "/UserPlayedItems/"+id, "", signed.AccessToken)
				if played.Code != 200 {
					t.Fatalf("played %d %s", played.Code, played.Body)
				}
				after := call("GET", "/Items/"+id, "", signed.AccessToken)
				if after.Code != 200 || !strings.Contains(after.Body.String(), `"Played":true`) {
					t.Fatalf("stale detail %d %s", after.Code, after.Body)
				}
			}
		})
	}
}
