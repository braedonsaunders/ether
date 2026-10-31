package core

// ReleaseRule is a protocol-neutral owner preference. Ordered soft preferences
// break ties lexicographically before ordinary quality scores. All hard rules
// must pass. Empty rules preserve existing profile behavior.
type ReleaseRule struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Action     string             `json:"action"`          // require, prefer, avoid (exclude)
	Match      string             `json:"match,omitempty"` // all (default) or any
	Conditions []ReleaseCondition `json:"conditions"`
	Unless     []ReleaseCondition `json:"unless,omitempty"`   // any matching exception skips the rule
	Fallback   []ReleaseCondition `json:"fallback,omitempty"` // require only, any matching condition
	Disabled   bool               `json:"disabled,omitempty"`
}

// ReleaseCondition inspects declared release metadata, never presumed language.
// Language and subtitle facts describe the indexer's/name's declarations, not
// verified tracks. A missing fact is unknown, not a successful negative match.
// audio_matches_original takes no value: evaluation compares declared audio
// with the catalogue original language supplied beside the release.
type ReleaseCondition struct {
	Field    string  `json:"field"`
	Operator string  `json:"operator"` // equals, contains, regex, range
	Value    string  `json:"value,omitempty"`
	Min      float64 `json:"min,omitempty"`
	Max      float64 `json:"max,omitempty"`
}
