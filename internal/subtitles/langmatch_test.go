package subtitles

import (
	"reflect"
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
	// Aliases e caixa: pob é pt-br exacto; POR genérico; região errada não casa.
	if got := langMatchTier("pt-br", "pob"); got != 3 {
		t.Errorf("langMatchTier(pt-br, pob) = %d, want 3", got)
	}
	if got := langMatchTier("POR", "PT-BR"); got != 2 {
		t.Errorf("langMatchTier(POR, PT-BR) = %d, want 2", got)
	}
	if got := langMatchTier("en", "pt-br"); got != 0 {
		t.Errorf("langMatchTier(en, pt-br) = %d, want 0", got)
	}
	if got := langMatchTier("", "pt"); got != 0 || langMatchTier("pt", "") != 0 {
		t.Error("empty language must be tier 0")
	}
}

func TestLangNormRegional(t *testing.T) {
	for in, want := range map[string]string{
		"pt-PT": "pt-pt", "zh-CN": "zh-cn", "zh_TW": "zh-tw", "zho-tw": "zh-tw",
		"por-PT": "pt-pt", "pob": "pt-br", "en-GB": "en", "eng-US": "en",
	} {
		if got := normLang(in); got != want {
			t.Errorf("normLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMissingLanguagesRegionalCoverage trava a direção da cobertura:
// have regional cobre want genérico (tier>=2), have genérico NÃO cobre
// want regional (tier 1 baixa), regiões distintas não cobrem (tier 0).
func TestMissingLanguagesRegionalCoverage(t *testing.T) {
	for _, c := range langCorpus {
		user := &core.User{SubtitleLanguages: []string{c.want}}
		embedded := []core.StreamInfo{{Type: "subtitle", Language: c.track}}
		got := MissingLanguages(user, embedded)
		if c.tier >= 2 {
			if len(got) != 0 {
				t.Errorf("want %q have %q (tier %d): missing %v, want covered", c.want, c.track, c.tier, got)
			}
		} else if want := []string{normLang(c.want)}; !reflect.DeepEqual(got, want) {
			t.Errorf("want %q have %q (tier %d): missing %v, want %v", c.want, c.track, c.tier, got, want)
		}
	}
	// Regressão B3: pt genérico com pt-br embarcado não pode baixar duplicado.
	if got := MissingLanguages(&core.User{SubtitleLanguages: []string{"pt"}}, []core.StreamInfo{{Type: "subtitle", Language: "pt-br"}}); len(got) != 0 {
		t.Fatalf("generic pt with embedded pt-br: missing %v, want none", got)
	}
}
