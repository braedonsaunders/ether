package subtitles

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestPreferenceChangesDoNotDownload(t *testing.T) {
	f := newForcedFake(t, []forcedCand{{id: 21, downloads: 9000}, {id: 23, downloads: 100, foreign: true}, {id: 24, downloads: 50, hi: true}})
	s := New(Options{Config: config.OpenSubtitles{Enabled: true, APIKey: "k"}, CacheDir: t.TempDir(), BaseURL: f.srv.URL})
	u := &core.User{SubtitleLanguages: []string{"en"}}
	title := forcedTitle(500)
	embedded := []core.StreamInfo{{Type: "subtitle", Index: 1, Language: "en"}}
	for _, p := range []Preferences{{Mode: "always", Style: "normal"}, {Mode: "forced-only"}, {Mode: "always", Style: "sdh"}} {
		out, e := s.SidecarsForRelease(context.Background(), title, u, nil, "release", p)
		if e != nil || len(out) != 0 {
			t.Fatalf("%+v: %+v %v", p, out, e)
		}
		out, e = s.SidecarsForRelease(context.Background(), title, u, embedded, "release", p)
		if e != nil || len(out) != 0 {
			t.Fatalf("embedded %+v: %+v %v", p, out, e)
		}
	}
	if got := f.downloads(); len(got) != 0 {
		t.Fatalf("preference changes downloaded %v", got)
	}
}
func TestPreferenceEmbeddedCoverage(t *testing.T) {
	u := &core.User{SubtitleLanguages: []string{"en"}}
	for _, tt := range []struct {
		p       Preferences
		st      core.StreamInfo
		missing bool
	}{
		{Preferences{Mode: "forced-only"}, core.StreamInfo{Language: "en"}, true},
		{Preferences{Mode: "forced-only"}, core.StreamInfo{Language: "en", Forced: true}, false},
		{Preferences{Mode: "forced-only", Style: "sdh"}, core.StreamInfo{Language: "en", Forced: true}, false},
		{Preferences{Style: "sdh"}, core.StreamInfo{Language: "en"}, true},
		{Preferences{Style: "sdh"}, core.StreamInfo{Language: "en", Title: "English (SDH)"}, false},
		{Preferences{Style: "normal"}, core.StreamInfo{Language: "en", Forced: true}, true},
		{Preferences{Style: "normal"}, core.StreamInfo{Language: "en"}, false},
	} {
		tt.st.Type = "subtitle"
		if got := len(missingForPreferences(u, []core.StreamInfo{tt.st}, tt.p)) > 0; got != tt.missing {
			t.Errorf("%+v %+v missing=%v", tt.p, tt.st, got)
		}
	}
}
