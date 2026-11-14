package core

import "context"

// VibeQuery is a natural-language ("vibe") search turned into a structured
// catalogue filter, e.g. "90s thrillers under 100 mins". It backs the
// ephemeral collections synthesized in search results (additive).
type VibeQuery struct {
	// Query is the viewer's original text.
	Query string `json:"query,omitempty"`
	// Title is the human collection title, e.g.
	// "90s Thrillers Under 100 Minutes".
	Title string `json:"title,omitempty"`
	// Filter is the catalogue filter the members are browsed with.
	Filter Filter    `json:"filter"`
	Sort   SortOrder `json:"sort,omitempty"`
	Desc   bool      `json:"desc,omitempty"`
	// SimilarSeed is the unresolved seed title text of a
	// "shows like the office" query; SimilarKey is it resolved.
	SimilarSeed string    `json:"similarSeed,omitempty"`
	SimilarKey  RatingKey `json:"similarKey,omitempty"`
	// PersonName is an unresolved person name ("christopher nolan
	// films"); once resolved it lands in Filter.Person.
	PersonName string `json:"personName,omitempty"`
}

// VibeSearcher is optionally implemented by the collections engine: it
// turns a vibe query into a browseable ephemeral collection for the
// viewer, alongside the collection's top items. ok is false when the
// query is not a vibe query (plain title searches stay untouched).
type VibeSearcher interface {
	VibeCollection(ctx context.Context, user *User, query string) (c *Collection, items []Title, ok bool, err error)
	// PromoteEphemeral saves an ephemeral collection as a persisted
	// personal smart collection for its owner. ok is false when key is
	// not a live ephemeral collection owned by user.
	PromoteEphemeral(ctx context.Context, user *User, key RatingKey) (c *Collection, ok bool, err error)
}

// VibeRefiner maps an otherwise unparseable vibe query to a VibeQuery
// (implemented by the optional LLM service). It must only read cached results
// and schedule bounded asynchronous work on a miss, including under
// LocalResponse. It must never wait for inference, and must respect ctx.
type VibeRefiner interface {
	RefineVibe(ctx context.Context, user *User, query string) (VibeQuery, bool, error)
}
