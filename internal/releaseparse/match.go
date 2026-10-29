package releaseparse

import (
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
)

// diacritics maps common accented latin runes to their base letters.
var diacritics = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'æ': 'a', 'ā': 'a',
	'ç': 'c', 'ć': 'c', 'č': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ę': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i', 'ı': 'i',
	'ñ': 'n', 'ń': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o', 'ō': 'o', 'œ': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u',
	'ý': 'y', 'ÿ': 'y',
	'ß': 's', 'ś': 's', 'š': 's', 'ş': 's',
	'ž': 'z', 'ź': 'z', 'ż': 'z',
	'ł': 'l', 'ð': 'd', 'þ': 't', 'ğ': 'g', 'ř': 'r', 'ť': 't', 'ď': 'd',
}

// NormalizeTitle lowercases, strips diacritics and punctuation, expands
// "&" to "and", and drops a leading article, for title comparisons.
func NormalizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, ".*") {
		s = collapseDottedInitialisms(s)
	}
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "’", "")
	var b strings.Builder
	for _, r := range s {
		if base, ok := diacritics[r]; ok {
			b.WriteRune(base)
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) > 1 {
		switch fields[0] {
		case "the", "a", "an", "le", "la", "les", "der", "die", "das", "el", "il":
			fields = fields[1:]
		}
	}
	return strings.Join(fields, " ")
}

// collapseDottedInitialisms joins single-letter sequences separated by dots
// or asterisks ("L.A." -> "LA", "M*A*S*H" -> "MASH", "A.I." -> "AI") so
// scene release names ("LA.Confidential", "MASH") match punctuated titles.
func collapseDottedInitialisms(s string) string {
	runes := []rune(s)
	var out []rune
	for i := 0; i < len(runes); {
		prevBoundary := i == 0 || !unicode.IsLetter(runes[i-1]) && !unicode.IsDigit(runes[i-1])
		if prevBoundary && unicode.IsLetter(runes[i]) && i+2 < len(runes) && (runes[i+1] == '.' || runes[i+1] == '*') {
			sep := runes[i+1]
			j := i
			var letters []rune
			for j < len(runes) && unicode.IsLetter(runes[j]) && (j+1 == len(runes) || !unicode.IsLetter(runes[j+1]) && !unicode.IsDigit(runes[j+1])) {
				letters = append(letters, runes[j])
				if j+1 < len(runes) && runes[j+1] == sep {
					j += 2
					continue
				}
				j++
				break
			}
			if len(letters) >= 2 {
				out = append(out, letters...)
				if j < len(runes) && unicode.IsLetter(runes[j]) {
					out = append(out, ' ')
				}
				i = j
				continue
			}
		}
		out = append(out, runes[i])
		i++
	}
	return string(out)
}

var tokenCanon = map[string]string{
	"ii": "2", "iii": "3", "iv": "4", "vi": "6", "vii": "7", "viii": "8", "ix": "9",
	"one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
	"versus": "vs", "part": "pt", "volume": "vol",
	"mister": "mr", "doctor": "dr", "saint": "st", "brothers": "bros",
	"international": "intl",
}

var regionalSuffixes = map[string]string{
	"us": "us", "usa": "us",
	"uk": "uk", "gb": "uk",
	"au": "au", "aus": "au",
	"ca": "ca", "can": "ca",
	"nz": "nz",
}

// canonicalTokens normalizes initialisms, numerals and common abbreviations
// across an already-normalized title string.
func canonicalTokens(norm string) []string {
	raw := strings.Fields(norm)
	if len(raw) == 0 {
		return nil
	}
	// Collapse runs of 2+ single-letter ASCII tokens ("l a" -> "la").
	var collapsed []string
	for i := 0; i < len(raw); {
		if len(raw[i]) == 1 && raw[i][0] >= 'a' && raw[i][0] <= 'z' {
			j := i + 1
			for j < len(raw) && len(raw[j]) == 1 && raw[j][0] >= 'a' && raw[j][0] <= 'z' {
				j++
			}
			if j-i >= 2 {
				joined := strings.Join(raw[i:j], "")
				// Strip leading "la" if it was the first token of a 2+ word title
				// so "L.A. Confidential" and "LA Confidential" agree.
				collapsed = append(collapsed, joined)
				i = j
				continue
			}
		}
		collapsed = append(collapsed, raw[i])
		i++
	}
	if len(collapsed) > 1 {
		switch collapsed[0] {
		case "the", "a", "an", "le", "la", "les", "der", "die", "das", "el", "il":
			collapsed = collapsed[1:]
		}
	}
	out := make([]string, 0, len(collapsed))
	for i, tok := range collapsed {
		if i > 0 && (tok == "v" || tok == "x") {
			if tok == "v" {
				tok = "5"
			} else {
				tok = "10"
			}
		} else if c, ok := tokenCanon[tok]; ok && (i > 0 || len(collapsed) == 1) {
			tok = c
		}
		out = append(out, tok)
	}
	return out
}

// TrailingRegionTag returns the canonical region code ("us", "uk", "au",
// "ca", "nz") when a release or show title ends with an explicit country
// disambiguator ("The.Office.US", "Ghosts (UK)"), or "".
func TrailingRegionTag(title string) string {
	fields := strings.Fields(NormalizeTitle(title))
	if len(fields) < 2 {
		return ""
	}
	return regionalSuffixes[fields[len(fields)-1]]
}

// subtitlePrefix extracts the main title before an explicit alternate-title
// clause ("Dr. Strangelove or: How I Learned...", "Birdman or (The Unexpected...)").
// Ordinary colons/dashes ("The Signal: Reloaded") name sequels and must not be stripped.
func subtitlePrefix(raw string) string {
	s := strings.TrimSpace(raw)
	best := -1
	lower := strings.ToLower(s)
	for _, sep := range []string{" or (", " or:"} {
		if idx := strings.Index(lower, sep); idx > 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	if best <= 0 {
		return ""
	}
	prefix := strings.TrimSpace(s[:best])
	norm := NormalizeTitle(prefix)
	words := strings.Fields(norm)
	if len(words) == 0 {
		return ""
	}
	if len(words) == 1 && len(words[0]) < 5 {
		return ""
	}
	return prefix
}

// splitAKA splits a title on whole-word "AKA" or "A.K.A." into its
// alternative names, or returns nil when no AKA separator is present.
func splitAKA(s string) []string {
	norm := NormalizeTitle(s)
	fields := strings.Fields(norm)
	var parts []string
	start := 0
	for i := 0; i < len(fields); i++ {
		if fields[i] == "aka" {
			if i > start {
				parts = append(parts, strings.Join(fields[start:i], " "))
			}
			start = i + 1
		} else if i+2 < len(fields) && fields[i] == "a" && fields[i+1] == "k" && fields[i+2] == "a" {
			if i > start {
				parts = append(parts, strings.Join(fields[start:i], " "))
			}
			start = i + 3
			i += 2
		}
	}
	if len(parts) == 0 {
		return nil
	}
	if start < len(fields) {
		parts = append(parts, strings.Join(fields[start:], " "))
	}
	return parts
}

// TitlesMatch reports whether two raw titles are the same after normalisation.
func TitlesMatch(a, b string) bool {
	na, nb := NormalizeTitle(a), NormalizeTitle(b)
	if na != "" && na == nb {
		return true
	}
	if na == "" || nb == "" {
		return false
	}
	ca := strings.Join(canonicalTokens(na), " ")
	cb := strings.Join(canonicalTokens(nb), " ")
	if ca != "" && ca == cb {
		return true
	}
	// AKA dual-title releases ("Le.Samourai.AKA.The.Godson.1967").
	if parts := splitAKA(a); len(parts) > 0 {
		for _, p := range parts {
			if TitlesMatch(p, b) {
				return true
			}
		}
	}
	if parts := splitAKA(b); len(parts) > 0 {
		for _, p := range parts {
			if TitlesMatch(a, p) {
				return true
			}
		}
	}
	// Regional suffix ("The Office US" vs "The Office", "Ghosts (UK)" vs "Ghosts").
	ta, tb := canonicalTokens(na), canonicalTokens(nb)
	if len(ta) == len(tb)+1 && regionalSuffixes[ta[len(ta)-1]] != "" && strings.Join(ta[:len(ta)-1], " ") == cb {
		return true
	}
	if len(tb) == len(ta)+1 && regionalSuffixes[tb[len(tb)-1]] != "" && strings.Join(tb[:len(tb)-1], " ") == ca {
		return true
	}
	// Explicit subtitle prefix ("Master and Commander: The Far Side of the World"
	// vs scene release "Master.and.Commander.2003").
	if pa := subtitlePrefix(a); pa != "" && subtitlePrefix(b) == "" {
		if TitlesMatch(pa, b) {
			return true
		}
	}
	if pb := subtitlePrefix(b); pb != "" && subtitlePrefix(a) == "" {
		if TitlesMatch(a, pb) {
			return true
		}
	}
	return false
}

// TitleAliases returns alternative search query titles for titles with
// initialisms, explicit subtitles, parenthetical disambiguators or Roman
// numerals. Ordinary titles return nil so standard searches spend no extra
// budget hits.
func TitleAliases(title string) []string {
	raw := strings.TrimSpace(title)
	if raw == "" {
		return nil
	}
	baseNorm := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	seen := map[string]bool{baseNorm: true}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
		if s == "" {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}
	if strings.ContainsAny(raw, ".*") {
		collapsed := collapseDottedInitialisms(raw)
		if collapsed != raw {
			add(collapsed)
		}
	}
	// Strip trailing parenthetical disambiguator, e.g. "The Office (US)" -> "The Office".
	if i := strings.LastIndex(raw, "("); i > 0 && strings.HasSuffix(raw, ")") {
		inner := strings.TrimSpace(raw[i+1 : len(raw)-1])
		stem := strings.TrimSpace(raw[:i])
		if stem != "" {
			add(stem)
			if regionalSuffixes[strings.ToLower(inner)] != "" {
				add(stem + " " + inner)
			}
		}
	}
	if p := subtitlePrefix(raw); p != "" {
		add(p)
	}
	if parts := splitAKA(raw); len(parts) > 0 {
		for _, p := range parts {
			add(p)
		}
	}
	return out
}

// TitleMatches reports whether a parsed release title matches the wanted title.
func TitleMatches(parsed core.ReleaseInfo, want string) bool {
	return TitlesMatch(parsed.Title, want)
}

// TitleYearMatches is TitleMatches plus a year check with ±1 tolerance.
// A zero year on either side skips the year check.
func TitleYearMatches(parsed core.ReleaseInfo, want string, wantYear int) bool {
	if !TitleMatches(parsed, want) {
		return false
	}
	return YearMatches(parsed.Year, wantYear)
}

// YearMatches reports whether two years agree within ±1 (zero = unknown).
func YearMatches(got, want int) bool {
	if got == 0 || want == 0 {
		return true
	}
	d := got - want
	return d >= -1 && d <= 1
}

// EpisodeMatches reports whether parsed info is exactly the wanted episode
// (not a season pack, season matches, episode is listed).
func EpisodeMatches(parsed core.ReleaseInfo, season, episode int) bool {
	if parsed.FullSeason || len(parsed.Episodes) == 0 {
		return false
	}
	seasonOK := len(parsed.Seasons) == 0
	for _, s := range parsed.Seasons {
		if s == season {
			seasonOK = true
		}
	}
	if !seasonOK {
		return false
	}
	for _, e := range parsed.Episodes {
		if e == episode {
			return true
		}
	}
	return false
}

// IsObfuscated reports whether a name/title looks like a hash or random
// string with no readable words.
func IsObfuscated(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	fields := strings.Fields(t)
	if len(fields) == 1 && len(fields[0]) >= 12 {
		w := fields[0]
		hasDigit, hasLetter := false, false
		for _, r := range w {
			if unicode.IsDigit(r) {
				hasDigit = true
			}
			if unicode.IsLetter(r) {
				hasLetter = true
			}
		}
		if hasDigit && hasLetter {
			return true
		}
	}
	// No vowel in a longish single token is a strong obfuscation signal.
	if len(fields) == 1 && len(fields[0]) >= 10 {
		lower := strings.ToLower(fields[0])
		if !strings.ContainsAny(lower, "aeiou") {
			return true
		}
	}
	return false
}

// HasUsableInfo reports whether parsed info carries enough technical
// detail to consider a release whose title cannot be verified.
func HasUsableInfo(info core.ReleaseInfo) bool {
	return info.Height > 0 || info.Source != core.SourceUnknown || info.VideoCodec != ""
}

// EpisodeMatchesAbsolute reports whether parsed info is the episode whose
// number counted from the series start is absolute: an anime release
// numbered that way ("Show - 1071") and naming no season.
func EpisodeMatchesAbsolute(parsed core.ReleaseInfo, absolute int) bool {
	if absolute <= 0 || parsed.FullSeason || len(parsed.Seasons) > 0 {
		return false
	}
	for _, e := range parsed.AbsoluteEpisodes {
		if e == absolute {
			return true
		}
	}
	return false
}

// AnimeSearchEpisode is the absolute episode number an anime episode
// request is also searched by ("Show 1071"), or 0: the catalogue's count,
// or the episode itself in the first season.
func AnimeSearchEpisode(req core.SearchRequest) int {
	if !req.Anime || req.Type != core.TypeEpisode || req.Season <= 0 || req.Episode <= 0 {
		return 0
	}
	if req.AbsoluteEpisode > 0 {
		return req.AbsoluteEpisode
	}
	if req.Season == 1 {
		return req.Episode
	}
	return 0
}
