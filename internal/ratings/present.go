package ratings

import (
	"math"
	"strconv"
)

// Presented is the wire-neutral form of one title's cached provider ratings.
// Community is 0–10 and Critic is 0–100, matching the Jellyfin and Emby
// fields Android TV divides by 10 and 100. A nil pointer means that configured
// source had no cached value. Sources lists every cached badge, including
// Letterboxd, under its own name.
type Presented struct {
	Community *float64
	Critic    *float64
	Sources   []PresentedSource
}

// PresentedSource is one named score. Score is 0–100. Display is the
// source scale the viewer reads.
type PresentedSource struct {
	Source, Name, Display string
	Score                 float64
	Votes                 int64
}

// Present maps cached badges through the owner's critic and audience choice.
// Disabled settings and a missing badge produce no number. A zero score that
// the provider actually returned is kept.
func Present(cfg Settings, badges []Badge) Presented {
	var out Presented
	if !cfg.Enabled {
		return out
	}
	for _, badge := range badges {
		view, ok := presentSource(badge)
		if !ok {
			continue
		}
		out.Sources = append(out.Sources, view)
		if badge.Source == cfg.Audience && out.Community == nil {
			value := badge.PlexValue()
			out.Community = &value
		}
		if badge.Source == cfg.Critic && out.Critic == nil {
			value := math.Round(badge.Value / badge.Scale * 100)
			out.Critic = &value
		}
	}
	return out
}

func presentSource(badge Badge) (PresentedSource, bool) {
	if badge.Scale <= 0 || !valid(badge.Value, badge.Scale) {
		return PresentedSource{}, false
	}
	score := math.Round(badge.Value / badge.Scale * 100)
	view := PresentedSource{Score: score, Votes: badge.Votes}
	switch badge.Source {
	case "imdb":
		view.Source, view.Name = "imdb", "IMDb"
		view.Display = strconv.FormatFloat(badge.Value, 'f', 1, 64)
	case "tomatoes":
		view.Source, view.Name = "rt_critic", "RT"
		view.Display = strconv.FormatFloat(score, 'f', 0, 64) + "%"
	case "popcorn":
		view.Source, view.Name = "rt_audience", "RT Audience"
		view.Display = strconv.FormatFloat(score, 'f', 0, 64) + "%"
	case "metacritic":
		view.Source, view.Name = "metacritic", "Metacritic"
		view.Display = strconv.FormatFloat(score, 'f', 0, 64)
	case "letterboxd":
		view.Source, view.Name = "letterboxd", "Letterboxd"
		view.Display = strconv.FormatFloat(badge.Value, 'f', 1, 64)
	default:
		return PresentedSource{}, false
	}
	return view, true
}
