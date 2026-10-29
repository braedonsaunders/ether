package releaseparse

import (
	"reflect"
	"testing"
)

func TestAudioAndSubtitleLanguagesStaySeparate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		audio, subs []string
	}{
		{"Movie.2019.VOSTFR.1080p.WEB.x264-GRP", nil, []string{"french"}},
		{"Movie.2019.MULTi.VOSTFR.1080p.WEB.x264-GRP", []string{"multi"}, []string{"french"}},
		{"Movie.2019.FRENCH.1080p.WEB.x264-GRP", []string{"french"}, nil},
		{"Movie.2019.TRUEFRENCH.1080p.BluRay.x264-GRP", []string{"french"}, nil},
		{"Pelicula.2019.VOSE.1080p.WEB-DL-GRP", nil, []string{"spanish"}},
		{"Filme.2019.1080p.WEB-DL.LEGENDADO-GRP", nil, []string{"portuguese-brazil"}},
		{"Movie.2019.1080p.WEB-DL.Multi-Subs-GRP", nil, nil},
		{"Movie.2019.1080p.WEB-DL.MULTi.SUB-GRP", nil, nil},
		{"Movie 2019 1080p WEB-DL Dual-Audio", []string{"multi"}, nil},
		// Title words are neither audio nor subtitle declarations.
		{"Vost.2019.1080p.WEB-DL-GRP", nil, nil},
	} {
		info := Parse(tc.name)
		if !reflect.DeepEqual(info.Languages, tc.audio) || !reflect.DeepEqual(info.SubtitleLanguages, tc.subs) {
			t.Errorf("%s: audio=%v subs=%v, want audio=%v subs=%v", tc.name, info.Languages, info.SubtitleLanguages, tc.audio, tc.subs)
		}
	}
}

func TestRegionalSpanishAndPortugueseVariants(t *testing.T) {
	for name, want := range map[string][]string{
		"Pelicula.2019.LATINO.1080p.WEB-DL-GRP":        {"spanish-latino"},
		"Pelicula.2019.CASTELLANO.1080p.WEB-DL-GRP":    {"spanish-spain"},
		"Pelicula.2019.SPANISH.1080p.WEB-DL-GRP":       {"spanish"},
		"Pelicula.2019.1080p.WEB-DL.ES-MX-GRP":         {"spanish-latino"},
		"Filme.2019.1080p.WEB-DL.DUBLADO-GRP":          {"portuguese-brazil"},
		"Filme.2019.1080p.WEB-DL.PT-PT-GRP":            {"portuguese-portugal"},
		"Filme.2019.PORTUGUESE.1080p.WEB-DL-GRP":       {"portuguese"},
		"Pelicula.2019.CASTELLANO.LATINO.1080p.WEB-DL": {"spanish-spain", "spanish-latino"},
	} {
		if got := Parse(name).Languages; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: languages %v, want %v", name, got, want)
		}
	}
}

func TestFlagLanguages(t *testing.T) {
	for text, want := range map[string][]string{
		"👤 12 💾 2.1 GB ⚙️ ThePirateBay\n🇬🇧 / 🇮🇹": {"english", "italian"},
		"Multi Audio / 🇲🇽 / 🇪🇸":                  {"spanish-latino", "spanish-spain"},
		"🇧🇷 🇵🇹":                                  {"portuguese-brazil", "portuguese-portugal"},
		"🇫🇷🇫🇷":                                   {"french"},
		"no flags here":                          nil,
		"🇦🇶 Antarctica has no language":          nil,
		"lone indicator 🇫 then text":             nil,
		"🇯🇵" + "🇵":                               {"japanese"}, // a stray indicator after a flag
	} {
		if got := FlagLanguages(text); !reflect.DeepEqual(got, want) {
			t.Errorf("FlagLanguages(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestLanguageName(t *testing.T) {
	for in, want := range map[string]string{
		"fr": "french", "fra": "french", "FRE": "french", "French": "french",
		"es": "spanish", "es-ES": "spanish-spain", "es-419": "spanish-latino", "es-MX": "spanish-latino",
		"es_AR": "spanish-latino", "Latino": "spanish-latino", "castellano": "spanish-spain",
		"pt": "portuguese", "pt-PT": "portuguese-portugal", "pt-BR": "portuguese-brazil", "pob": "portuguese-brazil",
		"Brazilian": "portuguese-brazil", "spanish-latino": "spanish-latino", "en-GB": "english",
		"zh-TW": "chinese", "": "", "klingon": "klingon", "xx": "xx",
	} {
		if got := LanguageName(in); got != want {
			t.Errorf("LanguageName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLanguageMatch(t *testing.T) {
	for _, tc := range []struct {
		release, want string
		match         int
	}{
		{"french", "fr", LanguageExactMatch},
		{"spanish-spain", "es-ES", LanguageExactMatch},
		{"spanish-latino", "es-ES", LanguageBaseMatch},
		{"spanish", "es-ES", LanguageBaseMatch},
		{"portuguese-portugal", "pt-PT", LanguageExactMatch},
		{"portuguese-brazil", "pt-PT", LanguageBaseMatch},
		{"portuguese", "pt-PT", LanguageBaseMatch},
		{"spanish-latino", "es", LanguageExactMatch}, // a plain Spanish preference takes any Spanish
		{"spanish", "es", LanguageExactMatch},
		{"spanish-latino", "es-419", LanguageExactMatch},
		{"spanish", "es-419", LanguageBaseMatch}, // Castilian for a Latino preference
		{"portuguese", "pt-BR", LanguageBaseMatch},
		{"portuguese-brazil", "pt-BR", LanguageExactMatch},
		{"english", "es-419", LanguageNoMatch},
		{"", "fr", LanguageNoMatch},
	} {
		if got := LanguageMatch(tc.release, tc.want); got != tc.match {
			t.Errorf("LanguageMatch(%q, %q) = %d, want %d", tc.release, tc.want, got, tc.match)
		}
	}
}
