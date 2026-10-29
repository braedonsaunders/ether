package episodeorder

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeSource struct{ group tmdb.EpisodeGroup }

func (f fakeSource) EpisodeGroups(context.Context, int64) ([]tmdb.EpisodeGroupSummary, error) {
	return []tmdb.EpisodeGroupSummary{f.group.EpisodeGroupSummary}, nil
}
func (f fakeSource) EpisodeGroup(context.Context, string) (*tmdb.EpisodeGroup, error) {
	g := f.group
	return &g, nil
}

type fakeCatalog struct {
	core.Catalog
	eps []core.Title
}

func (f fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	if k.Kind() == core.KindShow {
		return &core.Title{Key: k, Type: core.TypeShow, Title: "Fixture"}, nil
	}
	for _, ep := range f.eps {
		if ep.Key == k {
			return &ep, nil
		}
	}
	return nil, core.ErrNotFound
}
func (f fakeCatalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return f.eps, nil
}
func (f fakeCatalog) Children(_ context.Context, k core.RatingKey) ([]core.Title, error) {
	if k.Kind() == core.KindShow {
		return []core.Title{{Key: core.SeasonKey(10, 0), Season: 0}, {Key: core.SeasonKey(10, 1), Season: 1}}, nil
	}
	var out []core.Title
	for _, ep := range f.eps {
		if ep.Season == k.Season() {
			out = append(out, ep)
		}
	}
	return out, nil
}
func fixture(t *testing.T) *Service {
	t.Helper()
	db, e := store.OpenMemory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	ep := func(s, n int) core.Title {
		return core.Title{Key: core.EpisodeKey(10, s, n), Type: core.TypeEpisode, Season: s, Episode: n, Title: "Episode"}
	}
	cat := fakeCatalog{eps: []core.Title{ep(0, 1), ep(1, 1), ep(1, 2), ep(1, 3)}}
	src := fakeSource{tmdb.EpisodeGroup{EpisodeGroupSummary: tmdb.EpisodeGroupSummary{ID: "dvd", Name: "DVD", Type: 3}, Groups: []tmdb.EpisodeSubgroup{{ID: "one", Name: "Disc 1", Order: 0, Episodes: []tmdb.OrderedEpisode{{Episode: tmdb.Episode{SeasonNumber: 1, EpisodeNumber: 2}, Order: 0}, {Episode: tmdb.Episode{SeasonNumber: 0, EpisodeNumber: 1}, Order: 1}, {Episode: tmdb.Episode{SeasonNumber: 1, EpisodeNumber: 1}, Order: 2}}}}}}
	return New(db, src, cat)
}

func TestCachedVirtualSeasonIsViewerScopedAndNeverRefreshes(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	viewer := &core.User{ID: 42}
	key := core.SeasonKey(10, 9000)
	if _, err := s.CachedVirtualSeason(ctx, viewer, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cold cache: %v", err)
	}
	if err := s.Save(ctx, viewer.ID, core.ShowKey(10), Preference{GroupID: "dvd", Specials: "last"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Children(ctx, viewer, core.ShowKey(10)); err != nil {
		t.Fatal(err)
	}
	title, err := s.CachedVirtualSeason(ctx, viewer, key)
	if err != nil || title.Key != key || title.Title != "Disc 1" {
		t.Fatalf("cached season: %+v %v", title, err)
	}
	title.Title = "changed"
	if again, _ := s.CachedVirtualSeason(ctx, viewer, key); again.Title != "Disc 1" {
		t.Fatal("caller changed cached title")
	}
	if _, err = s.CachedVirtualSeason(ctx, &core.User{ID: 43}, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other viewer: %v", err)
	}
	s.mu.Lock()
	for key, cached := range s.cache {
		cached.expires = time.Now().Add(-time.Second)
		s.cache[key] = cached
	}
	s.mu.Unlock()
	if _, err = s.CachedVirtualSeason(ctx, viewer, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expired cache: %v", err)
	}
}
func TestOrderingPreservesCanonicalIdentityAndSpecials(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	show := core.ShowKey(10)
	if e := s.Save(ctx, 0, show, Preference{GroupID: "dvd", Specials: "last"}); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 42}
	p, e := s.Preference(ctx, u.ID, show)
	if e != nil || !p.Inherited {
		t.Fatalf("inherit: %+v %v", p, e)
	}
	seasons, e := s.Children(ctx, u, show)
	if e != nil || len(seasons) != 2 || seasons[0].Season != 9000 || seasons[1].Season != 9999 {
		t.Fatalf("seasons: %+v %v", seasons, e)
	}
	kids, e := s.Children(ctx, u, seasons[0].Key)
	if e != nil || len(kids) != 3 {
		t.Fatalf("kids %+v %v", kids, e)
	}
	if kids[0].Key != core.EpisodeKey(10, 1, 2) || kids[0].Episode != 2 || kids[1].Season != 0 {
		t.Fatal("canonical identity changed")
	}
	if season, index, name, ok := s.Position(ctx, u, kids[1].Key); !ok || season != 9000 || index != 2 || name != "Disc 1" {
		t.Fatal("display position", season, index, name, ok)
	}
	single, e := s.Get(ctx, u, seasons[0].Key)
	if e != nil || single.Title != "Disc 1" {
		t.Fatal(single, e)
	}
	if e = s.Save(ctx, u.ID, show, Preference{GroupID: "dvd", Specials: "hidden"}); e != nil {
		t.Fatal(e)
	}
	all, e := s.Grandchildren(ctx, u, show)
	if e != nil || len(all) != 3 {
		t.Fatal(all, e)
	}
	for _, t := range all {
		if t.Season == 0 {
			panic("hidden special")
		}
	}
	if e = s.Save(ctx, u.ID, show, Preference{Specials: "first"}); e != nil {
		t.Fatal(e)
	}
	all, e = s.Grandchildren(ctx, u, show)
	if e != nil || all[0].Season != 0 {
		t.Fatal("explicit aired order", all, e)
	}
	p, _ = s.Preference(ctx, u.ID, show)
	if p.Inherited || p.GroupID != "" {
		t.Fatal(p)
	}
	if e = s.Reset(ctx, u.ID, show); e != nil {
		t.Fatal(e)
	}
	p, _ = s.Preference(ctx, u.ID, show)
	if !p.Inherited || p.GroupID != "dvd" {
		t.Fatal(p)
	}
}
func TestOwnerAPIRejectsForeignGroupAndPersists(t *testing.T) {
	s := fixture(t)
	h := s.Router()
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"groupId":"foreign","specials":"last"}`, 400}, {`{"groupId":"dvd","specials":"last"}`, 200}, {`{"groupId":"dvd","specials":"unknown"}`, 400}} {
		r := httptest.NewRequest("PUT", "/shows/"+core.ShowKey(10).String()+"/users/0", strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/shows/"+core.ShowKey(10).String()+"/users/8/preview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"canonicalEpisode":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestGroupedResolvesViewerOverOwnerDefault(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	ordered, plain, own := core.ShowKey(10), core.ShowKey(11), core.ShowKey(12)
	for _, k := range []core.RatingKey{ordered, own} {
		if e := s.Save(ctx, 0, k, Preference{GroupID: "dvd", Specials: "last"}); e != nil {
			t.Fatal(e)
		}
	}
	// The viewer chose the canonical order for one show the owner grouped.
	if e := s.Save(ctx, 42, own, Preference{Specials: "first"}); e != nil {
		t.Fatal(e)
	}
	shows := []core.RatingKey{ordered, plain, own, core.EpisodeKey(10, 1, 1)}
	got, e := s.Grouped(ctx, &core.User{ID: 42}, shows)
	if e != nil || len(got) != 1 || !got[ordered] {
		t.Fatalf("viewer: %v %v", got, e)
	}
	got, e = s.Grouped(ctx, &core.User{ID: 43}, shows)
	if e != nil || len(got) != 2 || !got[ordered] || !got[own] {
		t.Fatalf("inheriting viewer: %v %v", got, e)
	}
	if got, e = s.Grouped(ctx, nil, nil); e != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, e)
	}
}
