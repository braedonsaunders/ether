package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestPersonalHomeViewsAreStableRankedAndAttributed(t *testing.T) {
	played := map[frontend.RatingKey]bool{2: true}
	hidden := map[int64]bool{}
	var interactions []frontend.RowInteraction
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{{Key: 9, Type: "movie", Title: "Library order"}}, Total: 1}, nil
		},
		RowsFunc: func(_ context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
			if req.Viewer == nil || req.Device.ID == "" || req.Placement != frontend.PlacementHome {
				t.Fatalf("row request %+v", req)
			}
			first := frontend.Title{Key: 2, Type: "movie", Title: "Zebra"}
			second := frontend.Title{Key: 3, Type: "movie", Title: "Apple"}
			third := frontend.Title{Key: 4, Type: "show", Title: "Shown"}
			if hidden[req.Viewer.ID] {
				second = frontend.Title{}
			}
			because := frontend.Row{ID: "home.because.dune", Title: "Because you watched Dune", Kind: "because-you-watched", Purpose: frontend.RowRecommendation, Handle: "because", Rank: 1, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &first}, {Kind: frontend.RowEntryTitle, Title: &second}}}
			trending := frontend.Row{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Handle: "trend", Rank: 0, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &third}}}
			skipped := frontend.Row{ID: "home.continue", Title: "Continue Watching", Kind: "continue-watching", Purpose: frontend.RowContinue, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &second}}}
			return []frontend.Row{trending, because, skipped}, nil
		},
		UserDataFunc: func(_ context.Context, _ *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			out := map[frontend.RatingKey]frontend.WatchStatus{}
			for _, key := range keys {
				if played[key] {
					out[key] = frontend.WatchStatus{ViewCount: 1}
				}
			}
			return out, nil
		},
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			if req.Key != 3 {
				t.Fatalf("detail key %d", req.Key)
			}
			return frontend.ItemDetail{Title: frontend.Title{Key: 3, Type: "movie", Title: "Apple"}}, nil
		},
		ReportRowInteractionFunc: func(_ context.Context, interaction frontend.RowInteraction) error {
			interactions = append(interactions, interaction)
			return nil
		},
	}
	viewer := func(id int64) frontend.Viewer { return frontend.Viewer{ID: id, Username: "ada"} }
	saved := map[string]json.RawMessage{}
	prefs := frontendtest.Preferences{
		GetFunc: func(_ context.Context, v *frontend.Viewer, client, key string) (json.RawMessage, error) {
			body, ok := saved[client+"\n"+key+"\n"+string(rune(v.ID))]
			if !ok {
				return nil, frontend.ErrPreferencesNotFound
			}
			return body, nil
		},
		PutFunc: func(_ context.Context, v *frontend.Viewer, client, key string, body json.RawMessage) error {
			saved[client+"\n"+key+"\n"+string(rune(v.ID))] = append(json.RawMessage(nil), body...)
			return nil
		},
	}
	mount := func(id int64, settings frontend.Settings) frontend.Instance {
		ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			v := viewer(id)
			return &v, nil
		}}
		inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Preferences: prefs, Sessions: frontendtest.Sessions{}, Config: func() frontend.Settings { return settings }})
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	call := func(inst frontend.Instance, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("X-Emby-Device-Id", "living-room")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	owner := mount(1, nil)
	views := waitFor(t, func() *httptest.ResponseRecorder { return call(owner, http.MethodGet, "/UserViews", "") }, "Trending")
	var listed struct {
		Items            []map[string]any
		TotalRecordCount float64
	}
	if views.Code != 200 || json.Unmarshal(views.Body.Bytes(), &listed) != nil || len(listed.Items) != 3 || listed.TotalRecordCount != 3 {
		t.Fatal(views.Code, views.Body.String())
	}
	if listed.Items[0]["Name"] != "Trending" || listed.Items[1]["Name"] != "Dune and Similar" || listed.Items[2]["Name"] != "Movies" {
		t.Fatal(listed.Items[0]["Name"], listed.Items[1]["Name"], listed.Items[2]["Name"])
	}
	if listed.Items[0]["CollectionType"] != "tvshows" || listed.Items[1]["CollectionType"] != "movies" || listed.Items[1]["Type"] != "CollectionFolder" {
		t.Fatal(listed.Items[0]["CollectionType"], listed.Items[1])
	}
	trendingID, _ := listed.Items[0]["Id"].(string)
	becauseID, _ := listed.Items[1]["Id"].(string)
	again := call(owner, http.MethodGet, "/UserViews", "")
	var listedAgain struct{ Items []map[string]any }
	if json.Unmarshal(again.Body.Bytes(), &listedAgain) != nil || listedAgain.Items[0]["Id"] != trendingID || listedAgain.Items[1]["Id"] != becauseID {
		t.Fatal("ids changed", again.Body.String())
	}
	wantBecause := personalViewNumber(1, "home.because.dune")
	gotBecause, err := ids.ParseGUID(ids.View, becauseID)
	if err != nil || gotBecause != wantBecause || !ids.PersonalView(gotBecause) {
		t.Fatal(gotBecause, wantBecause, err)
	}
	other := mount(7, nil)
	otherViews := waitFor(t, func() *httptest.ResponseRecorder { return call(other, http.MethodGet, "/UserViews", "") }, "Trending")
	var otherListed struct{ Items []map[string]any }
	if json.Unmarshal(otherViews.Body.Bytes(), &otherListed) != nil || otherListed.Items[0]["Id"] == trendingID {
		t.Fatal("viewer ids collided", otherViews.Body.String())
	}
	latest := call(owner, http.MethodGet, "/Items/Latest?ParentId="+becauseID+"&Limit=10", "")
	var latestItems []map[string]any
	if latest.Code != 200 || json.Unmarshal(latest.Body.Bytes(), &latestItems) != nil || len(latestItems) != 1 || latestItems[0]["Name"] != "Apple" {
		t.Fatal(latest.Code, latest.Body.String())
	}
	browse := call(owner, http.MethodGet, "/Items?ParentId="+becauseID+"&SortBy=SortName&SortOrder=Ascending", "")
	var browsed struct{ Items []map[string]any }
	if browse.Code != 200 || json.Unmarshal(browse.Body.Bytes(), &browsed) != nil || len(browsed.Items) != 2 || browsed.Items[0]["Name"] != "Zebra" || browsed.Items[1]["Name"] != "Apple" {
		t.Fatal(browse.Code, browse.Body.String())
	}
	detail := call(owner, http.MethodGet, "/Items/"+browsed.Items[1]["Id"].(string), "")
	var item map[string]any
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &item) != nil || item["Name"] != "Apple" || item["Id"] != browsed.Items[1]["Id"] {
		t.Fatal(detail.Code, detail.Body.String())
	}
	folder := call(owner, http.MethodGet, "/Items/"+becauseID, "")
	var folderItem map[string]any
	if folder.Code != 200 || json.Unmarshal(folder.Body.Bytes(), &folderItem) != nil || folderItem["Name"] != "Dune and Similar" || folderItem["ChildCount"] != float64(2) {
		t.Fatal(folder.Code, folder.Body.String())
	}
	sawImpression, sawPlay := false, false
	for _, interaction := range interactions {
		if interaction.Handle == "because" && interaction.Kind == frontend.RowImpression && interaction.Protocol == "jellyfin" {
			sawImpression = true
		}
	}
	playing := call(owner, http.MethodPost, "/Sessions/Playing", `{"PlaySessionId":"s","ItemId":"`+browsed.Items[1]["Id"].(string)+`"}`)
	if playing.Code != 204 {
		t.Fatal(playing.Code, playing.Body.String())
	}
	for _, interaction := range interactions {
		if interaction.Handle == "because" && interaction.Kind == frontend.RowPlay {
			sawPlay = true
		}
	}
	if !sawImpression || !sawPlay {
		t.Fatal(interactions)
	}
	hidden[2] = true
	restricted := mount(2, nil)
	restrictedLatest := call(restricted, http.MethodGet, "/Items/Latest?ParentId="+becauseID, "")
	if restrictedLatest.Code != 404 {
		t.Fatal(restrictedLatest.Code, restrictedLatest.Body.String())
	}
	ownLatest := waitFor(t, func() *httptest.ResponseRecorder { return call(restricted, http.MethodGet, "/UserViews", "") }, "Dune and Similar")
	var restrictedViews struct{ Items []map[string]any }
	if json.Unmarshal(ownLatest.Body.Bytes(), &restrictedViews) != nil {
		t.Fatal(ownLatest.Body.String())
	}
	for _, view := range restrictedViews.Items {
		if view["Id"] == becauseID {
			t.Fatal("restricted viewer inherited another viewer's row id")
		}
	}
	restrictedBrowse := call(restricted, http.MethodGet, "/Items?ParentId="+restrictedViews.Items[1]["Id"].(string), "")
	var restrictedItems struct{ Items []map[string]any }
	if json.Unmarshal(restrictedBrowse.Body.Bytes(), &restrictedItems) != nil || len(restrictedItems.Items) != 1 || restrictedItems.Items[0]["Name"] != "Zebra" {
		t.Fatal(restrictedBrowse.Body.String())
	}
	prefsBody := waitFor(t, func() *httptest.ResponseRecorder {
		return call(owner, http.MethodGet, "/DisplayPreferences/usersettings?client=jellyfin-web", "")
	}, "latestmedia")
	var display map[string]any
	if prefsBody.Code != 200 || json.Unmarshal(prefsBody.Body.Bytes(), &display) != nil || display["CustomPrefs"].(map[string]any)["homesection0"] != "latestmedia" {
		t.Fatal(prefsBody.Code, prefsBody.Body.String())
	}
	if savedPrefs := call(owner, http.MethodPost, "/DisplayPreferences/usersettings?client=jellyfin-web", `{"CustomPrefs":{"homesection0":"resume"}}`); savedPrefs.Code != 204 {
		t.Fatal(savedPrefs.Code, savedPrefs.Body.String())
	}
	kept := call(owner, http.MethodGet, "/DisplayPreferences/usersettings?client=jellyfin-web", "")
	if json.Unmarshal(kept.Body.Bytes(), &display) != nil || display["CustomPrefs"].(map[string]any)["homesection0"] != "resume" || display["CustomPrefs"].(map[string]any)["homesection1"] != nil {
		t.Fatal(kept.Body.String())
	}
	userID, err := ids.GUID(ids.User, 1)
	if err != nil {
		t.Fatal(err)
	}
	viewerEight := mount(8, nil)
	var me *httptest.ResponseRecorder
	var user struct{ Configuration map[string]any }
	var order []any
	deadline := time.Now().Add(2 * time.Second)
	for {
		me = call(viewerEight, http.MethodGet, "/Users/Me", "")
		order = nil
		if me.Code == 200 && json.Unmarshal(me.Body.Bytes(), &user) == nil {
			order, _ = user.Configuration["OrderedViews"].([]any)
		}
		if len(order) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(me.Code, me.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if order[0] == "Movies" {
		t.Fatal(user.Configuration["OrderedViews"])
	}
	libraryID, err := ids.GUID(ids.View, 1)
	if err != nil {
		t.Fatal(err)
	}
	chosen := call(owner, http.MethodPost, "/Users/"+userID+"/Configuration", `{"OrderedViews":["`+libraryID+`"],"HidePlayedInLatest":false}`)
	if chosen.Code != 204 {
		t.Fatal(chosen.Code, chosen.Body.String())
	}
	after := call(owner, http.MethodGet, "/Users/Me", "")
	if json.Unmarshal(after.Body.Bytes(), &user) != nil || len(user.Configuration["OrderedViews"].([]any)) != 1 || user.Configuration["OrderedViews"].([]any)[0] != libraryID || user.Configuration["HidePlayedInLatest"] != false {
		t.Fatal(after.Body.String())
	}
	movies := call(owner, http.MethodGet, "/Movies/Recommendations", "")
	var categories []map[string]any
	if movies.Code != 200 || json.Unmarshal(movies.Body.Bytes(), &categories) != nil || len(categories) != 1 || categories[0]["RecommendationType"] != "SimilarToRecentlyPlayed" || categories[0]["BaselineItemName"] != "Dune" {
		t.Fatal(movies.Code, movies.Body.String())
	}
	shows := call(owner, http.MethodGet, "/Shows/Recommendations", "")
	if shows.Code != 200 || json.Unmarshal(shows.Body.Bytes(), &categories) != nil || len(categories) != 1 || categories[0]["RecommendationType"] != "SimilarToLikedItem" || categories[0]["Items"].([]any)[0].(map[string]any)["Name"] != "Shown" {
		t.Fatal(shows.Code, shows.Body.String())
	}
	off := mount(1, frontend.Settings{"homeRows": false})
	offViews := call(off, http.MethodGet, "/UserViews", "")
	var onlyLibrary struct{ Items []map[string]any }
	if json.Unmarshal(offViews.Body.Bytes(), &onlyLibrary) != nil || len(onlyLibrary.Items) != 1 || onlyLibrary.Items[0]["Name"] != "Movies" {
		t.Fatal(offViews.Body.String())
	}
	if err := (Jellyfin{}).ValidateSettings(frontend.Settings{"homeRows": false, "homeRowCount": float64(4), "homeRowKinds": []any{"trending"}}); err != nil {
		t.Fatal(err)
	}
	if (Jellyfin{}).ValidateSettings(frontend.Settings{"homeRowCount": float64(99)}) == nil || (Emby{}).ValidateSettings(frontend.Settings{"extra": true}) == nil {
		t.Fatal("invalid personalization settings accepted")
	}
}

func TestPersonalViewIDsDoNotCollideWithLibraries(t *testing.T) {
	for _, library := range []int64{0, 1, 2, 42, 1 << 20} {
		if ids.PersonalView(library) {
			t.Fatal(library)
		}
	}
	a := personalViewNumber(1, "home.trending")
	b := personalViewNumber(1, "home.trending")
	c := personalViewNumber(2, "home.trending")
	d := personalViewNumber(1, "home.because.dune")
	if a != b || a == c || a == d || !ids.PersonalView(a) || !ids.PersonalView(c) {
		t.Fatal(a, b, c, d)
	}
}

func TestNowAvailableLibraryDoesNotUseThePersonalizedRowBudget(t *testing.T) {
	ready := frontend.Title{Key: 8, Type: "movie", Title: "Ready"}
	shown := frontend.Title{Key: 4, Type: "show", Title: "Shown"}
	exp := frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
	}, RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{
			{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &shown}}},
			{ID: "home.because.dune", Title: "Because you watched Dune", Kind: "because-you-watched", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &shown}}},
			{ID: "watchlist.available", Title: "Now available", Purpose: frontend.RowWatchlist, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &ready}}},
		}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada"}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{}, Config: func() frontend.Settings {
		return frontend.Settings{"homeRowCount": float64(1), "homeRowKinds": []any{"trending"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := waitFor(t, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("X-Emby-Device-Id", "living-room")
		inst.Handler().ServeHTTP(rec, r)
		return rec
	}, "Now available")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Name":"Trending"`) || !strings.Contains(w.Body.String(), `"Name":"Now available"`) || strings.Contains(w.Body.String(), "Dune and Similar") {
		t.Fatalf("views %d %s", w.Code, w.Body.String())
	}
}

func waitFor(t *testing.T, call func() *httptest.ResponseRecorder, needle string) *httptest.ResponseRecorder {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last *httptest.ResponseRecorder
	for {
		last = call()
		if last.Code == 200 && strings.Contains(last.Body.String(), needle) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %s: %d %s", needle, last.Code, last.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUserAndLibraryListsDoNotWaitForRows(t *testing.T) {
	block := make(chan struct{})
	title := frontend.Title{Key: 4, Type: "show", Title: "Shown"}
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			<-block
			return []frontend.Row{{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &title}}}}, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada"}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("X-Emby-Device-Id", "living-room")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- call("/UserViews") }()
	go func() { done <- call("/Users/Me") }()
	timeout := time.After(200 * time.Millisecond)
	for n := 0; n < 2; n++ {
		select {
		case w := <-done:
			if w.Code != 200 || strings.Contains(w.Body.String(), "Trending") {
				t.Fatalf("cold list did row work: %d %s", w.Code, w.Body.String())
			}
		case <-timeout:
			t.Fatal("user list waited on personalized rows")
		}
	}
	close(block)
	warm := waitFor(t, func() *httptest.ResponseRecorder { return call("/UserViews") }, "Trending")
	if warm.Code != 200 {
		t.Fatal(warm.Code)
	}
}

type savedHomeStore struct {
	body []byte
}

func (s savedHomeStore) LoadPersonalHome(context.Context, string, int64, string) ([]byte, error) {
	return append([]byte(nil), s.body...), nil
}
func (s savedHomeStore) SavePersonalHome(context.Context, string, int64, string, []byte) error {
	return nil
}
func (s savedHomeStore) PersonalHomeViewers(context.Context) ([]frontend.Viewer, error) {
	return []frontend.Viewer{{ID: 1, Username: "ada"}}, nil
}

func TestPersonalHomeFirstFetchServesSavedShelves(t *testing.T) {
	block := make(chan struct{})
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			<-block
			return nil, context.Canceled
		},
	}
	body, err := json.Marshal([]storedPersonalShelf{{Number: 1, WireID: "saved-shelf", Name: "Your 2025 in Review", Kind: "movies", Count: 4}})
	if err != nil {
		t.Fatal(err)
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada"}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{}, PersonalHomes: savedHomeStore{body: body},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(block) })
	raw, ok := inst.(*instance)
	if !ok {
		t.Fatal("instance type")
	}
	raw.PreparePersonalHomes(context.Background())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "living-room")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Your 2025 in Review") || !strings.Contains(w.Body.String(), "Movies") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPersonalLatestSharesOneHomeLoad(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	title := frontend.Title{Key: 3, Type: "movie", Title: "Apple"}
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			calls.Add(1)
			once.Do(func() { close(started) })
			<-release
			return []frontend.Row{{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Handle: "trend", Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &title}}}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada"}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ids.GUID(ids.View, personalViewNumber(1, "home.trending"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	for n := 0; n < 2; n++ {
		go func(n int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/Items/Latest?ParentId="+parent+"&Limit=10", nil)
			r.Header.Set("X-Emby-Token", "grant")
			r.Header.Set("X-Emby-Device-Id", "living-room")
			inst.Handler().ServeHTTP(w, r)
			codes[n] = w.Code
		}(n)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("home load did not start")
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || calls.Load() != 1 {
		t.Fatalf("codes %v rows %d", codes, calls.Load())
	}
}

type snapshotHome struct {
	frontendtest.Experience
	snaps atomic.Int32
}

func (s *snapshotHome) SnapshotRows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	s.snaps.Add(1)
	title := frontend.Title{Key: 3, Type: "movie", Title: "Apple"}
	return []frontend.Row{{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &title}}}}, nil
}

func TestPersonalLatestSnapshotsThenIssues(t *testing.T) {
	previous := personalHandleDelay
	personalHandleDelay = 30 * time.Millisecond
	t.Cleanup(func() { personalHandleDelay = previous })
	var issued atomic.Int32
	var impressions atomic.Int32
	title := frontend.Title{Key: 3, Type: "movie", Title: "Apple"}
	exp := &snapshotHome{Experience: frontendtest.Experience{
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			issued.Add(1)
			return []frontend.Row{{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Handle: "trend", Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &title}}}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
		ReportRowInteractionFunc: func(_ context.Context, interaction frontend.RowInteraction) error {
			if interaction.Handle == "trend" && interaction.Kind == frontend.RowImpression {
				impressions.Add(1)
			}
			return nil
		},
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada"}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ids.GUID(ids.View, personalViewNumber(1, "home.trending"))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/Latest?ParentId="+parent+"&Limit=10", nil)
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "living-room")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || exp.snaps.Load() != 1 || issued.Load() != 0 || !strings.Contains(w.Body.String(), "Apple") {
		t.Fatalf("code %d snaps %d issued %d body %s", w.Code, exp.snaps.Load(), issued.Load(), w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (issued.Load() != 1 || impressions.Load() != 1) {
		time.Sleep(10 * time.Millisecond)
	}
	if issued.Load() != 1 || impressions.Load() != 1 {
		t.Fatalf("issued %d impressions %d", issued.Load(), impressions.Load())
	}
}
