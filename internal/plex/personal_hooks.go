package plex

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Netflix-style per-viewer hooks (items #30–#32) in the front end. All
// three are optional capabilities of the recommender (core.PostPlayer,
// core.MicroReasoner, core.IntentTracker) gated by the owner's
// Personalize switches; without them every response is byte-identical to
// the plain PMS behaviour.

// postPlayPicks returns the viewer's ranked hook queue after key, or nil
// when the feature is off/unavailable (callers fall back to Related).
func (s *Server) postPlayPicks(ctx context.Context, pr *Request, key core.RatingKey, count int) []core.PostPlayPick {
	if s.d.Recommender == nil || pr.User == nil || !s.cfg().Personalize.PostPlay {
		return nil
	}
	pp, ok := s.d.Recommender.(core.PostPlayer)
	if !ok {
		return nil
	}
	picks, err := pp.PostPlay(ctx, pr.User, key, s.hubContext(pr, 0), count)
	if err != nil {
		s.log.Debug("postplay picks", "key", key, "err", err)
		return nil
	}
	return picks
}

func (s *Server) postPlaySelection(ctx context.Context, pr *Request, key core.RatingKey, unrestricted bool) experience.PostPlaySelection {
	selection := experience.PostPlaySelection{Visibility: s.visibility(), Unrestricted: unrestricted, Picks: func(ctx context.Context, fetch int) []core.PostPlayPick {
		return s.postPlayPicks(ctx, pr, key, fetch)
	}}
	if s.d.Recommender != nil {
		selection.Related = func(ctx context.Context, count int) ([]core.Hub, error) {
			return s.d.Recommender.Related(ctx, pr.User, key, count)
		}
	}
	return selection
}

// postPlayHub renders ranked picks as one hub in post-play shape. The
// prefix follows the just-watched title's family, like Related's hubs.
func (s *Server) postPlayHub(ctx context.Context, pr *Request, key core.RatingKey, row experience.PostPlayRow) (model.Hub, bool) {
	items := row.Items
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

// decorateDetail adds the viewer's micro-reason decision header (#31) to
// a single-item detail response. It only touches the rendered copy: the
// header prefixes Summary (every app shows it on the detail page), and
// when the title has no tagline of its own it takes the Tagline slot
// instead — the line living-room apps put directly under the title —
// rather than appearing twice. Original text is kept intact.
func (s *Server) decorateDetail(ctx context.Context, pr *Request, t *core.Title, md *model.Metadata) {
	if s.d.Recommender == nil || pr.User == nil || t == nil || md == nil {
		return
	}
	if !s.cfg().Personalize.MicroReasons {
		return
	}
	mr, ok := s.d.Recommender.(core.MicroReasoner)
	if !ok {
		return
	}
	// Copy: the title may be the shared, cached catalogue record and must
	// never carry one viewer's header into another viewer's response.
	cp := *t
	head := mr.MicroReason(ctx, pr.User, &cp, s.now())
	if head.Empty() {
		return
	}
	if strings.TrimSpace(md.Tagline) == "" {
		md.Tagline = head.Header
		return
	}
	if strings.TrimSpace(md.Summary) == "" {
		md.Summary = head.Header
	} else {
		md.Summary = head.Header + "\n\n" + md.Summary
	}
}

// recordDetailIntent feeds a single-item detail open to the live
// session-intent store (#32). Hub/list renders never call this.
func (s *Server) recordDetailIntent(ctx context.Context, pr *Request, t *core.Title) {
	if s.d.Recommender == nil || pr.User == nil || t == nil {
		return
	}
	if !s.cfg().Personalize.SessionIntent {
		return
	}
	if it, ok := s.d.Recommender.(core.IntentTracker); ok {
		cp := *t
		it.RecordDetailOpen(ctx, pr.User, &cp)
	}
}
