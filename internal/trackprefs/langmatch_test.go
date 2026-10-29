package trackprefs

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// langCorpus é a MESMA tabela nos 3 pacotes (subtitles, trackprefs, plex)
// para travar a consistência da semântica regional — não alterar só aqui.
var langCorpus = []struct {
	want, track string
	tier        int
}{
	{"pt", "pt", 3}, {"pt", "pt-br", 2}, {"pt", "pt-pt", 2},
	{"pt-br", "pt", 1}, {"pt-br", "pt-br", 3}, {"pt-br", "pt-pt", 0},
	{"pt-pt", "pt", 1}, {"pt-pt", "pt-br", 0}, {"pt-pt", "pt-pt", 3},
	{"por", "pt", 3}, {"por", "pt-br", 2}, {"por", "pt-pt", 2},
	{"zh", "zh", 3}, {"zh", "zh-cn", 2}, {"zh", "zh-tw", 2},
	{"zh-cn", "zh", 1}, {"zh-cn", "zh-cn", 3}, {"zh-cn", "zh-tw", 0},
	{"zh-tw", "zh", 1}, {"zh-tw", "zh-cn", 0}, {"zh-tw", "zh-tw", 3},
}

var tierScore = map[int]int{3: 1000, 2: 960, 1: 920, 0: 0}

func TestLangMatchTierMatrix(t *testing.T) {
	for _, c := range langCorpus {
		if got := langMatchTier(c.want, c.track); got != c.tier {
			t.Errorf("langMatchTier(%q, %q) = %d, want %d", c.want, c.track, got, c.tier)
		}
		if got := lang(c.track); c.tier >= 0 && got == "" {
			t.Errorf("lang(%q) empty", c.track)
		}
	}
	if got := lang("pob"); got != "pt-br" {
		t.Errorf("lang(pob) = %q, want pt-br", got)
	}
}

func TestRankLanguageMatrix(t *testing.T) {
	for _, c := range langCorpus {
		if got, want := rankLanguage(c.track, []string{c.want}), tierScore[c.tier]; got != want {
			t.Errorf("rankLanguage(track %q, want [%q]) = %d, want %d (tier %d)", c.track, c.want, got, want, c.tier)
		}
	}
	// Ordem estrita por tier para o mesmo want, e preferência genérica
	// não pode perder para região errada.
	if !(rankLanguage("pt-br", []string{"pt-br"}) > rankLanguage("pt-br", []string{"pt"}) &&
		rankLanguage("pt-br", []string{"pt"}) > rankLanguage("pt", []string{"pt-br"}) &&
		rankLanguage("pt", []string{"pt-br"}) > rankLanguage("pt-pt", []string{"pt-br"})) {
		t.Error("tier ordering exact > generic×regional > regional×generic > other-region broken")
	}
	// Want genérico casa track regional também via segunda preferência exacta.
	if got := rankLanguage("pt-br", []string{"pt", "pt-br"}); got != 980 {
		t.Errorf("rankLanguage(pt-br, [pt pt-br]) = %d, want 980 (exact at index 1)", got)
	}
}

// Tier fraco (regional×genérico) nunca vence o exacto nem com bónus de
// estilo/default: a penalização de tier (80) excede esses bónus (≤25).
func TestLanguageTierBeatsStyleBonus(t *testing.T) {
	streams := []core.StreamInfo{
		{Type: "subtitle", Language: "pt", Title: "SDH", Default: true},
		{Type: "subtitle", Language: "pt-br"},
	}
	if got := pick(streams, "subtitle", []string{"pt-br"}, "sdh", false, nil); got != 1 {
		t.Fatalf("pick(weak+bonuses vs exact) = %d, want 1 (exact)", got)
	}
	streams[1].Language = "pt-pt" // região errada: tier 0, não é a exacta
	if got := rankLanguage(streams[1].Language, []string{"pt-br"}); got != 0 {
		t.Fatalf("rankLanguage(pt-pt, [pt-br]) = %d, want 0", got)
	}
}

func TestSelectLanguageRegional(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	subs := func(langs ...string) core.MediaInfo {
		info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "en", Index: 0}}}
		for i, l := range langs {
			info.Streams = append(info.Streams, core.StreamInfo{Type: "subtitle", Language: l, Index: i + 1})
		}
		return info
	}
	cases := []struct {
		name   string
		want   string
		tracks []string
		pick   int // posição em info.Streams; -1 = nenhum
	}{
		{"exact beats weak and wrong region", "pt-br", []string{"pt-pt", "pt", "pt-br"}, 3},
		{"generic want prefers exact over regional", "pt", []string{"pt-br", "pt"}, 2},
		{"generic want accepts any region", "pt", []string{"pt-pt", "pt-br"}, 1},
		{"generic por accepts pt-br", "por", []string{"pt-br"}, 1},
		{"regional want accepts generic weakly", "pt-br", []string{"pt-pt", "pt"}, 2},
		{"zh-tw rejects zh-cn accepts generic", "zh-tw", []string{"zh-cn", "zh"}, 2},
		{"zh generic accepts zh-tw", "zh", []string{"zh-tw"}, 1},
	}
	for _, c := range cases {
		r := Default()
		r.SubtitleLanguages = []string{c.want}
		r.SubtitleMode = "always"
		if err := s.PutRules(ctx, 7, r); err != nil {
			t.Fatal(err)
		}
		sel, err := s.Select(ctx, 7, core.MovieKey(1), subs(c.tracks...), "")
		if err != nil {
			t.Fatal(err)
		}
		// Select devolve posição em Streams; pick conta o áudio no índice 0.
		if sel.Subtitle != c.pick {
			t.Errorf("%s: Select(want %q, tracks %v) subtitle = %d, want %d", c.name, c.want, c.tracks, sel.Subtitle, c.pick)
		}
	}
}
