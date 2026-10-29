package plex

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
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

func TestLangMatchTierMatrix(t *testing.T) {
	for _, c := range langCorpus {
		if got := langMatchTier(c.want, c.track); got != c.tier {
			t.Errorf("langMatchTier(%q, %q) = %d, want %d", c.want, c.track, got, c.tier)
		}
	}
	if got := langCanonical("pob"); got != "pt-br" {
		t.Errorf("langCanonical(pob) = %q, want pt-br", got)
	}
	// normLang continua base-only para display (languageName & amigos).
	if got := normLang("pt-br"); got != "pt" {
		t.Errorf("normLang(pt-br) = %q, want pt (display)", got)
	}
	if got := languageName("pt-br"); got != "Portuguese" {
		t.Errorf("languageName(pt-br) = %q, want Portuguese", got)
	}
}

func TestSameLanguageMatrix(t *testing.T) {
	for _, c := range langCorpus {
		want := c.tier > 0
		if got := sameLanguage(c.want, c.track); got != want {
			t.Errorf("sameLanguage(%q, %q) = %v, want %v (tier %d)", c.want, c.track, got, want, c.tier)
		}
		// sameLanguage é simétrico: inverter os lados não muda o resultado.
		if got := sameLanguage(c.track, c.want); got != want {
			t.Errorf("sameLanguage(%q, %q) = %v, want %v (symmetric)", c.track, c.want, got, want)
		}
	}
	if sameLanguage("", "pt") || sameLanguage("pt", "") {
		t.Error("sameLanguage with empty side must be false")
	}
}

func TestPickAudioLanguageTiers(t *testing.T) {
	audio := func(langs ...string) core.MediaInfo {
		info := core.MediaInfo{}
		for i, l := range langs {
			info.Streams = append(info.Streams, core.StreamInfo{Type: "audio", Language: l, Index: i})
		}
		return info
	}
	cases := []struct {
		name   string
		langs  []string
		tracks []string
		pick   int
	}{
		{"generic want accepts regional", []string{"pt"}, []string{"pt-br"}, 0},
		{"generic want prefers exact over regional", []string{"pt"}, []string{"pt-br", "pt"}, 1},
		{"regional want prefers exact over generic", []string{"pt-br"}, []string{"pt", "pt-br"}, 1},
		{"regional want accepts generic, rejects other region", []string{"pt-br"}, []string{"pt-pt", "pt"}, 1},
		{"por generic accepts pt-br", []string{"por"}, []string{"pt-br"}, 0},
		{"zh-tw rejects zh-cn, accepts generic", []string{"zh-tw"}, []string{"zh-cn", "zh"}, 1},
		{"zh generic accepts zh-cn", []string{"zh"}, []string{"zh-cn"}, 0},
	}
	for _, c := range cases {
		if got := pickAudio(audio(c.tracks...), c.langs); got != c.pick {
			t.Errorf("%s: pickAudio(%v, %v) = %d, want %d", c.name, c.tracks, c.langs, got, c.pick)
		}
	}
}
