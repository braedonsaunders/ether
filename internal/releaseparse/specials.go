package releaseparse

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	specialSE        = regexp.MustCompile(`(?i)\bS0{1,3}[ ._-]*E\d{1,4}\b`)
	specialLabel     = regexp.MustCompile(`(?i)\b(?:specials?|ovas?|oads?)\b`)
	specialTechnical = regexp.MustCompile(`(?i)\b(?:\d{3,4}[pi]|\d{3,4}x\d{3,4}|4k|uhd|blu[ -]?ray|bd(?:rip|remux)?|web(?:rip|[ -]?dl)?|hdtv|dvd(?:rip)?|remux|x26[45]|h[ .]?26[45]|hevc|avc|av1|aac|ac3|eac3|ddp|dts|flac|opus|multi|dual|custom|10bits?|hdr|sdr|dv|proper|repack|mkv|mp4|avi)\b`)
	specialTags      = regexp.MustCompile(`\[[^\]]*\]|\{[^}]*\}`)
	specialNumbers   = regexp.MustCompile(`(?i)^(?:s\d+(?:e\d+)?|\d+x\d+|e\d+|ep\d+|sp\d+|s\d+sp\d+|v\d+)$`)
)

// NamesSpecial requires an explicit special marker. Bare episode numbers and
// regular season packs do not establish season zero.
func NamesSpecial(name string) bool {
	return specialSE.MatchString(name) || specialLabel.MatchString(spacify(name))
}

// SpecialTitleWords drops filler, technical tags and the show's own names.
// Numbers are kept to distinguish named parts.
func SpecialTitleWords(title string, shows ...string) map[string]bool {
	words := func(s string) map[string]bool {
		s = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(s))
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			switch w {
			case "a", "an", "the", "of", "and", "to", "in", "on", "at", "for", "episode", "ep", "special", "specials", "ova", "ovas", "oad", "oads", "part":
				continue
			}
			if n, err := strconv.Atoi(w); err == nil {
				w = strconv.Itoa(n)
			}
			out[w] = true
		}
		return out
	}
	out := words(title)
	for _, show := range shows {
		for w := range words(show) {
			delete(out, w)
		}
	}
	return out
}

func SpecialTitleNumberConflict(a, b map[string]bool) bool {
	hasA, hasB := false, false
	for w := range a {
		if _, err := strconv.Atoi(w); err == nil {
			hasA = true
		}
	}
	for w := range b {
		if _, err := strconv.Atoi(w); err == nil {
			hasB = true
		}
	}
	if !hasA || !hasB {
		return false
	}
	for w := range a {
		if _, err := strconv.Atoi(w); err == nil && !b[w] {
			return true
		}
	}
	for w := range b {
		if _, err := strconv.Atoi(w); err == nil && !a[w] {
			return true
		}
	}
	return false
}

func specialNameWords(name string, shows []string) map[string]bool {
	name = specialTags.ReplaceAllString(name, " ")
	name = specialSE.ReplaceAllString(name, " ")
	// A filename's extension and technical suffix are not title evidence.
	if loc := specialTechnical.FindStringIndex(spacify(name)); loc != nil {
		name = spacify(name)[:loc[0]]
	}
	out := SpecialTitleWords(name, shows...)
	for w := range out {
		if specialNumbers.MatchString(w) {
			delete(out, w)
		}
	}
	return out
}

// SpecialTitleAgrees allows abbreviated subtitles after S00Exx only when at
// least half their meaningful words occur in the catalogue subtitle.
func SpecialTitleAgrees(name, want string, shows ...string) bool {
	spaced := spacify(name)
	loc := specialSE.FindStringIndex(spaced)
	if loc == nil {
		return true
	}
	tail := strings.Trim(spaced[loc[1]:], " -():")
	got := specialNameWords(tail, shows)
	expected := SpecialTitleWords(want, shows...)
	if len(got) == 0 {
		return true
	}
	if len(expected) == 0 || SpecialTitleNumberConflict(got, expected) {
		return false
	}
	shared := 0
	for w := range got {
		if expected[w] {
			shared++
		}
	}
	return shared*2 >= len(got)
}

// SpecialNamesAnother compares the basename and immediate folder, ignoring
// the show name. A stronger sibling title overrides a coincidental number.
func SpecialNamesAnother(name, want string, others, shows []string) bool {
	norm := strings.ReplaceAll(name, "\\", "/")
	words := specialNameWords(path.Base(norm), shows)
	for w := range specialNameWords(path.Base(path.Dir(norm)), shows) {
		words[w] = true
	}
	score := func(title string) float64 {
		ts := SpecialTitleWords(title, shows...)
		if len(ts) == 0 {
			return 0
		}
		shared := 0
		for w := range ts {
			if words[w] {
				shared++
			}
		}
		return float64(shared) / float64(len(ts))
	}
	wanted := score(want)
	for _, other := range others {
		if other != want {
			if s := score(other); s >= 0.5 && s > wanted {
				return true
			}
		}
	}
	return false
}

// SpecialCountOnly rejects group-local OAD/OVA counts without a title or an
// explicit S00Exx identity. A parent folder cannot confer a title on a count.
func SpecialCountOnly(name string, shows ...string) bool {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	if specialSE.MatchString(base) {
		return false
	}
	words := specialNameWords(base, shows)
	for w := range words {
		if _, err := strconv.Atoi(w); err != nil && !specialNumbers.MatchString(w) {
			return false
		}
	}
	return true
}

func SpecialMatches(name, want string, others, shows []string) bool {
	return NamesSpecial(name) && !SpecialCountOnly(name, shows...) && SpecialTitleAgrees(name, want, shows...) && !SpecialNamesAnother(name, want, others, shows)
}

// SpecialTitleEvidence is positive title evidence for a named OVA/OAD without
// catalogue episode numbering. Missing subtitles never provide evidence.
func SpecialTitleEvidence(name, want string, shows ...string) bool {
	got := specialNameWords(name, shows)
	expected := SpecialTitleWords(want, shows...)
	if len(got) == 0 || len(expected) == 0 || SpecialTitleNumberConflict(got, expected) {
		return false
	}
	shared := 0
	for w := range got {
		if expected[w] {
			shared++
		}
	}
	return shared > 0 && shared*2 >= len(got)
}

// SpecialSeriesTitle reads the actual series prefix before a specials label.
// It never supplies a query's title as evidence for an add-on's answer.
func SpecialSeriesTitle(name string) string {
	spaced := spacify(specialTags.ReplaceAllString(name, " "))
	loc := specialSE.FindStringIndex(spaced)
	if word := specialLabel.FindStringIndex(spaced); word != nil && (loc == nil || word[0] < loc[0]) {
		loc = word
	}
	if loc == nil || loc[0] == 0 {
		return ""
	}
	return Parse(strings.Trim(spaced[:loc[0]], " -():")).Title
}
