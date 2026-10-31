package silo

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) applyDetailFacts(r *http.Request, viewer *frontend.Viewer, title frontend.Title, out map[string]any) {
	source, ok := i.host.Experience.(frontend.DetailFactsSource)
	if !ok || r == nil || viewer == nil {
		return
	}
	facts := source.DetailFacts(r.Context(), viewer, title)
	tagline, _ := out["tagline"].(string)
	overview, _ := out["overview"].(string)
	tagline, overview = frontend.ApplyDetailHeader(tagline, overview, facts.Header)
	if tagline != "" {
		out["tagline"] = tagline
	}
	if overview != "" {
		out["overview"] = overview
	}
	applySiloRatings(out, title, facts)
}

func applySiloRatings(out map[string]any, title frontend.Title, facts frontend.DetailFacts) {
	if !facts.RatingsKnown {
		return
	}
	ratings := make([]any, 0, len(facts.Sources)+1)
	seen := map[string]bool{}
	for _, source := range facts.Sources {
		if source.Source == "" || source.Display == "" {
			continue
		}
		ratings = append(ratings, map[string]any{"source": source.Source, "name": source.Name, "score": source.Score, "display": source.Display})
		seen[source.Source] = true
		switch source.Source {
		case "imdb":
			out["rating_imdb"] = source.Score / 10
		case "rt_critic":
			out["rating_rt_critic"] = int(source.Score)
		case "rt_audience":
			out["rating_rt_audience"] = int(source.Score)
		}
	}
	if title.Rating > 0 && title.Rating <= 10 && !seen["tmdb"] {
		score := title.Rating * 10
		ratings = append(ratings, map[string]any{"source": "tmdb", "name": "TMDB", "score": score, "display": strconv.FormatFloat(title.Rating, 'f', 1, 64)})
	}
	out["ratings"] = ratings
	if len(ratings) == 0 {
		delete(out, "rating_sources")
		return
	}
	sources := make([]any, 0, len(ratings))
	for _, rating := range ratings {
		row, _ := rating.(map[string]any)
		if row == nil {
			continue
		}
		sources = append(sources, map[string]any{"source": row["source"], "name": row["name"], "score": row["score"]})
	}
	out["rating_sources"] = sources
}
