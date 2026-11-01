package resolver

import (
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func TestExplicitEuropeanLanguagePreferencesKeepTheirRegion(t *testing.T) {
	for _, tc := range []struct{ preference, genericPreference, wanted, other, generic string }{
		{"es-ES", "es", "CASTELLANO", "LATINO", "SPANISH"},
		{"pt-PT", "pt", "PT-PT", "PT-BR", "PORTUGUESE"},
	} {
		t.Run(tc.preference, func(t *testing.T) {
			score := func(tag, preference string) float64 {
				info := releaseparse.Parse("Movie.2019." + tag + ".1080p.WEB-DL-GRP")
				return languageScore(info, []string{preference})
			}
			wanted := score(tc.wanted, tc.preference)
			for _, tag := range []string{tc.other, tc.generic} {
				if got := score(tag, tc.preference); wanted <= got {
					t.Errorf("%s should favor %s (%v) over %s (%v)", tc.preference, tc.wanted, wanted, tag, got)
				}
			}
			generic := score(tc.generic, tc.genericPreference)
			for _, tag := range []string{tc.wanted, tc.other} {
				if got := score(tag, tc.genericPreference); got != generic {
					t.Errorf("generic %s should accept all regions equally: %s=%v, generic=%v", tc.genericPreference, tag, got, generic)
				}
			}
		})
	}
}

func TestImportedLanguageEnumsRetainEuropeanCompatibility(t *testing.T) {
	for _, tc := range []struct{ enum, wanted, other string }{
		{"3", "CASTELLANO", "LATINO"},
		{"18", "PT-PT", "PT-BR"},
		{"30", "PT-BR", "PT-PT"},
		{"37", "LATINO", "CASTELLANO"},
	} {
		spec := compiledSpec{def: core.FormatSpecification{
			Implementation: "LanguageSpecification",
			Fields:         map[string]json.RawMessage{"value": json.RawMessage(tc.enum)},
		}}
		for tag, want := range map[string]bool{tc.wanted: true, tc.other: false} {
			rel := core.Release{Info: releaseparse.Parse("Movie.2019." + tag + ".1080p.WEB-DL-GRP")}
			if got, err := specMatch(spec, rel, ""); err != nil || got != want {
				t.Errorf("enum %s, tag %s: got %v, %v; want %v", tc.enum, tag, got, err, want)
			}
		}
	}
}

// A bare DUAL release carries a local dub plus the original audio, so a
// language preference must not push it down the way a single foreign
// language is; foreign-title ranking orders it separately.
func TestBareDualIsNotPenalizedLikeAForeignLanguage(t *testing.T) {
	dual := languageScore(releaseparse.Parse("Movie.2026.1080p.WEB-DL.DUAL-BiOMA"), []string{"en"})
	untagged := languageScore(releaseparse.Parse("Movie.2026.1080p.WEB-DL-GRP"), []string{"en"})
	foreign := languageScore(releaseparse.Parse("Movie.2026.GERMAN.1080p.WEB-DL-GRP"), []string{"en"})
	if dual != untagged {
		t.Errorf("bare DUAL scored %v, untagged %v; want equal", dual, untagged)
	}
	if foreign >= dual {
		t.Errorf("GERMAN-only scored %v, bare DUAL %v; want GERMAN lower", foreign, dual)
	}
}
