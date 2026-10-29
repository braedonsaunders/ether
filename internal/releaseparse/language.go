package releaseparse

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/langtag"
)

// Language names are the release parser's lower-case English names
// ("french", "japanese"). A regional variant that release groups label
// separately carries its region after a hyphen ("spanish-latino",
// "portuguese-brazil"); its base is the part before the hyphen.
const (
	LanguageMulti              = "multi"
	LanguageSpanishLatino      = "spanish-latino"
	LanguageSpanishSpain       = "spanish-spain"
	LanguagePortugueseBrazil   = "portuguese-brazil"
	LanguagePortuguesePortugal = "portuguese-portugal"
)

// subtitleTags declare subtitles in a language without claiming its audio
// ("VOSTFR" is the original audio with French subtitles).
var subtitleTags = []struct{ token, name string }{
	{"VOSTFR", "french"}, {"VOST", "french"}, {"SUBFRENCH", "french"}, {"STFR", "french"},
	{"VOSE", "spanish"}, {"SUBESP", "spanish"},
	{"LEGENDADO", LanguagePortugueseBrazil},
	{"SUBITA", "italian"},
	{"ENGSUB", "english"}, {"ENGSUBS", "english"},
}

// isoLanguages names ISO 639-1 primaries the way release language tags
// are named.
var isoLanguages = map[string]string{"en": "english", "fr": "french", "es": "spanish", "de": "german", "it": "italian",
	"da": "danish", "nl": "dutch", "ja": "japanese", "is": "icelandic", "zh": "chinese", "ru": "russian", "uk": "ukrainian", "pl": "polish",
	"vi": "vietnamese", "sv": "swedish", "no": "norwegian", "nb": "norwegian", "nn": "norwegian", "fi": "finnish",
	"tr": "turkish", "pt": "portuguese", "ko": "korean", "hi": "hindi", "te": "telugu", "ta": "tamil", "ar": "arabic",
	"cs": "czech", "hu": "hungarian", "el": "greek", "th": "thai", "id": "indonesian", "cn": "chinese",
	"he": "hebrew", "lt": "lithuanian", "ro": "romanian", "bg": "bulgarian", "fa": "persian", "bn": "bengali",
	"sk": "slovak", "lv": "latvian", "ca": "catalan", "hr": "croatian", "sr": "serbian", "bs": "bosnian",
	"et": "estonian", "mk": "macedonian", "sl": "slovenian", "ml": "malayalam", "kn": "kannada", "sq": "albanian",
	"af": "afrikaans", "mr": "marathi", "tl": "tagalog", "ur": "urdu", "rm": "romansh", "mn": "mongolian",
	"ka": "georgian", "ga": "irish", "ms": "malay"}

// latinAmerica lists the regions whose Spanish releases are labelled
// Latino (UN M.49 419 is Latin America and the Caribbean).
var latinAmerica = map[string]bool{"419": true, "mx": true, "ar": true, "co": true, "cl": true, "pe": true,
	"ve": true, "uy": true, "ec": true, "bo": true, "py": true, "cr": true, "pa": true, "gt": true,
	"hn": true, "sv": true, "ni": true, "do": true, "cu": true, "pr": true, "us": true}

// LanguageName names a language preference or code the way the parser
// names release languages: ISO 639 codes and BCP 47 tags ("fr", "fra",
// "es-419", "pt-BR"), parser names ("french", "Spanish-Latino") and the
// scene words for regional variants ("latino", "castellano", "brazilian").
// Unknown values are returned lower-cased.
func LanguageName(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "":
		return ""
	case "latino", "latam", "spanish (latino)", "spanish latino", "spanish (latin america)":
		return LanguageSpanishLatino
	case "castellano", "castilian", "spanish (spain)":
		return LanguageSpanishSpain
	case "brazilian", "brazilian portuguese", "portuguese (brazil)":
		return LanguagePortugueseBrazil
	case "portuguese (portugal)":
		return LanguagePortuguesePortugal
	}
	if base, _, ok := strings.Cut(s, "-"); ok {
		if _, known := isoLanguages[base]; !known && len(base) > 3 {
			return s // already a parser name with a variant
		}
	}
	if len(s) > 3 && !strings.ContainsAny(s, "-_") {
		return s // a parser name ("french")
	}
	tag, ok := langtag.ParseBCP47(s)
	if !ok {
		return s
	}
	name, ok := isoLanguages[tag.Primary]
	if !ok {
		return s
	}
	switch {
	case name == "spanish" && latinAmerica[tag.Region]:
		return LanguageSpanishLatino
	case name == "spanish" && tag.Region == "es":
		return LanguageSpanishSpain
	case name == "portuguese" && tag.Region == "br":
		return LanguagePortugueseBrazil
	case name == "portuguese" && tag.Region == "pt":
		return LanguagePortuguesePortugal
	}
	return name
}

// LanguageBase is a language name without its regional variant.
func LanguageBase(name string) string {
	base, _, _ := strings.Cut(strings.ToLower(name), "-")
	return base
}

// Language match strengths (LanguageMatch).
const (
	LanguageNoMatch = iota
	// LanguageBaseMatch is the right language in another (or an unstated)
	// regional variant: Castilian or unlabelled Spanish for a Latino
	// preference.
	LanguageBaseMatch
	// LanguageExactMatch is the wanted language; a preference without a
	// variant accepts every variant of its language.
	LanguageExactMatch
)

// LanguageMatch reports how well a release language satisfies a wanted
// language. Both may be any form LanguageName accepts.
func LanguageMatch(release, want string) int {
	r, w := LanguageName(release), LanguageName(want)
	if r == "" || w == "" || LanguageBase(r) != LanguageBase(w) {
		return LanguageNoMatch
	}
	if r == w || !strings.Contains(w, "-") {
		return LanguageExactMatch
	}
	return LanguageBaseMatch
}

// flagLanguages maps the regional-indicator flags addons such as
// Torrentio print for a release's audio languages to language names.
// The Mexican flag is how those addons label Latino Spanish.
var flagLanguages = map[string]string{
	"GB": "english", "US": "english", "JP": "japanese", "RU": "russian", "IT": "italian", "PT": LanguagePortuguesePortugal,
	"BR": LanguagePortugueseBrazil, "ES": LanguageSpanishSpain, "MX": LanguageSpanishLatino, "KR": "korean", "CN": "chinese",
	"TW": "chinese", "HK": "chinese", "FR": "french", "DE": "german", "NL": "dutch", "IN": "hindi", "PL": "polish",
	"LT": "lithuanian", "LV": "latvian", "EE": "estonian", "CZ": "czech", "SK": "slovak", "SI": "slovenian",
	"HU": "hungarian", "RO": "romanian", "BG": "bulgarian", "RS": "serbian", "HR": "croatian", "UA": "ukrainian",
	"GR": "greek", "DK": "danish", "FI": "finnish", "SE": "swedish", "NO": "norwegian", "TR": "turkish",
	"SA": "arabic", "AE": "arabic", "IR": "persian", "IL": "hebrew", "VN": "vietnamese", "ID": "indonesian",
	"MY": "malay", "TH": "thai", "PH": "tagalog", "BA": "bosnian", "MK": "macedonian", "AL": "albanian", "GE": "georgian",
}

// FlagLanguages returns the languages named by flag emoji in text, in
// order of appearance. Unknown flags are ignored.
func FlagLanguages(text string) []string {
	var out []string
	seen := map[string]bool{}
	rs := []rune(text)
	for i := 0; i+1 < len(rs); i++ {
		a, b := rs[i], rs[i+1]
		if !isRegionalIndicator(a) || !isRegionalIndicator(b) {
			continue
		}
		code := string([]rune{'A' + (a - 0x1F1E6), 'A' + (b - 0x1F1E6)})
		i++ // a flag is two indicators; never pair across flags
		if name, ok := flagLanguages[code]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// parseSubtitleLanguages reads subtitle-only language declarations after
// floor (release metadata, not the title).
func parseSubtitleLanguages(spaced string, floor int) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range subtitleTags {
		if languageTagAfter(spaced, l.token, floor) && !seen[l.name] {
			seen[l.name] = true
			out = append(out, l.name)
		}
	}
	return out
}

// multiAudioAfter reports a MULTI audio tag after floor. "Multi-Subs" and
// "MULTi SUB" declare subtitles, not audio.
func multiAudioAfter(spaced string, floor int) bool {
	upper := strings.ToUpper(spaced)
	for from := max(floor+1, 0); ; {
		i := tokenIndex(upper[from:], "MULTI")
		if i < 0 {
			return false
		}
		i += from
		if i > floor {
			rest := strings.TrimLeft(upper[i+len("MULTI"):], " -")
			if !strings.HasPrefix(rest, "SUB") {
				return true
			}
		}
		from = i + len("MULTI")
	}
}
