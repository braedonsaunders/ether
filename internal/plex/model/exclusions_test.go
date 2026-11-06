package model

import (
	"bytes"
	"testing"
)

// Exclusion must recurse in either format without altering the shared response
// model or changing an unfiltered subsequent serialization.
func TestMarshalExclusionsPreserveResponse(t *testing.T) {
	mc := &MediaContainer{Size: 1, Metadata: []Metadata{{Type: "movie", Title: "Visible", RatingKey: "1", Media: []Media{{ID: 99}}}}}
	for _, format := range []Format{XML, JSON} {
		before, err := Marshal(mc, format)
		if err != nil {
			t.Fatal(err)
		}
		filtered, err := MarshalWithExclusions(mc, format, map[string]bool{"title": true}, map[string]bool{"Media": true})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(filtered, []byte("Visible")) || bytes.Contains(filtered, []byte(`id="99"`)) || bytes.Contains(filtered, []byte(`"Media":`)) {
			t.Fatal(string(filtered))
		}
		after, err := Marshal(mc, format)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("response mutated: %s %v", after, err)
		}
	}
}
