package experience

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type peopleCatalogue struct {
	mu          sync.RWMutex
	viewer      core.User
	titles      map[core.RatingKey]core.Title
	beforeGet   func(core.RatingKey)
	creditCalls atomic.Int32
}

func (c *peopleCatalogue) get(_ context.Context, key core.RatingKey) (*core.Title, error) {
	if c.beforeGet != nil {
		c.beforeGet(key)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	title, ok := c.titles[key]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &title, nil
}
func (c *peopleCatalogue) update(key core.RatingKey, change func(*core.Title)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	title := c.titles[key]
	change(&title)
	c.titles[key] = title
}
func peopleFixture() (*Service, *peopleCatalogue) {
	movie, show, episode := core.MovieKey(1), core.ShowKey(1), core.EpisodeKey(1, 1, 1)
	c := &peopleCatalogue{viewer: core.User{ID: 1}, titles: map[core.RatingKey]core.Title{
		movie:            {Key: movie, Type: core.TypeMovie, Cast: []core.Person{{TMDB: 10, Name: "The Scene", IMDB: "nm10", ThumbPath: "/portrait"}, {TMDB: 10, Name: "The Scene"}, {TMDB: 11, Name: "The Scenery"}}},
		show:             {Key: show, Type: core.TypeShow, Directors: []core.Person{{TMDB: 20, Name: "Series Person"}}},
		episode:          {Key: episode, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Cast: []core.Person{{TMDB: 30, Name: "Episode Only", IMDB: "nm30"}, {TMDB: 10, Name: "The Scene"}}},
		core.MovieKey(2): {Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true, Cast: []core.Person{{TMDB: 99, Name: "A Scene Hidden"}}},
	}}
	s := &Service{d: ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			c.mu.RLock()
			defer c.mu.RUnlock()
			if id != c.viewer.ID {
				return nil, core.ErrUnauthorized
			}
			v := c.viewer
			return &v, nil
		},
		CachedGet: c.get,
		CachedGetMany: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			out := map[core.RatingKey]core.Title{}
			for _, key := range keys {
				title, err := c.get(ctx, key)
				if err == nil {
					out[key] = *title
				} else if !errors.Is(err, core.ErrNotFound) {
					return nil, err
				}
			}
			return out, nil
		},
		CreditItems: func(_ context.Context, after core.RatingKey, limit int) ([]core.RatingKey, error) {
			c.creditCalls.Add(1)
			c.mu.RLock()
			defer c.mu.RUnlock()
			keys := []core.RatingKey{}
			for key, title := range c.titles {
				if len(title.Cast)+len(title.Directors)+len(title.Writers)+len(title.Crew) > 0 {
					keys = append(keys, key)
				}
			}
			sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
			start := sort.Search(len(keys), func(i int) bool { return keys[i] > after })
			end := min(start+limit, len(keys))
			return keys[start:end], nil
		},
		Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}},
		Get: func(ctx context.Context, _ *core.User, key core.RatingKey) (*core.Title, error) {
			return c.get(ctx, key)
		},
		Visibility: Visibility{SectionFor: func(kind core.MediaType) (int, bool) {
			if kind == core.TypeMovie {
				return 1, true
			}
			return 2, true
		}, GetTitle: c.get},
		Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
			panic("people used unfiltered browse fallback")
		},
		Grandchildren: func(_ context.Context, _ *core.User, key core.RatingKey) ([]core.Title, error) {
			c.mu.RLock()
			defer c.mu.RUnlock()
			items := []core.Title{}
			for _, title := range c.titles {
				if title.Type == core.TypeEpisode && title.ShowKey == key {
					items = append(items, title)
				}
			}
			return items, nil
		},
		Enrich: func(context.Context, *core.User, core.Device, *core.Title) (frontend.ItemDetail, error) {
			panic("people navigation reached playback/source enrichment")
		},
	}}
	return s, c
}
func TestPeopleCurrentMembershipScopesAndIdentity(t *testing.T) {
	s, c := peopleFixture()
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	device := core.Device{ID: "tv"}
	all, err := s.People(ctx, viewer, device, frontend.PeopleQuery{})
	if err != nil || len(all) != 4 {
		t.Fatalf("all people %+v %v", all, err)
	}
	for _, scope := range []frontend.PeopleScope{frontend.PeopleMovies, frontend.PeopleSeries, frontend.PeopleEpisodes, frontend.PeopleVideo, frontend.PeopleVideoWithEpisodes} {
		rows, err := s.People(ctx, viewer, device, frontend.PeopleQuery{Scope: scope})
		if err != nil {
			t.Fatal(scope, err)
		}
		episodeFound := false
		for _, row := range rows {
			if row.ID == facetID("person", "99") {
				t.Fatal("hidden credit leaked")
			}
			if row.TMDB == 30 {
				episodeFound = true
			}
		}
		if episodeFound != (scope == frontend.PeopleEpisodes || scope == frontend.PeopleVideoWithEpisodes) {
			t.Fatalf("scope%s episode membership %+v", scope, rows)
		}
	}
	ids, err := s.PersonFacetIDs(ctx, viewer, device, core.EpisodeKey(1, 1, 1), []int64{30})
	if err != nil {
		t.Fatal(err)
	}
	person, err := s.Person(ctx, viewer, device, ids[30], frontend.PersonReadOptions{Prefetch: true})
	if err != nil || person.ID != ids[30] || person.Name != "Episode Only" || person.IMDB != "nm30" {
		t.Fatalf("episode detail %+v %v", person, err)
	}
	for _, id := range []string{"30", facetID("person", "99"), "arbitrary-global-id"} {
		if _, err = s.Person(ctx, viewer, device, id, frontend.PersonReadOptions{}); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("unissued/inaccessible id %s: %v", id, err)
		}
	}
	c.update(core.EpisodeKey(1, 1, 1), func(title *core.Title) { title.Cast = nil })
	if _, err = s.Person(ctx, viewer, device, ids[30], frontend.PersonReadOptions{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("removed membership resolved: %v", err)
	}
	if _, err = s.People(ctx, &core.User{ID: 2}, device, frontend.PeopleQuery{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("inactive viewer %v", err)
	}
}
func TestPeopleNameRankingBoundsAndVisibilityBeforeLimit(t *testing.T) {
	s, c := peopleFixture()
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	rows, err := s.People(ctx, viewer, core.Device{}, frontend.PeopleQuery{Query: "the scene", Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].Name != "The Scene" {
		t.Fatalf("exact-first %+v %v", rows, err)
	}
	rows, err = s.People(ctx, viewer, core.Device{}, frontend.PeopleQuery{Query: "scene", Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].TMDB != 10 {
		t.Fatalf("hidden match consumed limit %+v %v", rows, err)
	}
	rows, err = s.People(ctx, viewer, core.Device{}, frontend.PeopleQuery{Query: "cene"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("matched substring %+v %v", rows, err)
	}
	c.update(core.MovieKey(1), func(title *core.Title) {
		title.Cast = []core.Person{{TMDB: 40, Name: "One Two Three Four Five Six Seven Eight"}, {TMDB: 41, Name: "Café René"}}
	})
	for query, want := range map[string]int64{"one one two three four five six seven eight ninth": 40, "CAFÉ RE": 41} {
		rows, err = s.People(ctx, viewer, core.Device{}, frontend.PeopleQuery{Query: query, Scope: frontend.PeopleMovies})
		if err != nil || len(rows) != 1 || rows[0].TMDB != want {
			t.Fatalf("word rules %q %+v %v", query, rows, err)
		}
	}
	for _, q := range []frontend.PeopleQuery{{Limit: -1}, {Limit: 101}, {Query: strings.Repeat("é", 201)}, {Scope: "manga"}} {
		before := c.creditCalls.Load()
		if _, err = s.People(ctx, viewer, core.Device{}, q); !errors.Is(err, frontend.ErrPeopleQuery) {
			t.Fatalf("invalid query %+v: %v", q, err)
		}
		if c.creditCalls.Load() != before {
			t.Fatal("invalid query read catalogue")
		}
	}
}
func TestPeopleDetailRechecksInheritedPolicyDuringRead(t *testing.T) {
	s, c := peopleFixture()
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	key := core.EpisodeKey(1, 1, 1)
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.beforeGet = func(k core.RatingKey) {
		if k == key {
			once.Do(func() { close(reached); <-resume })
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Person(ctx, viewer, core.Device{}, facetID("person", "30"), frontend.PersonReadOptions{Prefetch: true})
		result <- err
	}()
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	select {
	case <-reached:
	case err := <-result:
		t.Fatalf("read did not reach controlled boundary: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("person read boundary timeout")
	}
	c.update(core.ShowKey(1), func(title *core.Title) { title.Adult = true })
	close(resume)
	resumed = true
	select {
	case err := <-result:
		if !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("changed parent policy leaked detail: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("person read did not finish")
	}

}

func TestPeopleCreditPagesDoNotTruncateBeforePolicyAndMatch(t *testing.T) {
	s, c := peopleFixture()
	c.mu.Lock()
	for n := 100; n < 250; n++ {
		key := core.MovieKey(int64(n))
		c.titles[key] = core.Title{Key: key, Type: core.TypeMovie, Adult: true, Cast: []core.Person{{TMDB: int64(n + 1000), Name: "Hidden Match"}}}
	}
	key := core.MovieKey(999)
	c.titles[key] = core.Title{Key: key, Type: core.TypeMovie, Cast: []core.Person{{TMDB: 999, Name: "Late Match"}}}
	c.mu.Unlock()
	rows, err := s.People(context.Background(), &core.User{ID: 1}, core.Device{}, frontend.PeopleQuery{Query: "Match", Limit: 1, Scope: frontend.PeopleMovies})
	if err != nil || len(rows) != 1 || rows[0].TMDB != 999 || c.creditCalls.Load() < 2 {
		t.Fatalf("credit pages truncated before visibility/match %+v calls=%d %v", rows, c.creditCalls.Load(), err)
	}
}
