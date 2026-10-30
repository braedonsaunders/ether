package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
	"net/http"
)

func (s *Server) legacyHandlePostplay(w http.ResponseWriter, r *http.Request) {
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
		if mh, ok := s.legacyPostPlayHub(ctx, pr, k, kept); ok {
			mc.Hub = append(mc.Hub, mh)
		}
	}
	s.legacyRelatedAfterPostPlay(ctx, pr, mc, k, count, seen)
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Search

func (s *Server) legacyPostPlayHub(ctx context.Context, pr *Request, key core.RatingKey, picks []core.PostPlayPick) (model.Hub, bool) {
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

// relatedAfterPostPlay appends the plain Related rows behind the hook
// queue, dropping titles the queue already shows, as post-play screens
// scroll from the personal pick into the familiar rows.

func (s *Server) legacyRelatedAfterPostPlay(ctx context.Context, pr *Request, mc *model.MediaContainer, key core.RatingKey, count int, seen map[core.RatingKey]bool) {
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

// decorateDetail adds the viewer's micro-reason decision header (#31) to
// a single-item detail response. It only touches the rendered copy: the
// header prefixes Summary (every app shows it on the detail page), and
// when the title has no tagline of its own it takes the Tagline slot
// instead — the line living-room apps put directly under the title —
// rather than appearing twice. Original text is kept intact.

func (s *Server) legacyDiscoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	d := s.d.Discovery
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(hubs []core.Hub) {
		for _, h := range hubs {
			h.Items = s.visibleTitles(pr.User, h.Items)
			if len(h.Items) > 0 {
				out = append(out, hubSpec{Hub: h})
			}
		}
	}
	if d.Watchlist != nil {
		h, _ := d.Watchlist.Hubs(ctx, pr.User, limit)
		add(h)
	}
	if d.Personal != nil {
		h, _ := d.Personal.Hubs(ctx, pr.User, limit)
		add(h)
	}
	return out
}
