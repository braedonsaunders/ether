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

// EpisodeTitleMatches rejects an explicitly different episode title between
// SxxExx and the technical metadata. Unnamed scene releases remain eligible.
// A title is only extracted when a recognized metadata boundary follows it.
func EpisodeTitleMatches(name, want string) bool {
	if strings.TrimSpace(want) == "" {
		return true
	}
	generic := strings.Fields(strings.ToLower(want))
	if len(generic) == 2 && generic[0] == "episode" {
		if _, err := strconv.Atoi(generic[1]); err == nil {
			return true
		}
	}
	spaced := spacify(name)
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
		if loc = reMiniseriesPart.FindStringIndex(spaced); loc != nil {
			usedMiniseriesPart = true
		}
	}
	if loc == nil {
		return true
	}
	tail := strings.Trim(spaced[loc[1]:], " -")
	var words []string
	boundary := false
	for _, w := range strings.Fields(tail) {
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
		switch u {
		case "AAC", "AC3", "EAC3", "DDP", "DDP5", "DTS", "TRUEHD", "MP3", "FLAC", "OPUS",
			"READNFO", "UNCENSORED", "EXTENDED", "WS", "FS", "NTSC", "PAL",
			// Release tags that can precede the resolution; they are not
			// episode title words ("Show.S01E01.DV.2160p.WEB").
			"DV", "DOVI", "HDR10PLUS", "SDR", "HYBRID", "RERIP", "REAL", "10BIT", "DUAL", "DL",
			"AMZN", "NF", "DSNP", "HMAX", "ATVP", "PCOK", "HULU", "UNCUT", "REMASTERED", "IMAX":
			technical = true
		}
		if technical {
			boundary = true
			break
		}
		words = append(words, w)
	}
	if !boundary || len(words) == 0 {
		return true
	}
	got := normalizeEpisodeTitle(strings.Join(words, " "))
	expected := normalizeEpisodeTitle(want)
	if got == "" || got == "the" || got == "a" || got == "an" {
		return true
	}
	if usedMiniseriesPart {
		// When "Part N" was the episode marker itself ("Band.of.Brothers.Part.1.Currahee"),
		// a catalogue episode titled "Part 1" / "Part One" (normalized to "1") or
		// "Part 1: Currahee" (normalized to "1 currahee") matches the subtitle tail.
		if _, err := strconv.Atoi(expected); err == nil {
			return true
		}
		if strings.HasSuffix(expected, " "+got) {
			return true
		}
	}
	return got == expected || strings.HasPrefix(expected, got+" ")
}

// Catalogue and scene names express multipart suffixes differently:
// "Stone and Sky (2)", "Stone and Sky Part 2" and "Stone and Sky Pt02".
// Normalize the suffix without dropping its number, so different parts
// remain different episodes. Ordinary uses of the word "part" are kept.
func normalizeEpisodeTitle(title string) string {
	words := collapseEpisodeContractions(strings.Fields(NormalizeTitle(title)))
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
// an episode title must not be mistaken for the show's premiere year.
func EpisodeSeriesYear(name string, parsedYear int) int {
	spaced := spacify(name)
	loc := reSE.FindStringIndex(spaced)
	if loc == nil {
		loc = reSeasonAndEp.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reXEpisode.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reSeasonOnly.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reSeasonWord.FindStringIndex(spaced)
	}
	if loc == nil {
		loc = reMiniseriesPart.FindStringIndex(spaced)
	}
	if loc == nil {
		return parsedYear
	}
	info := core.ReleaseInfo{}
	return parseYear(spaced[:loc[0]], &info)
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
