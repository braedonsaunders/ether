package contentrating

// The rating systems. Labels are the ones TMDB lists for each country
// (GET /certification/movie/list and /certification/tv/list), plus the
// spellings its contributors and OMDb use; levels follow each board's own
// age definition, banded by AgeLevel. Judgment calls, where a label's age
// and its usual equivalent disagree:
//
//   - US R and TV-MA ("under 17 with a parent", "17+") are Mature, with
//     GB 15 and DE 16; NC-17 is Adult, with GB 18 and DE 18.
//   - US TV-14 sits with PG-13 (Teen), not above it, as Plex and Emby
//     treat it in practice; TV-Y7 is Children, below PG.
//   - Accompanied adult ratings count as Mature, not Adult: CA 18A, IE 15A,
//     NZ RP16. CA R and CA TV 18+ are Adult.
//   - Advisory ratings without a legal age use their stated advice: AU M
//     and NZ M are Teen, AU MA15+ is Mature, JP PG12 and GB PG are
//     Parental.
//   - "Approved" and "Passed" (pre-1968 US) are All ages, as Jellyfin's US
//     table scores them.
//   - Refused or banned classifications (AU RC, FI KK, KR Restricted
//     Screening, ZA XX, TH Banned) and exempt/unrated marks are Unrated.
//
// Jellyfin's per-country files were used as a cross-check only; nothing is
// copied from them (see THIRD_PARTY_NOTICES.md).

type entry struct {
	label string
	level Level
}

func e(level Level, labels ...string) []entry {
	out := make([]entry, len(labels))
	for i, l := range labels {
		out[i] = entry{l, level}
	}
	return out
}

func system(groups ...[]entry) []entry {
	var out []entry
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

const (
	u = Unrated
	a = AllAges
	c = Children
	p = Parental
	t = Teen
	m = Mature
	x = Adult
)

var tables = map[string][]entry{
	"US": system(
		e(u, "NR", "Not Rated", "Unrated", "N/A"),
		e(a, "G", "TV-G", "TV-Y", "Approved", "Passed"),
		e(c, "TV-Y7", "TV-Y7-FV"),
		e(p, "PG", "TV-PG", "GP"),
		e(t, "PG-13", "TV-14"),
		e(m, "R", "TV-MA"),
		e(x, "NC-17", "X", "TV-X", "AO"),
	),
	"AR": system(e(a, "ATP"), e(t, "+13", "SAM 13"), e(m, "+16", "SAM 16"), e(x, "+18", "SAM 18", "C")),
	"AT": system(e(a, "AA", "0"), e(c, "6", "8"), e(p, "10"), e(t, "12", "14"), e(m, "16"), e(x, "18")),
	"AU": system(
		e(u, "RC", "E", "Exempt", "CTC"),
		e(a, "G", "P", "C"),
		e(p, "PG"),
		e(t, "M"),
		e(m, "MA 15+", "MA", "AV 15+"),
		e(x, "R 18+", "X 18+"),
	),
	"BE": system(e(a, "AL", "KT", "KT/EA", "EA"), e(c, "6"), e(p, "9"), e(t, "12", "14"), e(m, "16", "KNT", "ENA", "KNT/ENA"), e(x, "18")),
	"BG": system(e(u, "Unrated"), e(a, "A", "B"), e(t, "C", "12", "14"), e(m, "D", "16"), e(x, "X", "18")),
	"BR": system(e(a, "L", "Livre", "AL", "ER"), e(p, "10"), e(t, "12", "14"), e(m, "16"), e(x, "18")),
	"CA": system(
		e(u, "E", "Exempt"),
		e(a, "G", "C"),
		e(c, "C8"),
		e(p, "PG"),
		e(t, "14A", "14+"),
		e(m, "18A"),
		e(x, "R", "A", "18+"),
	),
	"CA-QC": system(e(u, "NR"), e(a, "G"), e(c, "8+"), e(t, "13+"), e(m, "16+"), e(x, "18+")),
	"CH":    system(e(a, "0"), e(c, "6", "8"), e(p, "10"), e(t, "12", "14"), e(m, "16"), e(x, "18")),
	"CL":    system(e(u, "UR"), e(a, "TE", "I", "F"), e(c, "TE+7", "I7"), e(p, "I10"), e(t, "14", "I12"), e(m, "R"), e(x, "18", "A")),
	"CZ":    system(e(u, "UR"), e(a, "U", "0+", "5+"), e(c, "7+"), e(p, "9+"), e(t, "12+"), e(m, "15+"), e(x, "18+")),
	"DE":    system(e(a, "0", "FSK 0"), e(c, "6", "FSK 6"), e(t, "12", "FSK 12"), e(m, "16", "FSK 16"), e(x, "18", "FSK 18")),
	"DK":    system(e(u, "NR"), e(a, "A"), e(c, "7", "F"), e(p, "11"), e(m, "15"), e(x, "18")),
	"ES": system(
		e(u, "NR"),
		e(a, "A", "Ai", "APTA", "ERI", "TP"),
		e(c, "7", "7i"),
		e(p, "10"),
		e(t, "12", "13"),
		e(m, "16"),
		e(x, "18", "X"),
	),
	"FI": system(e(u, "KK"), e(a, "S"), e(c, "K-7", "7"), e(t, "K-12", "12"), e(m, "K-16", "16"), e(x, "K-18", "18")),
	"FR": system(e(u, "NR"), e(a, "TP", "U"), e(p, "10"), e(t, "12"), e(m, "16"), e(x, "18")),
	"GB": system(e(a, "U", "Uc"), e(p, "PG"), e(t, "12A", "12"), e(m, "15"), e(x, "18", "R18")),
	"GR": system(e(a, "K"), e(c, "8"), e(t, "K-12", "12"), e(m, "K-15", "16"), e(x, "K-18", "18")),
	"HK": system(e(a, "I"), e(p, "IIA"), e(t, "IIB"), e(x, "III")),
	"HU": system(e(u, "NR", "Unrated"), e(a, "KN", "Children"), e(c, "6"), e(t, "12"), e(m, "16"), e(x, "18", "X")),
	"ID": system(e(a, "SU", "P"), e(c, "A"), e(t, "13+", "R"), e(m, "17+"), e(x, "21+", "D")),
	"IE": system(e(a, "G"), e(p, "PG"), e(t, "12A", "12"), e(m, "15A", "15", "16"), e(x, "18")),
	"IL": system(e(u, "E"), e(a, "All", "G"), e(t, "12", "14", "12+"), e(m, "16", "15+"), e(x, "18", "18+")),
	"IN": system(e(u, "S"), e(a, "U"), e(c, "U/A 7+"), e(p, "UA"), e(t, "U/A 13+"), e(m, "U/A 16+"), e(x, "A")),
	"IT": system(e(u, "NR"), e(a, "T"), e(c, "6+", "VM6"), e(p, "10+", "BA"), e(t, "14+", "VM12", "VM14"), e(x, "18+", "VM18")),
	"JP": system(e(a, "G"), e(p, "PG12"), e(m, "R15+"), e(x, "R18+")),
	"KR": system(
		e(u, "Exempt", "Restricted Screening"),
		e(a, "ALL", "전체관람가"),
		e(c, "7"),
		e(t, "12", "12세 이상 관람가"),
		e(m, "15", "15세 이상 관람가"),
		e(x, "19", "18", "청소년 관람불가"),
	),
	"LT": system(e(u, "NR"), e(a, "V"), e(c, "N-7"), e(t, "N-13", "N-14"), e(m, "N-16"), e(x, "N-18", "S")),
	"LU": system(e(a, "A", "Tous"), e(c, "6"), e(p, "10"), e(t, "12"), e(m, "16"), e(x, "18")),
	"LV": system(e(a, "U"), e(c, "7+"), e(t, "12+"), e(m, "16+"), e(x, "18+")),
	"MA": system(e(u, "NR"), e(p, "10"), e(t, "12"), e(m, "16")),
	"MO": system(e(a, "A"), e(t, "B"), e(x, "C", "D")),
	"MX": system(e(a, "AA", "A"), e(t, "B"), e(m, "B-15"), e(x, "C", "D")),
	"MY": system(e(u, "NR"), e(a, "U"), e(t, "P13"), e(x, "18", "18SG", "18SX", "18PA", "18PL")),
	"NL": system(e(u, "NR"), e(a, "AL"), e(c, "6"), e(p, "9"), e(t, "12", "14"), e(m, "16"), e(x, "18")),
	"NO": system(e(u, "NR"), e(a, "A"), e(c, "6"), e(p, "9"), e(t, "12"), e(m, "15"), e(x, "18")),
	"NZ": system(
		e(a, "G"),
		e(p, "PG"),
		e(t, "M", "R13", "RP13"),
		e(m, "R15", "R16", "RP16", "16"),
		e(x, "R18", "RP18", "R", "18"),
	),
	"PH": system(e(u, "NR", "X"), e(a, "G"), e(p, "PG"), e(t, "R-13", "SPG"), e(m, "R-16"), e(x, "R-18")),
	"PL": system(e(a, "AL", "0"), e(c, "7"), e(t, "12"), e(m, "15", "16"), e(x, "18")),
	"PR": system(
		e(u, "NR"),
		e(a, "G", "TV-G", "TV-Y"),
		e(c, "TV-Y7"),
		e(p, "PG", "TV-PG"),
		e(t, "PG-13", "TV-14"),
		e(m, "R", "TV-MA"),
		e(x, "NC-17"),
	),
	"PT": system(e(u, "NR"), e(a, "Públicos", "M/3", "T"), e(c, "M/6"), e(p, "10"), e(t, "M/12", "M/14", "12"), e(m, "M/16", "16"), e(x, "M/18", "P")),
	"RO": system(e(u, "Unrated", "IC"), e(a, "AG"), e(p, "AP", "AP-12"), e(t, "12"), e(m, "15", "N-15"), e(x, "18", "18+", "IM-18")),
	"RU": system(e(u, "NR"), e(a, "0+"), e(c, "6+"), e(t, "12+"), e(m, "16+"), e(x, "18+")),
	"SE": system(e(u, "NR"), e(a, "Btl"), e(c, "7", "Från 7 år"), e(p, "11", "Från 11 år"), e(m, "15", "Från 15 år")),
	"SG": system(e(a, "G"), e(p, "PG"), e(t, "PG13"), e(m, "NC16"), e(x, "M18", "R21")),
	"SK": system(e(u, "NR"), e(a, "U"), e(c, "7"), e(t, "12"), e(m, "15"), e(x, "18")),
	"TH": system(
		e(u, "Banned", "-"),
		e(a, "P", "G", "ส", "ท"),
		e(t, "13", "น 13+"),
		e(m, "15", "น 15+"),
		e(x, "18", "20", "น 18+", "ฉ 20-"),
	),
	"TR": system(
		e(a, "Genel İzleyici Kitlesi", "Genel İzleyici"),
		e(c, "6A", "6+", "7+"),
		e(p, "10A", "10+"),
		e(t, "13A", "13+"),
		e(m, "16+"),
		e(x, "18+"),
	),
	"TW": system(e(a, "0+"), e(c, "6+"), e(t, "12+"), e(m, "15+"), e(x, "18+")),
	"UA": system(e(u, "UR"), e(a, "0+"), e(p, "9+"), e(t, "12+"), e(m, "16+"), e(x, "18+")),
	"VI": system(
		e(u, "NR"),
		e(a, "G", "TV-G", "TV-Y"),
		e(c, "TV-Y7"),
		e(p, "PG", "TV-PG"),
		e(t, "PG-13", "TV-14"),
		e(m, "R", "TV-MA"),
		e(x, "NC-17"),
	),
	"ZA": system(e(u, "XX"), e(a, "A", "All"), e(c, "7-9PG"), e(p, "PG", "10-12PG"), e(t, "13"), e(m, "16"), e(x, "18", "X18")),
}

// index is tables keyed by normalised label, built once.
var index = func() map[string]map[string]Level {
	out := make(map[string]map[string]Level, len(tables))
	for country, entries := range tables {
		idx := make(map[string]Level, len(entries))
		for _, en := range entries {
			idx[norm(en.label)] = en.level
		}
		out[country] = idx
	}
	return out
}()

func lookup(country, label string) (Level, bool) {
	idx, ok := index[country]
	if !ok {
		return Unrated, false
	}
	l, ok := idx[norm(label)]
	return l, ok
}

// foreignPriority orders other countries when the owner's region and the US
// have no rating: English-language boards first, then the large European,
// Latin American and Asian boards, then the rest alphabetically.
func foreignPriority(country string) int {
	for i, c := range foreignOrder {
		if c == country {
			return i
		}
	}
	return len(foreignOrder)
}

var foreignOrder = []string{"CA", "GB", "AU", "IE", "NZ", "DE", "FR", "NL", "ES", "IT", "SE", "DK", "NO", "FI", "BR", "MX", "JP", "KR"}
