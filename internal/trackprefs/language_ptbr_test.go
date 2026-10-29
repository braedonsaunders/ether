package trackprefs

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A pt-BR preference must pick the pt-BR track over a pt-PT track listed
// first, and pob must be understood as pt-BR.
func TestLanguagePtBRSelect(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	r := Default()
	r.SubtitleLanguages = []string{"pt-BR"}
	r.SubtitleMode = "always"
	r.SubtitleStyle = "normal"
	if err := s.PutRules(ctx, 0, r); err != nil {
		t.Fatal(err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "subtitle", Language: "pt-PT"},
		{Type: "subtitle", Language: "pt-BR"},
	}}
	sel, err := s.Select(ctx, 7, core.MovieKey(1), info, "")
	if err != nil || sel.Subtitle != 1 {
		t.Fatalf("pt-BR preference must select pt-BR track: %+v %v", sel, err)
	}
	info.Streams[1].Language = "pob"
	sel, err = s.Select(ctx, 7, core.MovieKey(1), info, "")
	if err != nil || sel.Subtitle != 1 {
		t.Fatalf("pob must count as pt-BR: %+v %v", sel, err)
	}
	if lang("pt-BR") != "pt-br" || lang("pob") != "pt-br" || lang("zh-TW") != "zh-tw" || lang("pt-PT") == lang("pt-BR") {
		t.Fatalf("lang normalisation: pt-BR=%q pob=%q zh-TW=%q pt-PT=%q", lang("pt-BR"), lang("pob"), lang("zh-TW"), lang("pt-PT"))
	}
}

// en-GB/en_US fold to en (only pt/zh keep a region).
func TestLanguageEnRegionFolding(t *testing.T) {
	for _, c := range []string{"en-GB", "en_US", "EN-gb"} {
		if got := lang(c); got != "en" {
			t.Fatalf("lang(%q)=%q want en", c, got)
		}
	}
}

// SDH detection needs a delimited marker: an "SDH" token buried in a
// release name is not a hearing-impaired track, while the generated
// sidecar marker " (SDH)" (and the legacy bare " SDH" suffix) is.
func TestSDHDelimitedMarker(t *testing.T) {
	for _, title := range []string{
		"EN (OpenSubtitles: Some.Release) (SDH)", // generated sidecar
		"English SDH",                            // legacy generated suffix
		"English (SDH)", "English [SDH]", "SDH", "English - SDH subs",
	} {
		if !sdh(core.StreamInfo{Title: title}) {
			t.Errorf("sdh(%q)=false, want true", title)
		}
	}
	for _, title := range []string{
		"Movie.2024.SDH.WEB-GRP", "Show.S01E01-SDH", "SDH.WEB-GRP",
		"Fight.Club.1999.SDH", "English", "",
	} {
		if sdh(core.StreamInfo{Title: title}) {
			t.Errorf("sdh(%q)=true, want false (release-name token is not HI)", title)
		}
	}
}

// Mutation target: pick's memory bonus must not be vetoed by SDH
// inequality. A legacy memory (SDH=false, recorded before sidecars
// carried the " (SDH)" marker) for an English track must still beat the
// rules-preferred Portuguese track when the English track now surfaces
// as SDH; before the fix the gate sdh(t)==memory.SDH silently dropped
// the +10000 bonus and inverted the remembered choice.
func TestPickMemoryLegacySDHDoesNotVeto(t *testing.T) {
	streams := []core.StreamInfo{
		{Type: "subtitle", Language: "pt", Title: "Portuguese"},
		{Type: "subtitle", Language: "en", Title: "EN (OpenSubtitles: Rel) (SDH)"},
	}
	mem := &Choice{Language: "en", SDH: false} // legacy, pre-marker
	if got := pick(streams, "subtitle", []string{"pt", "en"}, "normal", false, mem); got != 1 {
		t.Fatalf("legacy memory must still pick the remembered en track, got %d", got)
	}
	// When the style explicitly asks for SDH, SDH equality adds score on
	// top of the memory bonus.
	plainFirst := []core.StreamInfo{
		{Type: "subtitle", Language: "en", Title: "English"},
		{Type: "subtitle", Language: "en", Title: "English (SDH)"},
	}
	if got := pick(plainFirst, "subtitle", []string{"en"}, "sdh", false, &Choice{Language: "en", SDH: true}); got != 1 {
		t.Fatalf("sdh style+memory must pick the SDH track, got %d", got)
	}
}
