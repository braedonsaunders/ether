package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/store"
)

type peopleHTTPState struct {
	mu     sync.RWMutex
	viewer core.User
	active bool
	titles map[core.RatingKey]core.Title
}

func (s *peopleHTTPState) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.active || id != s.viewer.ID {
		return nil, core.ErrUnauthorized
	}
	v := s.viewer
	return &v, nil
}
func (s *peopleHTTPState) get(_ context.Context, key core.RatingKey) (*core.Title, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	title, ok := s.titles[key]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &title, nil
}
func (s *peopleHTTPState) update(key core.RatingKey, f func(*core.Title)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	title := s.titles[key]
	f(&title)
	s.titles[key] = title
}

type peopleUnusedState struct{ core.State }
type peopleHTTPArtwork struct {
	state      *peopleHTTPState
	beforeList func()
	beforeRead func()
}

func (a *peopleHTTPArtwork) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	if a.beforeList != nil {
		a.beforeList()
	}
	title, err := a.state.get(ctx, req.Item)
	if err != nil {
		return nil, false, err
	}
	images := []frontend.Image{}
	for _, person := range title.Cast {
		if person.TMDB > 0 && person.ThumbPath != "" {
			images = append(images, frontend.Image{Kind: frontend.ImagePerson, Subject: "tmdb:" + strconv.FormatInt(person.TMDB, 10), Path: "opaque-person-portrait", Tag: "v1", ThumbHash: "AQID"})
		}
	}
	return images, true, nil
}
func (a *peopleHTTPArtwork) Read(ctx context.Context, req frontend.ArtworkRequest, image frontend.Image, _ frontend.ArtworkTransform) ([]byte, string, error) {
	if a.beforeRead != nil {
		a.beforeRead()
	}
	images, _, err := a.List(ctx, req)
	if err != nil {
		return nil, "", err
	}
	for _, candidate := range images {
		if frontend.SameImage(candidate, image) {
			return []byte("person portrait"), "image/jpeg", nil
		}
	}
	return nil, "", core.ErrNotFound
}

func peopleHTTPFixture(t *testing.T) (*surfaceExperience, *peopleHTTPState, *peopleHTTPArtwork) {
	t.Helper()
	movie, show, episode := core.MovieKey(1), core.ShowKey(1), core.EpisodeKey(1, 1, 1)
	state := &peopleHTTPState{viewer: core.User{ID: 1}, active: true, titles: map[core.RatingKey]core.Title{
		movie:            {Key: movie, Type: core.TypeMovie, Cast: []core.Person{{TMDB: 10, Name: "Visible Person"}}},
		show:             {Key: show, Type: core.TypeShow},
		episode:          {Key: episode, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Cast: []core.Person{{TMDB: 30, Name: "Episode Person", IMDB: "nm30", ThumbPath: "known"}}},
		core.MovieKey(2): {Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true, Cast: []core.Person{{TMDB: 99, Name: "A Hidden Person"}}},
	}}
	deps := experience.ServiceDeps{
		Active: state.ActiveUser, Get: func(ctx context.Context, _ *core.User, key core.RatingKey) (*core.Title, error) {
			return state.get(ctx, key)
		}, CachedGet: state.get,
		CachedGetMany: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			out := map[core.RatingKey]core.Title{}
			for _, key := range keys {
				title, err := state.get(ctx, key)
				if err == nil {
					out[key] = *title
				} else if !errors.Is(err, core.ErrNotFound) {
					return nil, err
				}
			}
			return out, nil
		},
		CreditItems: func(_ context.Context, after core.RatingKey, limit int) ([]core.RatingKey, error) {
			state.mu.RLock()
			defer state.mu.RUnlock()
			keys := []core.RatingKey{}
			for key, title := range state.titles {
				if len(title.Cast)+len(title.Directors)+len(title.Writers)+len(title.Crew) > 0 {
					keys = append(keys, key)
				}
			}
			sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
			start := sort.Search(len(keys), func(i int) bool { return keys[i] > after })
			end := min(start+limit, len(keys))
			return keys[start:end], nil
		},
		Libraries: []experience.Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}},
		Visibility: experience.Visibility{SectionFor: func(kind core.MediaType) (int, bool) {
			if kind == core.TypeMovie {
				return 1, true
			}
			return 2, true
		}, GetTitle: state.get},
		Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
			panic("people HTTP used unfiltered browse fallback")
		},
		Grandchildren: func(_ context.Context, _ *core.User, key core.RatingKey) ([]core.Title, error) {
			state.mu.RLock()
			defer state.mu.RUnlock()
			items := []core.Title{}
			for _, title := range state.titles {
				if title.Type == core.TypeEpisode && title.ShowKey == key {
					items = append(items, title)
				}
			}
			return items, nil
		},
		Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil }, State: peopleUnusedState{}, Rows: &experience.RowsService{}, Protocol: "silo", Search: experience.Search{Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
			panic("people read reached provider search")
		}},
		Enrich: func(context.Context, *core.User, core.Device, *core.Title) (frontend.ItemDetail, error) {
			panic("people read reached playback/source enrichment")
		},
	}
	deps.BeginPeopleRead = func(ctx context.Context, viewer *core.User) (experience.PeopleRead, error) {
		owned, err := state.ActiveUser(ctx, viewer.ID)
		if err != nil {
			return experience.PeopleRead{}, err
		}
		return experience.PeopleRead{Viewer: owned, SectionFor: deps.Visibility.SectionFor, Policy: peoplePolicy{viewerID: state.viewer.ID, policy: safety.Policy{}}, Titles: deps.CachedGetMany, Finish: func(context.Context) error { return nil }, Close: func() {}}, nil
	}
	service, err := experience.NewService(deps)
	if err != nil {
		t.Fatal(err)
	}
	artwork := &peopleHTTPArtwork{state: state}
	return &surfaceExperience{Service: service, protocol: "silo", artwork: artwork, active: state.ActiveUser}, state, artwork
}

func TestSiloPeopleHTTPCurrentMembershipAndArtwork(t *testing.T) {
	exp, state, artwork := peopleHTTPFixture(t)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	device := core.Device{ID: "tv"}
	ids, err := exp.PersonFacetIDs(ctx, viewer, device, core.EpisodeKey(1, 1, 1), []int64{30})
	if err != nil {
		t.Fatal(err)
	}
	personID := ids[30]
	db := store.OpenTest(t)
	if _, err = db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, state, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	images, err := identity.NewArtworkImageGrantService(db, "silo", grants, func() frontend.Experience { return exp }, artwork, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := (silo.Frontend{MachineID: "people-test"}).Mount(ctx, frontend.Host{Experience: exp, ImageGrants: images, Identity: frontendtest.Identity{ResolveFunc: func(ctx context.Context, token string) (*core.Viewer, error) {
		return grants.Resolve(ctx, "silo", token)
	}}, Sessions: frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		panic("people HTTP reached source planning")
	}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, token, profile string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Silo-Device-Id", device.ID)
		if profile != "" {
			r.Header.Set("X-Profile-Id", profile)
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	list := call("/api/v2/catalog/people?media_scope=video_with_episodes&q=Person&limit=2", credential, "1")
	var collection struct {
		Items []map[string]any `json:"items"`
	}
	if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &collection) != nil || len(collection.Items) != 2 || strings.Contains(list.Body.String(), "Hidden") {
		t.Fatalf("visible list %d %s", list.Code, list.Body.String())
	}
	detail := call("/api/v2/catalog/people/"+personID+"?prefetch=false", credential, "1")
	var person map[string]any
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &person) != nil || person["id"] != personID || person["name"] != "Episode Person" || person["imdb_id"] != "nm30" {
		t.Fatalf("episode detail %d %s", detail.Code, detail.Body.String())
	}
	if strings.Contains(detail.Body.String(), "opaque-person") || strings.Contains(detail.Body.String(), credential) {
		t.Fatal("person response leaked internal image reference or credential")
	}
	for _, field := range []string{"bio", "birth_date", "death_date", "birthplace", "homepage"} {
		if _, ok := person[field]; ok {
			t.Fatalf("invented %s", field)
		}
	}
	photo, _ := person["photo_url"].(string)
	if photo == "" {
		t.Fatal("authorized photo absent")
	}
	if got := call(photo, "", ""); got.Code != 200 || got.Body.String() != "person portrait" {
		t.Fatalf("photo read %d %s", got.Code, got.Body.String())
	}
	for _, tc := range []struct {
		path, token, profile string
		status               int
	}{
		{"/api/v2/catalog/people", "", "1", 401}, {"/api/v2/catalog/people", credential, "2", 401}, {"/api/v2/catalog/people", credential, "", 422},
		{"/api/v2/catalog/people?limit=0", credential, "1", 400}, {"/api/v2/catalog/people?limit=101", credential, "1", 400}, {"/api/v2/catalog/people?media_scope=manga", credential, "1", 400}, {"/api/v2/catalog/people?media_scope=", credential, "1", 400},
		{"/api/v2/catalog/people/" + personID + "?prefetch=1", credential, "1", 400}, {"/api/v2/catalog/people/30", credential, "1", 404}, {"/api/v2/favorites/" + personID, credential, "1", 404},
	} {
		if got := call(tc.path, tc.token, tc.profile); got.Code != tc.status {
			t.Errorf("%s got%d want%d: %s", tc.path, got.Code, tc.status, got.Body.String())
		}
	}
	caps := call("/api/v2/catalog/search/capabilities", credential, "1")
	if caps.Code != 200 || !strings.Contains(caps.Body.String(), `"people_media_scope":true`) || !strings.Contains(caps.Body.String(), `"video_with_episodes_scope":false`) {
		t.Fatalf("overclaimed catalogue support %d %s", caps.Code, caps.Body.String())
	}
	state.update(core.EpisodeKey(1, 1, 1), func(title *core.Title) { title.Cast = nil })
	if got := call("/api/v2/catalog/people/"+personID, credential, "1"); got.Code != 404 {
		t.Fatalf("removed credit detail %d %s", got.Code, got.Body.String())
	}
	if got := call(photo, "", ""); got.Code == 200 || strings.Contains(got.Body.String(), "person portrait") {
		t.Fatalf("removed credit photo served %d %s", got.Code, got.Body.String())
	}
	state.mu.Lock()
	state.active = false
	state.mu.Unlock()
	if got := call("/api/v2/catalog/people", credential, "1"); got.Code != 401 {
		t.Fatalf("inactive viewer %d %s", got.Code, got.Body.String())
	}
}

func TestPersonArtworkRechecksCreditAfterDescriptorRead(t *testing.T) {
	exp, state, artwork := peopleHTTPFixture(t)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	episode := core.EpisodeKey(1, 1, 1)
	ids, err := exp.PersonFacetIDs(ctx, viewer, core.Device{}, episode, []int64{30})
	if err != nil {
		t.Fatal(err)
	}
	artwork.beforeList = func() { state.update(episode, func(title *core.Title) { title.Cast = nil }) }
	if got, err := exp.Person(ctx, viewer, core.Device{}, ids[30], frontend.PersonReadOptions{}); !errors.Is(err, core.ErrNotFound) || got.ID != "" || len(got.Images) != 0 {
		t.Fatalf("removed credit returned descriptor %+v %v", got, err)
	}
}

func TestProductionPeopleIgnoresStaleMovieCreditCache(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(777)
	title := core.Title{Key: key, Type: core.TypeMovie, Title: "On-demand credit", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Cast: []core.Person{{TMDB: 30, Name: "Cached Person", ThumbPath: "known"}}}
	if err = a.catalog.UpsertTitle(ctx, title, false); err != nil {
		t.Fatal(err)
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	artwork := &peopleHTTPArtwork{state: &peopleHTTPState{titles: map[core.RatingKey]core.Title{key: title}}}
	exp, err := a.newSurfaceExperience("silo", artwork)
	if err != nil {
		t.Fatal(err)
	}
	people, ok := exp.(frontend.PeopleReader)
	if !ok {
		t.Fatal("production people reader absent")
	}
	rows, err := people.People(ctx, viewer, core.Device{ID: "tv"}, frontend.PeopleQuery{Query: "Cached Person", Scope: frontend.PeopleMovies})
	if err != nil || len(rows) != 1 {
		t.Fatalf("on-demand membership %+v %v", rows, err)
	}
	cached, err := a.catalog.CachedGet(ctx, key)
	if err != nil || len(cached.Cast) != 1 {
		t.Fatalf("warm cache %+v %v", cached, err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, viewer.ID, nil, "silo", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	images, err := identity.NewArtworkImageGrantService(a.db, "silo", grants, func() frontend.Experience { return exp }, artwork, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	person, err := people.Person(ctx, viewer, core.Device{ID: "tv"}, rows[0].ID, frontend.PersonReadOptions{})
	if err != nil || len(person.Images) != 1 {
		t.Fatalf("portrait descriptor %+v %v", person, err)
	}
	secret, err := images.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: key, Image: person.Images[0]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`DELETE FROM catalog_credits WHERE key=? AND person_tmdb=30`, int64(key)); err != nil {
		t.Fatal(err)
	}
	// A different process can change shared credits without this node's writeGen.
	if stale, err := a.catalog.CachedGet(ctx, key); err != nil || len(stale.Cast) != 1 {
		t.Fatalf("test did not retain stale local cache %+v %v", stale, err)
	}
	if _, err = people.Person(ctx, viewer, core.Device{ID: "tv"}, person.ID, frontend.PersonReadOptions{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("removed DB membership resolved through stale cache: %v", err)
	}
	response := httptest.NewRecorder()
	images.Serve(response, httptest.NewRequest("GET", "/", nil), secret)
	if response.Code == 200 || strings.Contains(response.Body.String(), "person portrait") {
		t.Fatalf("stale descriptor bypassed current credit check: %d %s", response.Code, response.Body.String())
	}
}

func TestProductionPeopleUsesFreshSafetyAndRejectsPortraitReplay(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		t.Run(strconv.FormatBool(duringRead), func(t *testing.T) {
			a := setupTestApp(t, true)
			ctx := context.Background()
			if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
				t.Fatal(err)
			}
			viewer, err := a.users.Owner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(778)
			title := core.Title{Key: key, Type: core.TypeMovie, Title: "Restricted credit", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Thriller"}, Cast: []core.Person{{TMDB: 30, Name: "Cached Person", ThumbPath: "known"}}}
			if err := a.catalog.UpsertTitle(ctx, title, false); err != nil {
				t.Fatal(err)
			}
			availability := a.availability
			a.availability = nil
			t.Cleanup(func() { a.availability = availability })
			artwork := &peopleHTTPArtwork{state: &peopleHTTPState{titles: map[core.RatingKey]core.Title{key: title}}}
			exp, err := a.newSurfaceExperience("silo", artwork)
			if err != nil {
				t.Fatal(err)
			}
			people := exp.(frontend.PeopleReader)
			device := core.Device{ID: "tv"}
			rows, err := people.People(ctx, viewer, device, frontend.PeopleQuery{Scope: frontend.PeopleMovies})
			if err != nil || len(rows) != 1 {
				t.Fatalf("initial membership %+v %v", rows, err)
			}
			person, err := people.Person(ctx, viewer, device, rows[0].ID, frontend.PersonReadOptions{})
			if err != nil || len(person.Images) != 1 {
				t.Fatalf("initial portrait %+v %v", person, err)
			}
			grants, err := identity.NewGrants(a.db, a.users, time.Hour, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := grants.Issue(ctx, viewer.ID, nil, "silo", device)
			if err != nil {
				t.Fatal(err)
			}
			images, err := identity.NewArtworkImageGrantService(a.db, "silo", grants, func() frontend.Experience { return exp }, artwork, time.Minute, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			secret, err := images.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: device, Item: key, Image: person.Images[0]})
			if err != nil {
				t.Fatal(err)
			}
			writer, err := safety.New(ctx, a.db)
			if err != nil {
				t.Fatal(err)
			}
			writePolicy := func() {
				t.Helper()
				if err := writer.Put(ctx, viewer.ID, safety.Policy{BlockedGenres: []string{"Thriller"}}); err != nil {
					t.Fatal(err)
				}
				if !a.safety.Allowed(viewer, &title) {
					t.Fatal("reader cache unexpectedly refreshed; test needs two Services")
				}
			}
			if duringRead {
				artwork.beforeRead = writePolicy
			} else {
				writePolicy()
			}
			response := httptest.NewRecorder()
			images.Serve(response, httptest.NewRequest("GET", "/", nil), secret)
			if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "person portrait") {
				t.Fatalf("stale policy published portrait %d %s", response.Code, response.Body.String())
			}
			if _, err := people.Person(ctx, viewer, device, person.ID, frontend.PersonReadOptions{Prefetch: true}); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("restricted person resolved: %v", err)
			}
			rows, err = people.People(ctx, viewer, device, frontend.PeopleQuery{})
			if err != nil || len(rows) != 0 {
				t.Fatalf("restricted person listed %+v %v", rows, err)
			}
		})
	}
}

func TestSiloPeoplePrefetchDoesNotReachArtworkQueues(t *testing.T) {
	exp, _, artwork := peopleHTTPFixture(t)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	ids, err := exp.PersonFacetIDs(ctx, viewer, core.Device{}, core.EpisodeKey(1, 1, 1), []int64{30})
	if err != nil {
		t.Fatal(err)
	}
	artwork.beforeList = func() { t.Fatal("prefetch reached artwork work/refresh queues") }
	person, err := exp.Person(ctx, viewer, core.Device{}, ids[30], frontend.PersonReadOptions{Prefetch: true})
	if err != nil || person.ID != ids[30] || person.Name != "Episode Person" || len(person.Images) != 0 {
		t.Fatalf("cached prefetch %+v %v", person, err)
	}
}

type hiddenPeopleExperience struct{ frontend.Experience }

func TestSiloPortraitDeniesMissingReaderAndGrantChangesDuringRead(t *testing.T) {
	for _, change := range []string{"missing_reader", "image_revoke", "source_revoke", "header_delete", "image_expiry", "source_expiry", "cancel"} {
		t.Run(change, func(t *testing.T) {
			exp, state, artwork := peopleHTTPFixture(t)
			ctx := context.Background()
			db := store.OpenTest(t)
			if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
				t.Fatal(err)
			}
			device := core.Device{ID: "tv"}
			grants, err := identity.NewGrants(db, state, time.Hour, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := grants.Issue(ctx, 1, nil, "silo", device)
			if err != nil {
				t.Fatal(err)
			}
			key := core.EpisodeKey(1, 1, 1)
			ids, err := exp.PersonFacetIDs(ctx, &state.viewer, device, key, []int64{30})
			if err != nil {
				t.Fatal(err)
			}
			person, err := exp.Person(ctx, &state.viewer, device, ids[30], frontend.PersonReadOptions{})
			if err != nil || len(person.Images) != 1 {
				t.Fatalf("initial portrait %+v %v", person, err)
			}
			var current frontend.Experience = exp
			images, err := identity.NewArtworkImageGrantService(db, "silo", grants, func() frontend.Experience { return current }, artwork, time.Minute, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			req := frontend.ImageGrantRequest{Credential: credential, Device: device, Item: key, Image: person.Images[0]}
			secret, err := images.Issue(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if change == "missing_reader" {
				current = hiddenPeopleExperience{exp}
				if _, err := images.Issue(ctx, req); !errors.Is(err, core.ErrNotAvailable) {
					t.Fatalf("missing image reader issued: %v", err)
				}
				current = exp
			}
			readCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			artwork.beforeRead = func() {
				var err error
				switch change {
				case "missing_reader":
					current = hiddenPeopleExperience{exp}
				case "image_revoke":
					err = images.Revoke(ctx, credential, secret)
				case "source_revoke":
					err = grants.RevokeDevice(ctx, 1, device.ID)
				case "header_delete":
					_, err = db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace='image'`)
				case "image_expiry":
					_, err = db.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=1`)
				case "source_expiry":
					_, err = db.ExecContext(ctx, `UPDATE identity_grants SET expires=1 WHERE viewer_id=1`)
				case "cancel":
					cancel()
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			images.Serve(response, httptest.NewRequest("GET", "/", nil).WithContext(readCtx), secret)
			if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "person portrait") {
				t.Fatalf("losing %s returned bytes: %d %s", change, response.Code, response.Body.String())
			}
		})
	}
}
