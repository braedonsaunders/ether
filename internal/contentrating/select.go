package contentrating

import (
	"sort"
	"strconv"
	"strings"
)

// Certification is one rating a provider lists for a title.
type Certification struct {
	Country string // ISO 3166-1, any case
	Label   string
	// ReleaseType is TMDB's movie release type (1 premiere, 2 limited
	// theatrical, 3 theatrical, 4 digital, 5 physical, 6 TV); 0 for TV
	// show content ratings.
	ReleaseType int
}

// Selection is the chosen rating in stored form, and where it came from.
type Selection struct {
	Value  string // stored form: "PG", "de/6"; "" when nothing usable
	Source string // "tmdb:US:6", "tmdb:CA"; "" when Value is ""
}

// releasePreference orders TMDB release types: theatrical, limited, digital,
// physical, TV, premiere. A TV movie has only a type 6 release, so its US
// rating is found there; a festival premiere's rating is the last resort.
var releasePreference = map[int]int{3: 0, 2: 1, 4: 2, 5: 3, 6: 4, 1: 5, 0: 6}

// Select chooses a title's rating from a provider's certifications: the
// owner's region first, then the US, then other countries (foreignPriority
// order). Within a country it takes the first label that has an age level,
// by release type. A foreign rating is kept region-qualified ("de/6"),
// never passed off as US. When no country has a rated label, an explicit
// "not rated" from the region or the US is kept ("NR"), as Plex shows it.
func Select(certs []Certification, region string) Selection {
	region = strings.ToUpper(strings.TrimSpace(region))
	byCountry := map[string][]Certification{}
	for _, c := range certs {
		country := strings.ToUpper(strings.TrimSpace(c.Country))
		if country == "" || strings.TrimSpace(c.Label) == "" {
			continue
		}
		byCountry[country] = append(byCountry[country], c)
	}
	order := make([]string, 0, len(byCountry))
	for country := range byCountry {
		if country != region && country != "US" {
			order = append(order, country)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		pi, pj := foreignPriority(order[i]), foreignPriority(order[j])
		if pi != pj {
			return pi < pj
		}
		return order[i] < order[j]
	})
	head := []string{"US"}
	if region != "" && region != "US" {
		head = []string{region, "US"}
	}
	order = append(head, order...)
	var notRated Selection
	for _, country := range order {
		list := byCountry[country]
		sort.SliceStable(list, func(i, j int) bool { return rank(list[i].ReleaseType) < rank(list[j].ReleaseType) })
		for _, c := range list {
			label := strings.TrimSpace(c.Label)
			if Lookup(country, label) != Unrated {
				return Selection{Value: Qualify(country, label), Source: source(country, c.ReleaseType)}
			}
			if notRated.Value == "" && (country == region || country == "US") && IsNotRated(label) {
				notRated = Selection{Value: Qualify(country, label), Source: source(country, c.ReleaseType)}
			}
		}
	}
	return notRated
}

func rank(releaseType int) int {
	if r, ok := releasePreference[releaseType]; ok {
		return r
	}
	return len(releasePreference)
}

func source(country string, releaseType int) string {
	s := "tmdb:" + country
	if releaseType > 0 {
		s += ":" + strconv.Itoa(releaseType)
	}
	return s
}
