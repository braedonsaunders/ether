package plex

// Frozen d318b76d postplay pipeline: compare raw bytes, with no normalization.
import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"testing"
)

func (s *Server) controlLegacyHandlePostplay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	count := countParam(r, 12)
	mc := hubContainer()
	seen := map[core.RatingKey]bool{k: true}
	if h, ok := s.upNextHub(ctx, pr, k); ok {
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, h, hubCtx{related: k}))
		seen[h.Items[0].Key] = true
	}
	if picks := s.postPlayPicks(ctx, pr, k, count); len(picks) > 0 {
		// Skip whatever Up Next already covers so the queue never
		// repeats the next episode as a "recommendation".
		kept := picks[:0:0]
		for _, p := range picks {
			if !seen[p.Title.Key] {
				seen[p.Title.Key] = true
				kept = append(kept, p)
			}
		}
		if mh, ok := s.controlLegacyPostPlayHub(ctx, pr, k, kept); ok {
			mc.Hub = append(mc.Hub, mh)
		}
	}
	s.controlLegacyRelatedAfterPostPlay(ctx, pr, mc, k, count, seen)
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Search

func (s *Server) controlLegacyPostPlayHub(ctx context.Context, pr *Request, key core.RatingKey, picks []core.PostPlayPick) (model.Hub, bool) {
	items := make([]core.Title, 0, len(picks))
	for _, p := range picks {
		items = append(items, p.Title)
	}
	items = s.filterAllowed(pr.User, items)
	if len(items) == 0 {
		return model.Hub{}, false
	}
	prefix := "movie"
	if k := key.Kind(); k == core.KindShow || k == core.KindSeason || k == core.KindEpisode {
		prefix = "tv"
	}
	h := core.Hub{
		ID: prefix + ".postplay", Title: "Recommended For You",
		Context: "hub." + prefix + ".postplay", Style: "shelf", Items: items,
	}
	return s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}), true
}

// controlRelatedAfterPostPlay appends the plain Related rows behind the hook
// queue, dropping titles the queue already shows, as post-play screens
// scroll from the personal pick into the familiar rows.

func (s *Server) controlLegacyRelatedAfterPostPlay(ctx context.Context, pr *Request, mc *model.MediaContainer, key core.RatingKey, count int, seen map[core.RatingKey]bool) {
	if s.d.Recommender == nil {
		return
	}
	hubs, err := s.d.Recommender.Related(ctx, pr.User, key, count)
	if err != nil {
		s.log.Debug("postplay related hubs", "key", key, "err", err)
		return
	}
	for _, h := range hubs {
		items := h.Items[:0:0]
		for _, t := range h.Items {
			if !seen[t.Key] {
				seen[t.Key] = true
				items = append(items, t)
			}
		}
		if len(items) == 0 {
			continue
		}
		h.Items = items
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}))
	}
}

func (s *Server) controlPostPlayRow(ctx context.Context, pr *Request, key core.RatingKey, count int, seen map[core.RatingKey]bool) experience.PostPlayRow {
	return experience.LoadPostPlay(ctx, pr.User, s.visibility(), count, seen, func(ctx context.Context, fetch int) []core.PostPlayPick {
		return s.postPlayPicks(ctx, pr, key, fetch)
	})
}

func controlCloneSeen(seen map[core.RatingKey]bool) map[core.RatingKey]bool {
	out := make(map[core.RatingKey]bool, len(seen))
	for key, value := range seen {
		out[key] = value
	}
	return out
}

func (s *Server) controlRelatedAfterPostPlay(ctx context.Context, pr *Request, mc *model.MediaContainer, key core.RatingKey, count int, seen map[core.RatingKey]bool) {
	if s.d.Recommender == nil {
		return
	}
	var hubs []core.Hub
	rows, err := experience.LoadRelated(ctx, pr.User, s.visibility(), count, seen, func(ctx context.Context, fetch int) ([]experience.RelatedCandidate, error) {
		var err error
		hubs, err = s.d.Recommender.Related(ctx, pr.User, key, fetch)
		if err != nil {
			return nil, err
		}
		rows := make([]experience.RelatedCandidate, len(hubs))
		for i, hub := range hubs {
			rows[i] = experience.RelatedCandidate{Index: i, Items: hub.Items}
		}
		return rows, nil
	})
	if err != nil {
		s.log.Debug("postplay related hubs", "key", key, "err", err)
		return
	}
	for _, row := range rows {
		h := hubs[row.Index]
		h.Items = row.Items
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}))
	}
}

func (s *Server) controlHandlePostplay(w http.ResponseWriter, r *http.Request) {
	if !s.viewerRestricted(FromContext(r.Context()).User) {
		s.controlLegacyHandlePostplay(w, r)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	count := countParam(r, 12)
	mc := hubContainer()
	seen := map[core.RatingKey]bool{k: true}
	if h, ok := s.upNextHub(ctx, pr, k); ok {
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, h, hubCtx{related: k}))
		seen[h.Items[0].Key] = true
	}
	if row := s.controlPostPlayRow(ctx, pr, k, count, seen); len(row.Items) > 0 {
		if mh, ok := s.postPlayHub(ctx, pr, k, row); ok {
			mc.Hub = append(mc.Hub, mh)
		}
	}
	s.controlRelatedAfterPostPlay(ctx, pr, mc, k, count, seen)
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

func TestPostplaySelectionMatchesFrozenBytes(t *testing.T) {
	for _, format := range []model.Format{model.JSON, model.XML} {
		for _, owner := range []bool{true, false} {
			for _, enabled := range []bool{true, false} {
				for _, key := range []core.RatingKey{fightClub.Key, bbE1.Key} {
					var bodies [][]byte
					for _, control := range []bool{true, false} {
						f, _ := hookFixture(t, enabled)
						viewer := ownerUser
						if !owner {
							viewer = friendUser
							viewer.AllowedSections = []int{1, 2}
						}
						request := httptest.NewRequest(http.MethodGet, "/hubs/metadata/"+key.String()+"/postplay", nil)
						route := chi.NewRouteContext()
						route.URLParams.Add("key", key.String())
						ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
						request = request.WithContext(withRequest(ctx, &Request{User: &viewer, Format: format, ClientID: "client-1"}))
						response := httptest.NewRecorder()
						if control {
							f.srv.controlHandlePostplay(response, request)
						} else {
							f.srv.handlePostplay(response, request)
						}
						if response.Code != http.StatusOK {
							t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
						}
						bodies = append(bodies, append([]byte(nil), response.Body.Bytes()...))
					}
					if !bytes.Equal(bodies[0], bodies[1]) {
						t.Fatalf("format=%v owner=%v enabled=%v key=%v\ncontrol=%s\nshared=%s", format, owner, enabled, key, bodies[0], bodies[1])
					}
				}
			}
		}
	}
}
