package plex

import (
	"net/http"
	"testing"
)

// Both stream selectors share a route but not a parser: malformed subtitles
// clear the choice, malformed audio preserves it, and -1 restores defaults.
func TestPartSelectionValueGrammars(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	const key int64 = 1000000000000550
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"audioStreamID=0", 400}, {"audioStreamID=-2", 400}, {"subtitleStreamID=-2", 400},
		{"audioStreamID=nonsense", 200}, {"subtitleStreamID=nonsense", 200},
		{"audioStreamID=-1", 200}, {"subtitleStreamID=-1", 200}, {"subtitleStreamID=0", 200},
	} {
		f.srv.selections.update(1, key, func(s *selection) { s.audioSet = true; s.audio = 3; s.subtitleSet = true; s.subtitle = 4 })
		rec := f.request(t, http.MethodPut, "/library/parts/1000000000000550?"+tc.query, nil)
		if rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.query, rec.Code, tc.code)
		}
		sel := f.srv.selections.get(1, key)
		switch tc.query {
		case "audioStreamID=nonsense":
			if !sel.audioSet || sel.audio != 3 {
				t.Error("malformed audio changed selection")
			}
		case "subtitleStreamID=nonsense", "subtitleStreamID=0":
			if !sel.subtitleSet || sel.subtitle != -1 {
				t.Error("subtitle was not cleared")
			}
		case "audioStreamID=-1":
			if sel.audioSet {
				t.Error("audio default did not reset")
			}
		case "subtitleStreamID=-1":
			if sel.subtitleSet {
				t.Error("subtitle default did not reset")
			}
		}
	}
}

// Part identity has priority over malformed track values, and a refusal
// must leave the previously selected title untouched.
func TestPartSelectionResolvesPartBeforeValues(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, part := range []string{"abc", "999", "1000000000999999", "8000000000000001"} {
		for _, query := range []string{"", "?audioStreamID=0", "?subtitleStreamID=-2", "?audioStreamID=-1", "?subtitleStreamID=invalid"} {
			for _, method := range []string{http.MethodPut, http.MethodPost} {
				rec := f.request(t, method, "/library/parts/"+part+query, nil)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s%s: %d", method, part, query, rec.Code)
				}
			}
		}
	}
}

func TestPartSelectionRefusesFullPartKeys(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, path := range []string{"/library/parts/1000000000000550/123/file.mkv", "/library/parts/1000000000000550/123"} {
		for _, method := range []string{http.MethodPut, http.MethodPost} {
			if rec := f.request(t, method, path+"?subtitleStreamID=0", nil); rec.Code != 404 {
				t.Errorf("%s %s: %d", method, path, rec.Code)
			}
		}
	}
}
