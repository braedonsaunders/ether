package frontend

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// ExternalRating is one provider score a client can name. Score is 0–100.
// Display is the source's own scale ("8.2", "94%"). Votes is zero when the
// provider did not report a count.
type ExternalRating struct {
	Source, Name, Display string
	Score                 float64
	Votes                 int64
}

// DetailFacts is per-viewer detail copy and provider ratings for one title.
// Header is empty when micro-reasons are off or the taste model is too thin
// to state a match. RatingsKnown means the ratings provider is on: Community
// and Critic are nil when that source has no cached value, and callers must
// not substitute the catalogue score. Community is 0–10. Critic is 0–100.
type DetailFacts struct {
	Header       string
	RatingsKnown bool
	Community    *float64
	Critic       *float64
	Sources      []ExternalRating
}

// DetailFactsSource is optional. Experience implementations that do not
// personalize detail copy leave the interface unimplemented.
type DetailFactsSource interface {
	DetailFacts(context.Context, *core.Viewer, core.Title) DetailFacts
	PageRatings(context.Context, *core.Viewer, []core.Title) map[core.RatingKey]DetailFacts
}
