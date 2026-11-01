package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/ratings"
)

type detailPersonalization struct {
	micro   func(context.Context, *core.User, *core.Title, time.Time) core.MicroReason
	enabled func() bool
	ratings *ratings.Service
	now     func() time.Time
}

func (s *surfaceExperience) DetailFacts(ctx context.Context, viewer *core.Viewer, title core.Title) frontend.DetailFacts {
	facts := s.ratingFact(ctx, viewer, []core.Title{title})[title.Key]
	if header := s.detailHeader(ctx, viewer, title); header != "" {
		facts.Header = header
	}
	return facts
}

func (s *surfaceExperience) PageRatings(ctx context.Context, viewer *core.Viewer, titles []core.Title) map[core.RatingKey]frontend.DetailFacts {
	return s.ratingFact(ctx, viewer, titles)
}

func (s *surfaceExperience) detailHeader(ctx context.Context, viewer *core.Viewer, title core.Title) string {
	if s == nil || s.detail.micro == nil || s.detail.enabled == nil || !s.detail.enabled() || viewer == nil || title.Key == 0 {
		return ""
	}
	now := time.Now()
	if s.detail.now != nil {
		now = s.detail.now()
	}
	cp := title
	reason := s.detail.micro(ctx, viewer, &cp, now)
	if reason.Empty() {
		return ""
	}
	return reason.Header
}

func (s *surfaceExperience) ratingFact(ctx context.Context, viewer *core.Viewer, titles []core.Title) map[core.RatingKey]frontend.DetailFacts {
	out := map[core.RatingKey]frontend.DetailFacts{}
	if s == nil || s.detail.ratings == nil || viewer == nil {
		return out
	}
	cfg, err := s.detail.ratings.Settings(ctx)
	if err != nil || !cfg.Enabled {
		return out
	}
	cached, err := s.detail.ratings.CachedMany(ctx, titles)
	if err != nil {
		return out
	}
	for _, title := range titles {
		if title.Key == 0 || (title.Type != core.TypeMovie && title.Type != core.TypeShow) {
			continue
		}
		if _, done := out[title.Key]; done {
			continue
		}
		fact := frontend.DetailFacts{RatingsKnown: true}
		badges, ok := cached[title.Key]
		if !ok {
			s.detail.ratings.Queue(title)
		} else {
			presented := ratings.Present(cfg, badges)
			fact.Community = presented.Community
			fact.Critic = presented.Critic
			fact.Sources = make([]frontend.ExternalRating, 0, len(presented.Sources))
			for _, source := range presented.Sources {
				fact.Sources = append(fact.Sources, frontend.ExternalRating{Source: source.Source, Name: source.Name, Display: source.Display, Score: source.Score, Votes: source.Votes})
			}
		}
		out[title.Key] = fact
	}
	return out
}
