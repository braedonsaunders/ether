package core

// WatchPage carries the anchor clocks used to select next-up items. The row's
// own offset and counters remain separate from its show's selection anchor.
type WatchPage struct {
	Page
	Anchors map[RatingKey]WatchStatus
}
