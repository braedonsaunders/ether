package core

// LibraryExclusion is one ordered visibility rule. Slice order is evaluation
// order: the first match wins. Action is "show" or "hide"; an empty Action is
// "hide", so a saved exclusion group stays a hide rule. Conditions inside a
// rule combine with Match "all" or "any". Watch history and metadata are kept.
type LibraryExclusion struct {
	Name      string           `json:"name"`
	Action    string           `json:"action,omitempty"` // show, hide; empty means hide
	MediaType string           `json:"mediaType"`        // movie, show, mixed
	Match     string           `json:"match"`            // all, any
	Rules     []CollectionRule `json:"rules"`
}
