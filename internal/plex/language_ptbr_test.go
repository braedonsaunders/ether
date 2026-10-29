package plex

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// End-to-end pt-BR: subtitles.MissingLanguages decides a sidecar is needed,
// the sidecar's language must still mean pt-BR (not generic pt) when Plex
// compares/tags it and when trackprefs selects among subtitle tracks.
func TestLanguagePtBREndToEnd(t *testing.T) {
	ctx := context.Background()
	embeddedPT := core.StreamInfo{Type: "subtitle", Language: "pt-PT", Index: 2}
	missing := subtitles.MissingLanguages(&core.User{SubtitleLanguages: []string{"pt-BR"}}, []core.StreamInfo{embeddedPT})
	if len(missing) != 1 || missing[0] != "pt-br" {
		t.Fatalf("subtitles: pt-PT must not satisfy pt-BR, missing=%v", missing)
	}
	sidecar := core.StreamInfo{Type: "subtitle", Language: missing[0], Codec: "srt", External: true, ExternalURL: "ether-subtitle:x"}

	if !sameLanguage(sidecar.Language, "pt-BR") || !sameLanguage(sidecar.Language, "pob") {
		t.Fatalf("plex: %q must be the same language as pt-BR/pob", sidecar.Language)
	}
	if sameLanguage(sidecar.Language, "pt-PT") {
		t.Fatalf("plex: %q must not collapse into European Portuguese", sidecar.Language)
	}
	// B3: sameLanguage is the symmetric tier>0 view, so a pt-BR track
	// weakly matches generic pt (tier 1) — distinctness lives in the
	// tiers/MissingLanguages/languageTag, not in collapsing to equality.
	if !sameLanguage(sidecar.Language, "pt") {
		t.Fatalf("plex: %q must weakly match generic pt (B3 tier 1)", sidecar.Language)
	}
	if got := languageTag(sidecar.Language); got != "pt-br" {
		t.Fatalf("plex: languageTag(pt-BR sidecar)=%q want pt-br", got)
	}
	if got := languageName(sidecar.Language); got != "Portuguese" {
		t.Fatalf("plex: languageName(pt-BR sidecar)=%q want Portuguese", got)
	}
	// The Plex code3 stays the base language's code even though Language
	// keeps the region; pob is an alias for pt-BR on every surface.
	if got := languageCode3(sidecar.Language); got != "por" {
		t.Fatalf("plex: languageCode3(pt-br)=%q want por", got)
	}
	if got := languageCode3("pob"); got != "por" || languageTag("pob") != "pt-br" {
		t.Fatalf("plex: pob must map to por/pt-br, got code3=%q tag=%q", languageCode3("pob"), languageTag("pob"))
	}
	// en-GB/en_US fold to en (only pt/zh keep a region).
	if languageTag("en-GB") != "en" || languageCode3("en_US") != "eng" || normLang("en_US") != "en" {
		t.Fatalf("plex: en region folding: tag=%q code3=%q norm=%q", languageTag("en-GB"), languageCode3("en_US"), normLang("en_US"))
	}
	// The exact form the native probe now emits for a BCP47-only pt-BR
	// track ("pt-br") satisfies both consumers end to end.
	probed := core.StreamInfo{Type: "subtitle", Language: "pt-br", Index: 3}
	if miss := subtitles.MissingLanguages(&core.User{SubtitleLanguages: []string{"pt-BR"}}, []core.StreamInfo{probed}); len(miss) != 0 {
		t.Fatalf("probed pt-br track must satisfy pt-BR: missing=%v", miss)
	}

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefs := trackprefs.New(db)
	r := trackprefs.Default()
	r.SubtitleLanguages = []string{"pt-BR"}
	r.SubtitleMode = "always"
	if err := prefs.PutRules(ctx, 0, r); err != nil {
		t.Fatal(err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{embeddedPT, sidecar}}
	sel, err := prefs.Select(ctx, 7, core.MovieKey(1), info, "")
	if err != nil || sel.Subtitle != 1 {
		t.Fatalf("trackprefs: pt-BR preference must select the pt-BR sidecar: %+v %v", sel, err)
	}
}
