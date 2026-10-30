package plex

import (
	"bytes"
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Frozen d318b76d orchestration; compare raw wire bytes without normalization.
func (s *Server) controlBeforecontinueHub(ctx context.Context, pr *Request, v cwVariant, sec Section, count int) hubSpec {
	t := core.MediaType("")
	if v == cwSection {
		t = sec.Type
	}
	items, state := s.controlLegacyInProgressTitles(ctx, pr, t, count+1)
	items = s.discoveryContinue(ctx, pr, items)
	hs := hubSpec{Hub: core.Hub{Title: "Continue Watching", Style: "hero", Promoted: true}, state: state}
	switch v {
	case cwHome:
		hs.ID, hs.Context, hs.key, hs.plain = "home.continue", "hub.home.continue", "/hubs/home/continueWatching", true
	case cwGlobal:
		hs.ID, hs.Context, hs.key, hs.noPromoted = "continueWatching", "hub.continueWatching", "/hubs/continueWatching/items", true
		hs.included = true
	case cwSection:
		id := strconv.Itoa(sec.ID)
		hs.Style, hs.noPromoted = "shelf", true
		hs.key = "/hubs/sections/" + id + "/continueWatching/items"
		if sec.Type == core.TypeShow {
			hs.ID, hs.Context, hs.Type = "tv.inprogress."+id, "hub.tv.inprogress", core.TypeEpisode
		} else {
			hs.ID, hs.Context, hs.Type = "movie.inprogress."+id, "hub.movie.inprogress", core.TypeMovie
		}
	}
	if len(items) > count {
		items, hs.More = items[:count], true
	}
	hs.Items = items
	return hs
}

func (s *Server) controlBeforehandleHubSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("query")) == "" {
		// PMS rejects an empty hub search with its HTML 400 page.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	limit := 10
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 100)
	}
	sectionID, _ := strconv.Atoi(q.Get("sectionId"))
	vibe, vibeItems, isVibe, err := s.controlLegacyVibeSearch(ctx, pr, q.Get("query"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var res []core.Title
	if isVibe {
		res = vibeItems
	} else {
		res, err = s.controlLegacyRunSearch(ctx, pr, q.Get("query"), sectionID, limit*2)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	byType := map[core.MediaType][]core.Title{}
	for _, t := range res {
		byType[t.Type] = append(byType[t.Type], t)
	}
	allowed := map[core.MediaType]bool{}
	for _, t := range s.searchTypes(pr.User, sectionID) {
		allowed[t] = true
		if t == core.TypeShow {
			allowed[core.TypeEpisode] = true
		}
	}
	// PMS returns every result hub, empty ones included, in this order.
	mc := &model.MediaContainer{}
	for _, hd := range []struct {
		t     core.MediaType
		title string
	}{{core.TypeMovie, "Movies"}, {core.TypeEpisode, "Episodes"}, {"collection", "Collections"}, {core.TypeShow, "Shows"}} {
		items := byType[hd.t]
		if !allowed[hd.t] {
			items = nil
		}
		more := false
		if len(items) > limit {
			items, more = items[:limit], true
		}
		h := core.Hub{ID: string(hd.t), Title: hd.title, Type: hd.t, Items: items, More: more, Style: "shelf"}
		mh := s.renderHub(ctx, pr, hubSpec{Hub: h, noPromoted: true}, hubCtx{})
		if hd.t == "collection" && isVibe && vibe != nil {
			mh.Directory = []model.Directory{s.collectionDirectory(ctx, vibe)}
			mh.Size = len(mh.Directory)
		}
		mh.Context, mh.Key, mh.HubKey = "", "", ""
		for i := range mh.Metadata {
			mh.Metadata[i].Score = strconv.FormatFloat(searchScore(i, len(mh.Metadata)), 'f', 5, 64)
		}
		mc.Hub = append(mc.Hub, mh)
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// searchScore is a descending relevance score for the i-th of n results.
func (s *Server) controlBeforehandleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	limit := 50
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 200)
	}
	vibe, vibeItems, isVibe, err := s.controlLegacyVibeSearch(ctx, pr, q.Get("query"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var res []core.Title
	if isVibe {
		res = vibeItems
	} else {
		res, err = s.controlLegacyRunSearch(ctx, pr, q.Get("query"), 0, limit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	searchCollections := false
	if st := q.Get("searchTypes"); st != "" {
		allow := map[core.MediaType]bool{}
		for _, v := range strings.Split(st, ",") {
			switch strings.TrimSpace(v) {
			case "movies", "movie":
				allow[core.TypeMovie] = true
			case "collections", "collection":
				searchCollections = true
			case "tv", "shows", "show":
				allow[core.TypeShow] = true
				allow[core.TypeEpisode] = true
			}
		}
		if len(allow) == 0 {
			res = nil
		} else {
			filtered := res[:0:0]
			for _, t := range res {
				if allow[t.Type] {
					filtered = append(filtered, t)
				}
			}
			res = filtered
		}
	}
	md := s.renderTitles(ctx, pr, res, renderOpts{media: true})
	mc := &model.MediaContainer{}
	for i := range md {
		score := math.Round(searchScore(i, len(md))*10000) / 10000
		// PMS names the section here but sends no section id or key.
		md[i].LibrarySectionID, md[i].LibrarySectionKey = 0, ""
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: score, Metadata: &md[i]})
	}
	if isVibe && vibe != nil && (searchCollections || q.Get("searchTypes") == "") {
		d := s.collectionDirectory(ctx, vibe)
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Directory: &d})
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}

func TestExperienceSelectionWireBytes(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	for _, key := range []core.RatingKey{fightClub.Key, alien.Key, bbE1.Key} {
		f.state.set(ownerUser.ID, core.WatchStatus{Key: key, ViewOffset: time.Minute, LastViewedAt: fixedNow})
	}
	for _, format := range []model.Format{model.XML, model.JSON} {
		pr := &Request{User: &ownerUser, Format: format}
		for _, variant := range []cwVariant{cwHome, cwGlobal, cwSection} {
			for _, limit := range []int{1, 20} {
				old := f.srv.controlBeforecontinueHub(ctx, pr, variant, f.srv.sections[1], limit)
				got := f.srv.continueHub(ctx, pr, variant, f.srv.sections[1], limit)
				// Warm metadata enrichment before comparing the rendered copies.
				f.srv.renderHub(ctx, pr, old, hubCtx{})
				f.srv.waitBackground()
				a, ae := model.Marshal(f.srv.renderHub(ctx, pr, old, hubCtx{}), format)
				b, be := model.Marshal(f.srv.renderHub(ctx, pr, got, hubCtx{}), format)
				if ae != nil || be != nil || !bytes.Equal(a, b) {
					t.Fatalf("continue format=%v variant=%v limit=%d legacy=%s shared=%s errors=%v/%v", format, variant, limit, a, b, ae, be)
				}
			}
		}
		for _, source := range []*vibeControlSource{nil, {ok: false}, {ok: true}, {ok: true, collection: &core.Collection{Title: "Vibe", Key: core.CollectionKey(5)}, items: []core.Title{fightClub, breakingBad}}, {err: errors.New("source unavailable")}} {
			f.srv.d.VibeSearch = source
			if source == nil {
				f.srv.d.VibeSearch = nil
			}
			for _, pair := range []struct {
				path         string
				old, current http.HandlerFunc
			}{
				{"/hubs/search?query=fight&limit=1", f.srv.controlBeforehandleHubSearch, f.srv.handleHubSearch},
				{"/library/search?query=fight&searchTypes=movies,collections", f.srv.controlBeforehandleLibrarySearch, f.srv.handleLibrarySearch},
			} {
				request := httptest.NewRequest("GET", pair.path, nil).WithContext(withRequest(ctx, pr))
				pair.old(httptest.NewRecorder(), request)
				f.srv.waitBackground()
				a, b := httptest.NewRecorder(), httptest.NewRecorder()
				pair.old(a, request)
				pair.current(b, request)
				if a.Code != b.Code || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
					t.Fatalf("search %s format=%v source=%+v status=%d/%d legacy=%s shared=%s", pair.path, format, source, a.Code, b.Code, a.Body, b.Body)
				}
			}
		}
	}
}

func (s *Server) controlBeforeSectionSearch(w http.ResponseWriter, r *http.Request, sec Section, t core.MediaType, query string, mc *model.MediaContainer) {
	ctx := r.Context()
	pr := FromContext(ctx)
	var items []core.Title
	if strings.TrimSpace(query) != "" {
		limit := 50
		if n, _ := strconv.Atoi(r.URL.Query().Get("limit")); n > 0 {
			limit = min(n, 200)
		}
		res, err := s.d.Catalog.Search(ctx, query, []core.MediaType{t}, limit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items = s.filterAllowed(pr.User, res)
	}
	mc.Title2 = "Search for '" + query + "'"
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func TestSectionSearchWireBytes(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	cat := &legacySearchCatalog{Catalog: f.catalog, results: []core.Title{fightClub, alien, breakingBad, bbE1}}
	f.srv.d.Catalog = cat
	f.srv.d.AccessPolicy = legacySearchPolicy{denied: map[core.RatingKey]bool{fightClub.Key: true, breakingBad.Key: true}}
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	for _, viewer := range []*core.User{&ownerUser, {ID: 2, AllowedSections: []int{1}, MaxContentRating: "PG-13"}, {ID: 3, AllowedSections: []int{2}}} {
		for _, format := range []model.Format{model.XML, model.JSON} {
			pr := &Request{User: viewer, Format: format}
			ctx := withRequest(context.Background(), pr)
			for _, kind := range []core.MediaType{core.TypeMovie, core.TypeShow} {
				req := httptest.NewRequest("GET", "/library/sections/1/search?query=fight&limit=1", nil).WithContext(ctx)
				for pass := 0; pass < 2; pass++ {
					cat.limits = nil
					old := httptest.NewRecorder()
					f.srv.controlBeforeSectionSearch(old, req, f.srv.sections[0], kind, "fight", libraryContainer())
					oldLimits := append([]int(nil), cat.limits...)
					f.srv.waitBackground()
					cat.limits = nil
					got := httptest.NewRecorder()
					f.srv.sectionSearch(got, req, f.srv.sections[0], kind, "fight", libraryContainer())
					f.srv.waitBackground()
					if pass == 1 && (old.Code != got.Code || !bytes.Equal(old.Body.Bytes(), got.Body.Bytes()) || len(oldLimits) != 1 || len(cat.limits) != 1 || cat.limits[0] != oldLimits[0]) {
						t.Fatalf("viewer=%+v format=%v kind=%v legacy=%s shared=%s limits=%v/%v", viewer, format, kind, old.Body, got.Body, oldLimits, cat.limits)
					}
				}
			}
		}
	}
}
