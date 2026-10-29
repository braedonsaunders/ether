package collections_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestInvalidUpdatePreservesPublishedDefinition(t *testing.T) {
	e := newEnv(t)
	sp := spec("Valid original", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{core.MovieKey(101).String()}}})
	created, err := e.svc.CreateSpec(e.ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	key := core.SmartKey(mustInt(t, created.ID))
	for _, mutate := range []func(*core.CollectionSpec){
		func(s *core.CollectionSpec) { s.Sort = "unknown" },
		func(s *core.CollectionSpec) { s.Match = "nested" },
		func(s *core.CollectionSpec) { s.Limit = -1 },
		func(s *core.CollectionSpec) { s.Rules = []core.CollectionRule{rl("rating", "between", []int{1})} },
		func(s *core.CollectionSpec) {
			s.Sources = []core.CollectionSource{{Kind: "watchProvider", ID: 8, Region: "USA"}}
		},
	} {
		invalid := sp
		mutate(&invalid)
		if _, err := e.svc.UpdateSpec(e.ctx, created.ID, invalid); err == nil {
			t.Fatalf("accepted invalid update: %+v", invalid)
		}
		saved, err := e.svc.GetSpec(e.ctx, created.ID)
		if err != nil || saved.Name != sp.Name || saved.Sort != sp.Sort {
			t.Fatalf("stored spec changed: %+v %v", saved, err)
		}
		page, err := e.svc.Items(e.ctx, owner, key, 0, 10)
		if err != nil || len(page.Items) != 1 || page.Items[0].Key != core.MovieKey(101) {
			t.Fatalf("published membership changed: %+v %v", page, err)
		}
	}
}

func TestCancelledPreviewReturnsCancellation(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	_, err := e.svc.Preview(ctx, spec("Cancelled", "movie", catalogSource()), 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preview error = %v", err)
	}
}

func TestOfflineSortDirectionsAndTies(t *testing.T) {
	e := newEnv(t)
	// The "added" cases below are tie-breaks: all three rows must share one
	// added_at, whatever second the fixture happened to be seeded in.
	var distinct int
	if err := e.db.(*sql.DB).QueryRowContext(e.ctx,
		`SELECT COUNT(DISTINCT added_at) FROM catalog_titles WHERE key IN (?,?,?)`,
		int64(core.MovieKey(101)), int64(core.MovieKey(102)), int64(core.MovieKey(103)),
	).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Fatalf("fixture rows have %d distinct added_at values, want 1", distinct)
	}
	sp := spec("Sort audit", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{core.MovieKey(101).String(), core.MovieKey(102).String(), core.MovieKey(103).String()}}})
	tests := []struct {
		mode string
		desc bool
		want []core.RatingKey
	}{
		{"source", false, []core.RatingKey{core.MovieKey(101), core.MovieKey(102), core.MovieKey(103)}},
		{"source", true, []core.RatingKey{core.MovieKey(103), core.MovieKey(102), core.MovieKey(101)}},
		{"title", false, []core.RatingKey{core.MovieKey(102), core.MovieKey(101), core.MovieKey(103)}},
		{"title", true, []core.RatingKey{core.MovieKey(103), core.MovieKey(101), core.MovieKey(102)}},
		{"rating", false, []core.RatingKey{core.MovieKey(101), core.MovieKey(103), core.MovieKey(102)}},
		{"rating", true, []core.RatingKey{core.MovieKey(102), core.MovieKey(103), core.MovieKey(101)}},
		{"popularity", false, []core.RatingKey{core.MovieKey(103), core.MovieKey(102), core.MovieKey(101)}},
		{"popularity", true, []core.RatingKey{core.MovieKey(101), core.MovieKey(102), core.MovieKey(103)}},
		{"released", false, []core.RatingKey{core.MovieKey(102), core.MovieKey(101), core.MovieKey(103)}},
		{"released", true, []core.RatingKey{core.MovieKey(103), core.MovieKey(101), core.MovieKey(102)}},
		// Fixture rows have identical added timestamps; ties use popularity.
		{"added", false, []core.RatingKey{core.MovieKey(101), core.MovieKey(102), core.MovieKey(103)}},
		{"added", true, []core.RatingKey{core.MovieKey(101), core.MovieKey(102), core.MovieKey(103)}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/%t", tc.mode, tc.desc), func(t *testing.T) {
			sp.Sort, sp.SortDesc = tc.mode, tc.desc
			p, err := e.svc.Preview(e.ctx, sp, 60)
			if err != nil {
				t.Fatal(err)
			}
			var got []core.RatingKey
			for _, item := range p.Items {
				got = append(got, item.Key)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestFailedSourcesWarnAndPreserveOtherSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline fixture", http.StatusBadGateway) }))
	defer server.Close()
	bases := collections.ExternalBases{TraktAPI: server.URL, Letterboxd: server.URL, MDBList: server.URL, AniListAPI: server.URL, AnimeIDs: server.URL, StevenLu: server.URL}
	e := newEnv(t, func(o *envOptions) { o.noTMDB = true }, withOpts(collections.WithExternalBases(bases), collections.WithTraktConfig(func() config.Trakt { return config.Trakt{ClientID: "fixture"} })))
	cases := []core.CollectionSource{
		{Kind: "tmdbCollection", ID: 999}, {Kind: "tmdbCollection", Label: "Missing"}, {Kind: "tmdbKeyword", ID: 1}, {Kind: "tmdbCompany", ID: 1}, {Kind: "tmdbNetwork", ID: 1}, {Kind: "tmdbPerson", ID: 1}, {Kind: "watchProvider", ID: 1}, {Kind: "tmdbDiscover", Params: map[string]string{"with_genres": "28"}},
		{Kind: "traktList", URL: "https://trakt.tv/users/alice/lists/greats"}, {Kind: "letterboxdList", URL: "https://letterboxd.com/someone/list/favourites/"}, {Kind: "mdblist", URL: "https://mdblist.com/lists/bob/top-picks"}, {Kind: "anilistList", URL: "https://anilist.co/user/kai/animelist"}, {Kind: "stevenlu"},
	}
	for _, src := range cases {
		t.Run(src.Kind, func(t *testing.T) {
			sp := spec("Partial source", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{core.MovieKey(101).String()}}, src})
			p, err := e.svc.Preview(e.ctx, sp, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) != 1 || p.Items[0].Key != core.MovieKey(101) || len(p.Warnings) == 0 {
				t.Fatalf("expected partial result and warning: %+v", p)
			}
		})
	}
}

func TestScheduledRefreshUpdatesPublishedMembership(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	e := newEnv(t, withOpts(collections.WithNow(func() time.Time { return now })))
	sp := spec("Refresh audit", "movie", catalogSource(), rl("rating", "gte", 9.5))
	sp.Refresh = "hourly"
	created, err := e.svc.CreateSpec(e.ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	key := core.SmartKey(mustInt(t, created.ID))
	first, err := e.svc.Items(e.ctx, owner, key, 0, 20)
	if err != nil || first.Total != 0 {
		t.Fatalf("initial page: %+v %v", first, err)
	}
	title := core.Title{Key: core.MovieKey(901), Type: core.TypeMovie, IDs: core.IDs{TMDB: 901}, Title: "New mirror entry", Rating: 9.8, Released: now.AddDate(-1, 0, 0)}
	if err := e.cat.UpsertTitle(e.ctx, title, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Minute)
	e.svc.RunDue(e.ctx)
	early, err := e.svc.Items(e.ctx, owner, key, 0, 20)
	if err != nil || early.Total != 0 {
		t.Fatalf("refreshed before due: %+v %v", early, err)
	}
	now = now.Add(2 * time.Minute)
	e.svc.RunDue(e.ctx)
	after, err := e.svc.Items(e.ctx, owner, key, 0, 20)
	if err != nil || after.Total != 1 || after.Items[0].Key != title.Key {
		t.Fatalf("scheduled refresh: %+v %v", after, err)
	}
}

func TestViewerWatchRulesAndAudienceStayIndependent(t *testing.T) {
	a, b := core.MovieKey(101), core.MovieKey(102)
	st := &fakeState{watched: map[int64]map[core.RatingKey]bool{1: {a: true}, 3: {b: true}}}
	e := newEnv(t, withOpts(collections.WithState(st)))
	sp := spec("Viewer audit", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{a.String(), b.String()}}}, rl("watched", "is", true))
	sp.Targets.Users = core.UserTargets{IDs: []int64{1, 3}}
	created, err := e.svc.CreateSpec(e.ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	key := core.SmartKey(mustInt(t, created.ID))
	for _, tc := range []struct {
		user *core.User
		want core.RatingKey
	}{{owner, a}, {guest, b}} {
		page, err := e.svc.Items(e.ctx, tc.user, key, 0, 10)
		if err != nil || len(page.Items) != 1 || page.Items[0].Key != tc.want {
			t.Fatalf("viewer %d: %+v %v", tc.user.ID, page, err)
		}
	}
	if _, err := e.svc.Items(e.ctx, kid, key, 0, 10); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("untargeted viewer: %v", err)
	}
	st.mu.Lock()
	st.watched[1] = map[core.RatingKey]bool{b: true}
	st.mu.Unlock()
	page, err := e.svc.Items(e.ctx, owner, key, 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != b {
		t.Fatalf("watch change: %+v %v", page, err)
	}
}

func TestLaterListPageFailureWarnsAndKeepsEarlierItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" || strings.Contains(r.URL.Path, "/page/2/") {
			http.Error(w, "page failed", http.StatusBadGateway)
			return
		}
		if strings.Contains(r.URL.Path, "/users/") {
			w.Header().Set("X-Pagination-Page-Count", "2")
			fmt.Fprint(w, `[{"type":"movie","movie":{"title":"Space Saga","ids":{"tmdb":101}}}]`)
			return
		}
		fmt.Fprint(w, `<div class="poster" data-film-slug="space-saga" data-target-link="/film/space-saga/"><img alt="Space Saga" /></div><div class="pagination"><a class="next" href="/someone/list/favourites/page/2/">Next</a></div>`)
	}))
	defer server.Close()
	e := newEnv(t, withOpts(collections.WithExternalBases(collections.ExternalBases{TraktAPI: server.URL, Letterboxd: server.URL}), collections.WithTraktConfig(func() config.Trakt { return config.Trakt{ClientID: "fixture"} })))
	cases := []core.CollectionSource{{Kind: "traktList", URL: "https://trakt.tv/users/alice/lists/greats"}, {Kind: "letterboxdList", URL: "https://letterboxd.com/someone/list/favourites/"}}
	for _, src := range cases {
		t.Run(src.Kind, func(t *testing.T) {
			p, err := e.svc.Preview(e.ctx, spec("Partial pages", "movie", []core.CollectionSource{src}), 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) != 1 || p.Items[0].Key != core.MovieKey(101) || len(p.Warnings) == 0 {
				t.Fatalf("missing partial item or warning: total=%d warnings=%v", p.Total, p.Warnings)
			}
			created, err := e.svc.CreateSpec(e.ctx, spec("Saved partial "+src.Kind, "movie", []core.CollectionSource{src}))
			if err != nil {
				t.Fatal(err)
			}
			if len(created.Warnings) == 0 {
				t.Fatalf("saved build lost partial warning: %+v", created)
			}
			page, err := e.svc.Items(e.ctx, owner, core.SmartKey(mustInt(t, created.ID)), 0, 20)
			if err != nil || len(page.Items) != 1 || page.Items[0].Key != core.MovieKey(101) {
				t.Fatalf("saved partial items: %+v %v", page, err)
			}
		})
	}
}

func TestLaterDiscoverPageFailureWarnsAndKeepsEarlierItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/discover/movie" {
			if r.URL.Query().Get("page") == "2" {
				http.Error(w, "page failed", http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `{"page":1,"total_pages":2,"total_results":40,"results":[{"id":101,"title":"Space Saga","release_date":"1977-05-25"}]}`)
			return
		}
		fmt.Fprint(w, `{"genres":[]}`)
	}))
	defer server.Close()
	client := tmdb.New("fixture", slog.Default(), tmdb.WithBaseURL(server.URL), tmdb.WithRateLimit(100000, 100000), tmdb.WithMaxRetries(0))
	defer client.Close()
	e := newEnv(t, withOpts(collections.WithTMDB(client)))
	p, err := e.svc.Preview(e.ctx, spec("Partial discover", "movie", []core.CollectionSource{{Kind: "tmdbDiscover", Params: map[string]string{"with_genres": "28"}}}), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].Key != core.MovieKey(101) || len(p.Warnings) == 0 {
		t.Fatalf("missing partial discover item or warning: total=%d warnings=%v", p.Total, p.Warnings)
	}
}

func TestRandomAndPersonalSortDirectionsAreStable(t *testing.T) {
	scorer := &fakeScorer{}
	e := newEnv(t, withOpts(collections.WithScorer(scorer)))
	sp := spec("Direction audit", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{core.MovieKey(101).String(), core.MovieKey(102).String(), core.MovieKey(103).String()}}})
	sp.Sort = "random"
	var randomKeys []core.RatingKey
	for _, desc := range []bool{false, true} {
		sp.SortDesc = desc
		p, err := e.svc.Preview(e.ctx, sp, 20)
		if err != nil {
			t.Fatal(err)
		}
		var keys []core.RatingKey
		for _, item := range p.Items {
			keys = append(keys, item.Key)
		}
		if randomKeys == nil {
			randomKeys = keys
		} else if !slices.Equal(randomKeys, keys) {
			t.Fatalf("random direction changed order: %v %v", randomKeys, keys)
		}
	}
	sp.Sort = "personal"
	for _, desc := range []bool{false, true} {
		sp.SortDesc = desc
		created, err := e.svc.CreateSpec(e.ctx, sp)
		if err != nil {
			t.Fatal(err)
		}
		page, err := e.svc.Items(e.ctx, owner, core.SmartKey(mustInt(t, created.ID)), 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 3 || page.Items[0].Key != core.MovieKey(101) || page.Items[2].Key != core.MovieKey(103) {
			t.Fatalf("personal order desc=%t: %+v", desc, page.Items)
		}
	}
}

func TestSortUnknownRatingAndUnreleasedTitles(t *testing.T) {
	e := newEnv(t)
	known := time.Now().AddDate(-1, 0, 0)
	titles := []core.Title{
		{Key: core.MovieKey(901), Type: core.TypeMovie, Title: "Unrated A", Released: known, Popularity: 5},
		{Key: core.MovieKey(902), Type: core.TypeMovie, Title: "Unrated B", Released: known, Popularity: 10},
		{Key: core.MovieKey(903), Type: core.TypeMovie, Title: "Unknown release", Rating: 9},
	}
	for _, item := range titles {
		if err := e.cat.UpsertTitle(e.ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	sp := spec("Unknown values", "movie", []core.CollectionSource{{Kind: "titles", Keys: []string{titles[0].Key.String(), titles[1].Key.String(), titles[2].Key.String()}}})
	sp.Sort = "rating"
	for _, desc := range []bool{false, true} {
		sp.SortDesc = desc
		p, err := e.svc.Preview(e.ctx, sp, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 2 || p.Items[0].Key != titles[1].Key || p.Items[1].Key != titles[0].Key {
			t.Fatalf("desc=%t unknown rating/release: %+v", desc, p.Items)
		}
	}
}
