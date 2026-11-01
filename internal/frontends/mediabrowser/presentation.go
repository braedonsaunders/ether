package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) pageRatingFacts(r *http.Request, viewer *frontend.Viewer, titles []frontend.Title) map[frontend.RatingKey]frontend.DetailFacts {
	source, ok := i.host.Experience.(frontend.DetailFactsSource)
	if !ok || r == nil || viewer == nil || len(titles) == 0 {
		return nil
	}
	return source.PageRatings(r.Context(), viewer, titles)
}

func applyRatingFacts(item map[string]any, facts frontend.DetailFacts) {
	if !facts.RatingsKnown {
		return
	}
	delete(item, "CommunityRating")
	delete(item, "CriticRating")
	if facts.Community != nil {
		item["CommunityRating"] = *facts.Community
	}
	if facts.Critic != nil {
		item["CriticRating"] = *facts.Critic
	}
}

func applyDetailHeader(item map[string]any, header string) {
	if header == "" {
		return
	}
	tagline := firstTagline(item["Taglines"])
	overview, _ := item["Overview"].(string)
	tagline, overview = frontend.ApplyDetailHeader(tagline, overview, header)
	if tagline != "" {
		item["Taglines"] = []string{tagline}
	}
	if overview != "" {
		item["Overview"] = overview
	}
}

func firstTagline(value any) string {
	switch lines := value.(type) {
	case []string:
		if len(lines) > 0 {
			return lines[0]
		}
	case []any:
		if len(lines) > 0 {
			if text, ok := lines[0].(string); ok {
				return text
			}
		}
	}
	return ""
}

func (i *instance) applyListedRatings(item map[string]any, facts map[frontend.RatingKey]frontend.DetailFacts, key frontend.RatingKey) {
	if facts == nil {
		return
	}
	applyRatingFacts(item, facts[key])
}

func (i *instance) applyOpenedDetail(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	source, ok := i.host.Experience.(frontend.DetailFactsSource)
	if !ok || r == nil || viewer == nil {
		return
	}
	facts := source.DetailFacts(r.Context(), viewer, title)
	applyDetailHeader(item, facts.Header)
	applyRatingFacts(item, facts)
}
