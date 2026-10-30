package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (s *Server) controlLegacySectionRecentlyViewed(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	mc.Title2 = "Recently Viewed"
	if s.d.State != nil && pr.User != nil {
		hist, err := s.d.State.History(ctx, pr.User.ID, time.Time{}, 100)
		if err == nil {
			seen := map[core.RatingKey]bool{}
			var keys []core.RatingKey
			for _, h := range hist {
				if !seen[h.Key] {
					seen[h.Key] = true
					keys = append(keys, h.Key)
				}
			}
			var items []core.Title
			for _, t := range s.getTitles(ctx, keys) {
				if sct, ok := s.sectionFor(t.Type); ok && sct.ID == sec.ID {
					items = append(items, t)
				}
			}
			mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) controlLegacyRecentlyAddedHub(ctx context.Context, pr *Request, sec Section, count int) (hubSpec, error) {
	q := core.BrowseQuery{Filter: core.Filter{Type: sec.Type}, Sort: core.SortAdded, Desc: true, Limit: count + 1}
	if pr.User != nil {
		q.Filter.MaxContentRating = pr.User.MaxContentRating
	}
	page, err := s.d.Catalog.Browse(ctx, q)
	if err != nil {
		return hubSpec{}, err
	}
	id := strconv.Itoa(sec.ID)
	hs := hubSpec{Hub: core.Hub{Title: "Recently Added in " + sec.Title, Type: sec.Type, Style: "shelf", Promoted: true, Items: page.Items}}
	if sec.Type == core.TypeShow {
		hs.ID, hs.Context, hs.Type = "tv.recentlyadded."+id, "hub.tv.recentlyadded", ""
		hs.key = "/hubs/home/recentlyAdded?type=2&sectionID=" + id
	} else {
		hs.ID, hs.Context = "movie.recentlyadded."+id, "hub.movie.recentlyadded"
		hs.key = "/library/sections/" + id + "/all?sort=addedAt:desc"
	}
	if len(hs.Items) > count {
		hs.Items, hs.More = hs.Items[:count], true
	}
	return hs, nil
}

// parseSectionList parses "1,2" (contentDirectoryID).

type recentControlCatalog struct {
	core.Catalog
	page    core.Page
	queries []core.BrowseQuery
	err     error
}

func (c *recentControlCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	c.queries = append(c.queries, q)
	return c.page, c.err
}

func TestRecentlyAddedLegacyControl(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	hidden := alien
	hidden.Adult = true
	cat := &recentControlCatalog{Catalog: f.catalog, page: core.Page{Items: []core.Title{hidden, fightClub, bbE1}}}
	f.srv.d.Catalog = cat
	owner := ownerUser
	owner.MaxContentRating = "PG"
	pr := &Request{User: &owner}
	sec, _ := f.srv.sectionFor(core.TypeMovie)
	for _, limit := range []int{0, 1, 5} {
		cat.queries = nil
		want, we := f.srv.controlLegacyRecentlyAddedHub(context.Background(), pr, sec, limit)
		before := append([]core.BrowseQuery(nil), cat.queries...)
		cat.queries = nil
		got, ge := f.srv.recentlyAddedHub(context.Background(), pr, sec, limit)
		if we != nil || ge != nil || !reflect.DeepEqual(want, got) || !reflect.DeepEqual(before, cat.queries) {
			t.Fatalf("limit=%d legacy=%+v %v %v; shared=%+v %v %v", limit, want, we, before, got, ge, cat.queries)
		}
	}
}

func TestRecentlyViewedLegacyControl(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	show := breakingBad
	show.Adult = true
	f.catalog.titles[show.Key] = show
	denied := bbE2
	denied.Adult = true
	f.catalog.titles[denied.Key] = denied
	state := &continueControlState{fakeState: f.state, history: []core.HistoryEntry{{Key: bbE1.Key}, {Key: bbE1.Key}, {Key: core.EpisodeKey(999, 1, 1)}, {Key: denied.Key}, {Key: fightClub.Key}}}
	f.srv.d.State = state
	pr := &Request{User: &ownerUser}
	sec, _ := f.srv.sectionFor(core.TypeShow)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(withRequest(req.Context(), pr))
	want := httptest.NewRecorder()
	f.srv.controlLegacySectionRecentlyViewed(want, req, sec)
	got := httptest.NewRecorder()
	f.srv.sectionRecentlyViewed(got, req, sec)
	if want.Code != got.Code || want.Body.String() != got.Body.String() {
		t.Fatalf("legacy=%d %s; shared=%d %s", want.Code, want.Body.String(), got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), bbE1.Key.String()) || strings.Contains(got.Body.String(), denied.Key.String()) {
		t.Fatalf("legacy render filtering changed: %s", got.Body.String())
	}
	recent := experience.Recent{Visibility: f.srv.visibility(), Unrestricted: func(*core.User) bool { return true }}
	items, err := recent.Viewed(context.Background(), pr.User, sec.ID, state.History, f.srv.getTitles, func(kind core.MediaType) (int, bool) { section, ok := f.srv.sectionFor(kind); return section.ID, ok })
	if err != nil || len(items) != 2 || items[0].Key != bbE1.Key || items[1].Key != denied.Key {
		t.Fatalf("legacy pre-render selection changed: %+v %v", items, err)
	}
}
