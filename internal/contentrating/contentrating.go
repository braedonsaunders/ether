// Package contentrating is Ether's one content-rating model. Every
// certification (US MPAA and TV Parental Guidelines, and the boards of the
// countries TMDB lists) normalises to a Rating: the issuing country, the
// label as certified, and an age Level. Rules, viewer limits and pickers
// compare Levels, so "PG" and "TV-PG" are the same choice everywhere.
//
// Stored form. US labels are stored bare ("PG", "TV-14"), exactly as Plex
// shows them. Any other country's label is stored region-qualified the way
// Plex Media Server writes it, lowercase country then a slash: "de/6",
// "gb/12A", "ca/14A". A foreign label is never stored as if it were US.
package contentrating

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Level is an age band. Higher is more mature; Unrated sorts first and is
// never inside a limit. The integer values are persisted (catalogue
// content_rating_rank), so they must not be renumbered.
type Level int

const (
	Unrated  Level = iota // no usable certification: blank, NR, Unrated, unknown label
	AllAges               // G, TV-G, TV-Y; U (GB), 0 (DE), L (BR)
	Children              // TV-Y7; 6 (DE), 7 (SE)
	Parental              // PG, TV-PG; PG (GB), 10 (BR)
	Teen                  // PG-13, TV-14; 12A (GB), 12 (DE), 14A (CA)
	Mature                // R, TV-MA; 15 (GB), 16 (DE), MA15+ (AU)
	Adult                 // NC-17; 18 (GB, DE), R18+ (AU)
)

// Levels lists the rated levels, mildest first.
var Levels = []Level{AllAges, Children, Parental, Teen, Mature, Adult}

// Age is the minimum viewer age the level stands for (-1 when unrated).
// It is also the parental score other protocols use (Jellyfin and Emby
// compare these numbers; Jellyfin's US file scores G 0, TV-Y7 7, PG 10,
// PG-13 13).
func (l Level) Age() int {
	switch l {
	case AllAges:
		return 0
	case Children:
		return 7
	case Parental:
		return 10
	case Teen:
		return 13
	case Mature:
		return 16
	case Adult:
		return 18
	}
	return -1
}

// Key is the canonical value a rule or viewer limit stores for the level:
// the US film label where there is one. "NR" is the explicit "Not rated"
// choice.
func (l Level) Key() string {
	switch l {
	case AllAges:
		return "G"
	case Children:
		return "TV-Y7"
	case Parental:
		return "PG"
	case Teen:
		return "PG-13"
	case Mature:
		return "R"
	case Adult:
		return "NC-17"
	}
	return NotRatedKey
}

// NotRatedKey is the rule and picker value for titles without a usable
// rating.
const NotRatedKey = "NR"

// USLabels are the US film and TV labels of a level, in display order.
func (l Level) USLabels() []string {
	switch l {
	case AllAges:
		return []string{"G", "TV-G", "TV-Y"}
	case Children:
		return []string{"TV-Y7", "TV-Y7-FV"}
	case Parental:
		return []string{"PG", "TV-PG"}
	case Teen:
		return []string{"PG-13", "TV-14"}
	case Mature:
		return []string{"R", "TV-MA"}
	case Adult:
		return []string{"NC-17"}
	}
	return []string{"NR"}
}

// Rating is one parsed certification.
type Rating struct {
	// Country is the ISO 3166-1 code of the issuing board, upper case
	// ("US", "DE", "CA-QC"). Empty when a bare stored label is not a US
	// label and its country cannot be known.
	Country string
	Label   string // as certified: "TV-PG", "6", "12A"
	Level   Level
}

// Rated reports whether the rating has an age level.
func (r Rating) Rated() bool { return r.Level != Unrated }

// Qualify is the stored form of a country's label: bare for the US,
// otherwise "cc/label" like Plex Media Server.
func Qualify(country, label string) string {
	label = strings.TrimSpace(label)
	country = strings.ToUpper(strings.TrimSpace(country))
	if label == "" || country == "" || country == "US" {
		return label
	}
	return strings.ToLower(country) + "/" + label
}

// Parse reads a stored or owner-entered rating. Bare US labels resolve as
// US. A bare label from another system (stored by an older release, or
// typed by the owner) resolves only when every board that uses it agrees on
// the level, or when it is a plain age ("12", "16+"); anything else is
// Unrated rather than a guess.
func Parse(s string) Rating {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rating{}
	}
	if country, label, ok := splitQualified(s); ok {
		return Rating{Country: country, Label: label, Level: Lookup(country, label)}
	}
	if l, ok := lookup("US", s); ok {
		return Rating{Country: "US", Label: s, Level: l}
	}
	return Rating{Label: s, Level: anyCountry(s)}
}

// LevelOf is Parse(s).Level.
func LevelOf(s string) Level { return Parse(s).Level }

// Lookup is the level of a country's label. A label the country's table
// does not list falls back to a plain age ("16", "12+"); otherwise it is
// Unrated.
func Lookup(country, label string) Level {
	if l, ok := lookup(strings.ToUpper(strings.TrimSpace(country)), label); ok {
		return l
	}
	return ageLevel(label)
}

// Canonical maps any accepted rule or limit value to the level's Key: "TV-G"
// and "gb/U" become "G", "Not rated" becomes "NR". ok is false for a value
// that names no level and is not an explicit "not rated".
func Canonical(v string) (key string, ok bool) {
	if IsNotRated(v) {
		return NotRatedKey, true
	}
	if l := LevelOf(v); l != Unrated {
		return l.Key(), true
	}
	return "", false
}

// IsNotRated reports whether an owner value means "titles without a rating".
func IsNotRated(v string) bool {
	switch norm(v) {
	case "NR", "NOTRATED", "UNRATED":
		return true
	}
	return false
}

// Exceeds reports whether rating is above the limit max. An empty limit is
// unrestricted. An unrated title, or a limit that names no level, exceeds
// every limit: restricted viewers never see what Ether cannot place.
func Exceeds(rating, max string) bool {
	if strings.TrimSpace(max) == "" {
		return false
	}
	m := LevelOf(max)
	if m == Unrated {
		return true
	}
	r := LevelOf(rating)
	return r == Unrated || r > m
}

// Allowed is !Exceeds, for readability at call sites.
func Allowed(rating, max string) bool { return !Exceeds(rating, max) }

// Equivalents lists the labels each country's board uses for level l, as
// stored values (bare US, "cc/label" elsewhere), US first then by country.
func Equivalents(l Level) []string {
	out := append([]string(nil), l.USLabels()...)
	if l == Unrated {
		return out
	}
	countries := make([]string, 0, len(tables))
	for c := range tables {
		if c != "US" {
			countries = append(countries, c)
		}
	}
	sort.Slice(countries, func(i, j int) bool {
		pi, pj := foreignPriority(countries[i]), foreignPriority(countries[j])
		if pi != pj {
			return pi < pj
		}
		return countries[i] < countries[j]
	})
	for _, c := range countries {
		for _, e := range tables[c] {
			if e.level == l {
				out = append(out, Qualify(c, e.label))
			}
		}
	}
	return out
}

// splitQualified recognises "cc/label" and "cc-xx/label" (Plex's stored
// form). Labels that merely contain a slash ("U/A 13+", "M/12") are not
// qualified: their prefix is not a country code.
func splitQualified(s string) (country, label string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	prefix := s[:i]
	if len(prefix) != 2 && !(len(prefix) == 5 && prefix[2] == '-') {
		return "", "", false
	}
	for j, r := range prefix {
		if j == 2 && r == '-' {
			continue
		}
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return "", "", false
		}
	}
	return strings.ToUpper(prefix), strings.TrimSpace(s[i+1:]), true
}

// anyCountry resolves a bare label used outside the US: the level every
// board listing it agrees on, else its plain age, else Unrated.
func anyCountry(label string) Level {
	n := norm(label)
	if n == "" {
		return Unrated
	}
	found, agreed := false, Unrated
	for _, idx := range index {
		l, ok := idx[n]
		if !ok {
			continue
		}
		if found && l != agreed {
			return ageLevel(label)
		}
		found, agreed = true, l
	}
	if found && agreed != Unrated {
		return agreed
	}
	return ageLevel(label)
}

// ageLevel reads a plain age certification: digits with optional "+" and
// spaces ("12", "16+", "+13", "18 ans" is not plain). It maps the age to the
// band whose range holds it.
func ageLevel(label string) Level {
	s := strings.TrimSpace(label)
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimSuffix(s, "+")
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 21 {
		return Unrated
	}
	return AgeLevel(n)
}

// AgeLevel is the band holding a minimum age: 0–5 all ages, 6–8 children,
// 9–11 parental guidance, 12–14 teens, 15–17 mature, 18 and over adults.
func AgeLevel(age int) Level {
	switch {
	case age < 0:
		return Unrated
	case age <= 5:
		return AllAges
	case age <= 8:
		return Children
	case age <= 11:
		return Parental
	case age <= 14:
		return Teen
	case age <= 17:
		return Mature
	}
	return Adult
}

// norm folds a label for table lookups: upper case, look-alike Greek and
// Cyrillic capitals to Latin, and spaces, hyphens, dots and plus signs
// dropped, so "MA 15+", "MA15+" and "MA15" match, and "Κ-12" (Greek) is
// "K12". Slashes stay ("U/A13", "M/12").
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch r {
		case ' ', ' ', '-', '_', '.', '+', '‐', '‑', '–':
			continue
		}
		if l, ok := lookalike[r]; ok {
			r = l
		}
		b.WriteRune(r)
	}
	return b.String()
}

var lookalike = map[rune]rune{
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T', 'Χ': 'X',
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X',
}

// Prefixed is a stored rating in the form Jellyfin and Emby store and show
// ratings: bare for the US, "DE-6" (country, hyphen, label) elsewhere.
func Prefixed(stored string) string {
	if country, label, ok := splitQualified(strings.TrimSpace(stored)); ok {
		return country + "-" + label
	}
	return stored
}

// FromPrefixed reads a Jellyfin or Emby rating label back into stored form:
// "DE-6" is "de/6" when DE is a board Ether knows; anything else ("PG-13",
// "TV-14") is already a stored label.
func FromPrefixed(label string) string {
	label = strings.TrimSpace(label)
	for _, n := range []int{5, 2} { // "CA-QC-13+" before "CA-…"
		if len(label) > n+1 && label[n] == '-' {
			if _, known := tables[strings.ToUpper(label[:n])]; known && strings.ToUpper(label[:n]) != "US" {
				return Qualify(label[:n], label[n+1:])
			}
		}
	}
	return label
}
