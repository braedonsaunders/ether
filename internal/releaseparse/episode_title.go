package releaseparse

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

var reMiniseriesPart = regexp.MustCompile(`(?i)\b(?:Part|Pt)\s*(0?[1-9]|[12]\d|I|II|III|IV|V|VI|VII|VIII|IX|X|One|Two|Three|Four|Five|Six|Seven|Eight|Nine|Ten)\b`)

var partNumbers = map[string]int{
	"i": 1, "ii": 2, "iii": 3, "iv": 4, "v": 5,
	"vi": 6, "vii": 7, "viii": 8, "ix": 9, "x": 10,
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
}

// MiniseriesPart extracts the series title and 1-based part number from a
// miniseries release named "Show.Name.Part.3.Episode.Title.1080p.BluRay" or
// "Show.Name.1990.Pt.01.720p". It is used only when resolving a Season 1
// episode so movie titles ("Dune Part Two") are unaffected.
func MiniseriesPart(name string) (seriesTitle string, part int, ok bool) {
	spaced := spacify(name)
	loc := reMiniseriesPart.FindStringSubmatchIndex(spaced)
	if loc == nil || loc[0] == 0 {
		return "", 0, false
	}
	tok := strings.ToLower(strings.TrimSpace(spaced[loc[2]:loc[3]]))
	if n, err := strconv.Atoi(tok); err == nil && n > 0 {
		part = n
	} else if n, found := partNumbers[tok]; found {
		part = n
	}
	if part <= 0 {
		return "", 0, false
	}
	prefix := strings.Trim(spaced[:loc[0]], " -")
	// Strip a trailing series premiere year from the prefix ("It 1990 Part 1").
	if y, yAt := parseYearAt(prefix); y > 0 && yAt > 0 {
		prefix = strings.Trim(prefix[:yAt], " -")
	}
	if prefix == "" {
		return "", 0, false
	}
	return prefix, part, true
}

func isGenericEpisodeLabel(word string) bool {
	switch word {
	case "episode", "episodio", "capitulo", "capítulo", "серия", "эпизод", "folge", "épisode":
		return true
	}
	return false
}

// EpisodeTitleMatches rejects an explicitly different episode title between
// SxxExx and the technical metadata. Unnamed scene releases remain eligible.
// A title is only extracted when a recognized metadata boundary follows it.
func EpisodeTitleMatches(name, want string) bool {
	return episodeTitleMatches(name, want, false)
}

// EpisodeTitleEvidence reports a positively matching episode subtitle. Unlike
// EpisodeTitleMatches, missing or generic subtitles carry no evidence.
func EpisodeTitleEvidence(name, want string) bool {
	return episodeTitleMatches(name, want, true)
}

func episodeTitleMatches(name, want string, requireEvidence bool) bool {
	if strings.TrimSpace(want) == "" {
		return !requireEvidence
	}
	generic := strings.Fields(strings.ToLower(want))
	if len(generic) == 2 && isGenericEpisodeLabel(generic[0]) {
		if _, err := strconv.Atoi(generic[1]); err == nil {
			return !requireEvidence
		}
	}
	if absolute, start, end := seasonAbsoluteAnnotation(name); absolute > 0 {
		name = name[:start] + " " + name[end:]
	}
	spaced := spacify(normalizeSpanishCapRange(name))
	usedMiniseriesPart := false
	loc := reSE.FindStringIndex(spaced)
	if loc == nil {
		loc = reSeasonAndEp.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reXEpisode.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reBareEpisode.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = spanishEpisodeIndexWithRaw(name, spaced)
	}
	if loc == nil {
		if loc = reMiniseriesPart.FindStringIndex(spaced); loc != nil {
			usedMiniseriesPart = true
		}
	}
	if loc == nil {
		return !requireEvidence
	}
	tail := strings.Trim(spaced[loc[1]:], " -")
	var words []string
	boundary := false
	tailWords := strings.Fields(tail)
	for i, w := range tailWords {
		u := strings.ToUpper(strings.Trim(w, "-"))
		base := strings.SplitN(u, "-", 2)[0]
		technical := false
		for _, kw := range boundaryKeywords {
			if u == kw || base == kw {
				technical = true
				break
			}
		}
		for _, l := range languages {
			if u == l.token {
				technical = true
				break
			}
		}
		if shortLanguageName(u) != "" && reAudioAfterLanguage.MatchString(" "+strings.Join(tailWords[i+1:], " ")) {
			technical = true
		}
		switch u {
		case "AAC", "AC3", "EAC3", "DDP", "DDP5", "DTS", "TRUEHD", "MP3", "FLAC", "OPUS",
			"READNFO", "UNCENSORED", "EXTENDED", "WS", "FS", "NTSC", "PAL",
			// Release tags that can precede the resolution; they are not
			// episode title words ("Show.S01E01.DV.2160p.WEB").
			"DV", "DOVI", "HDR10PLUS", "SDR", "HYBRID", "RERIP", "REAL", "10BIT", "DUAL", "DL",
			"SUBT", "SUBTITULADO", "VOSE", "VOS",
			"AMZN", "NF", "DSNP", "HMAX", "ATVP", "APTV", "APTV+", "PCOK", "HULU", "UNCUT", "REMASTERED", "IMAX":
			technical = true
		}
		if technical {
			boundary = true
			break
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return !requireEvidence
	}
	if !boundary && !requireEvidence {
		return true
	}
	got := normalizeEpisodeTitle(strings.Join(words, " "))
	expected := normalizeEpisodeTitle(want)
	if requireEvidence {
		got = joinEpisodePossessives(got)
		expected = joinEpisodePossessives(expected)
	}
	if got == "" || got == "the" || got == "a" || got == "an" {
		return !requireEvidence
	}
	// Russian and P2P releases frequently place the season/air year right
	// after SxxExx ("Doktor.Krasnov.S01.E01.2023.WEB-DLRip", "s02.E01.(2025).WEBRip").
	// When the wanted episode title is not itself a year, that bare year is
	// release metadata rather than a conflicting episode title.
	if len(words) == 1 && reYear.MatchString(got) && !reYear.MatchString(expected) {
		return !requireEvidence
	}
	if usedMiniseriesPart {
		// When "Part N" was the episode marker itself ("Band.of.Brothers.Part.1.Currahee"),
		// a catalogue episode titled "Part 1" / "Part One" (normalized to "1") or
		// "Part 1: Currahee" (normalized to "1 currahee") matches the subtitle tail.
		if _, err := strconv.Atoi(expected); err == nil {
			return !requireEvidence
		}
		if strings.HasSuffix(expected, " "+got) {
			return true
		}
	}
	return got == expected || (!requireEvidence && strings.HasPrefix(expected, got+" "))
}

// Catalogue and scene names express multipart suffixes differently:
// "Stone and Sky (2)", "Stone and Sky Part 2" and "Stone and Sky Pt02".
// Normalize the suffix without dropping its number, so different parts
// remain different episodes. Ordinary uses of the word "part" are kept.
func normalizeEpisodeTitle(title string) string {
	words := collapseEpisodeContractions(canonicalTokens(NormalizeTitle(title)))
	if len(words) == 0 {
		return ""
	}
	last := len(words) - 1
	if n, err := strconv.Atoi(words[last]); err == nil && n > 0 {
		words[last] = strconv.Itoa(n)
		if last > 0 && (words[last-1] == "part" || words[last-1] == "pt") {
			words = append(words[:last-1], words[last])
		}
	} else {
		for _, prefix := range []string{"part", "pt"} {
			if suffix, ok := strings.CutPrefix(words[last], prefix); ok {
				if n, err := strconv.Atoi(suffix); err == nil && n > 0 {
					words[last] = strconv.Itoa(n)
					break
				}
			}
		}
	}
	return strings.Join(words, " ")
}

// EpisodeSeriesYear reads only the series portion of a TV name. A year in
// an episode title or after a season/episode boundary must not be mistaken
// for the show's premiere year.
func EpisodeSeriesYear(name string, parsedYear int) int {
	spaced := spacify(normalizeSpanishCapRange(name))
	cut := -1
	consider := func(loc []int) {
		if loc != nil && loc[0] >= 0 && (cut < 0 || loc[0] < cut) {
			cut = loc[0]
		}
	}
	consider(reSE.FindStringIndex(spaced))
	consider(reSeasonAndEp.FindStringIndex(spaced))
	consider(reXEpisode.FindStringIndex(spaced))
	consider(spanishEpisodeIndexWithRaw(name, spaced))
	consider(reSeasonOnly.FindStringIndex(spaced))
	consider(reSeasonWord.FindStringIndex(spaced))
	consider(reMiniseriesPart.FindStringIndex(spaced))
	consider(reRussianSeason.FindStringIndex(spaced))
	consider(reRussianSeasonSuffix.FindStringIndex(spaced))
	consider(reRussianEpisode.FindStringIndex(spaced))
	consider(reRussianEpisodeSuffix.FindStringIndex(spaced))
	if cut < 0 {
		return parsedYear
	}
	info := core.ReleaseInfo{}
	return parseYear(spaced[:cut], &info)
}

// Scene separators can split apostrophes ("I.ll"), while catalogue titles
// retain them ("I'll"). Normalize only familiar contraction pairs; do not
// merge arbitrary words or relax episode-title identity to token overlap.
func collapseEpisodeContractions(words []string) []string {
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		if i+1 < len(words) {
			suffixes := ""
			switch words[i] {
			case "i":
				suffixes = "m ll d ve"
			case "you", "we", "they":
				suffixes = "re ll d ve"
			case "he", "she", "it", "that", "there", "what", "who", "how":
				suffixes = "s ll d"
			case "don", "doesn", "didn", "can", "won", "wouldn", "shouldn", "couldn", "isn", "aren", "wasn", "weren", "hasn", "haven", "hadn":
				suffixes = "t"
			case "let":
				suffixes = "s"
			}
			if suffixes != "" && strings.Contains(" "+suffixes+" ", " "+words[i+1]+" ") {
				out = append(out, words[i]+words[i+1])
				i++
				continue
			}
		}
		out = append(out, words[i])
	}
	return out
}

// Scene separators sometimes replace the apostrophe in a possessive:
// "Samantha s" and "Samanthas" carry the same subtitle evidence.
func joinEpisodePossessives(title string) string {
	words := strings.Fields(title)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if word == "s" && len(out) > 0 {
			out[len(out)-1] += "s"
		} else {
			out = append(out, word)
		}
	}
	return strings.Join(out, " ")
}
