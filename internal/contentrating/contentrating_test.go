package contentrating

import (
	"reflect"
	"testing"
)

func TestUSFilmAndTVLabelsShareLevels(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   Level
	}{
		{[]string{"G", "TV-G", "TV-Y", "tv-g", " TV G "}, AllAges},
		{[]string{"TV-Y7", "TV-Y7-FV"}, Children},
		{[]string{"PG", "TV-PG", "tvpg"}, Parental},
		{[]string{"PG-13", "PG13", "TV-14"}, Teen},
		{[]string{"R", "TV-MA"}, Mature},
		{[]string{"NC-17", "NC17"}, Adult},
		{[]string{"", "NR", "Not Rated", "Unrated", "N/A"}, Unrated},
	} {
		for _, l := range tc.labels {
			if got := LevelOf(l); got != tc.want {
				t.Errorf("LevelOf(%q) = %v, want %v", l, got, tc.want)
			}
		}
	}
}

func TestForeignQualifiedLabels(t *testing.T) {
	for _, tc := range []struct {
		stored, country, label string
		want                   Level
	}{
		{"gb/U", "GB", "U", AllAges},
		{"gb/PG", "GB", "PG", Parental},
		{"gb/12A", "GB", "12A", Teen},
		{"gb/15", "GB", "15", Mature},
		{"gb/18", "GB", "18", Adult},
		{"de/0", "DE", "0", AllAges},
		{"de/6", "DE", "6", Children},
		{"de/FSK 16", "DE", "FSK 16", Mature},
		{"br/L", "BR", "L", AllAges},
		{"ca/G", "CA", "G", AllAges},
		{"ca/14A", "CA", "14A", Teen},
		{"ca/18A", "CA", "18A", Mature},
		{"ca/R", "CA", "R", Adult},
		{"au/M", "AU", "M", Teen},
		{"au/MA 15+", "AU", "MA 15+", Mature},
		{"au/MA15+", "AU", "MA15+", Mature},
		{"se/7", "SE", "7", Children},
		{"se/Btl", "SE", "Btl", AllAges},
		{"gr/Κ-12", "GR", "Κ-12", Teen}, // Greek capital kappa
		{"kr/19", "KR", "19", Adult},
		{"in/U/A 13+", "IN", "U/A 13+", Teen},
		{"pt/M/12", "PT", "M/12", Teen},
		{"ca-qc/13+", "CA-QC", "13+", Teen},
		{"xx/12", "XX", "12", Teen}, // unknown board: plain age
		{"xx/Yes", "XX", "Yes", Unrated},
		{"au/RC", "AU", "RC", Unrated},
	} {
		got := Parse(tc.stored)
		want := Rating{Country: tc.country, Label: tc.label, Level: tc.want}
		if got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.stored, got, want)
		}
	}
}

// Bare labels written by older releases (any country's label stored as if
// it were US) resolve only when every board agrees, or as a plain age.
func TestBareForeignLabels(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  Level
	}{
		{"L", AllAges},      // Brazil only
		{"TP", AllAges},     // France and Spain agree
		{"Btl", AllAges},    // Sweden
		{"0", AllAges},      // plain age
		{"6", Children},     // plain age
		{"7", Children},     // plain age
		{"12", Teen},        // plain age
		{"16", Mature},      // plain age
		{"+16", Mature},     // plain age
		{"18+", Adult},      // plain age
		{"MA 15+", Mature},  // Australia
		{"12A", Teen},       // GB and IE agree
		{"14A", Teen},       // Canada
		{"U/A 13+", Teen},   // India
		{"A", Unrated},      // all ages in Spain, adults in India
		{"C", Unrated},      // children in Canada, adults in Argentina
		{"U/A 16+", Mature}, // India
		{"IMDB", Unrated},   // junk
		{"전체관람가", AllAges},  // Korea, Hangul
	} {
		if got := LevelOf(tc.label); got != tc.want {
			t.Errorf("LevelOf(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
	if r := Parse("L"); r.Country != "" {
		t.Errorf("a bare foreign label must not claim a country: %+v", r)
	}
	if r := Parse("PG"); r.Country != "US" {
		t.Errorf("a bare US label is US: %+v", r)
	}
}

func TestPlainLabelsWithSlashesAreNotQualified(t *testing.T) {
	for _, s := range []string{"U/A 13+", "M/12", "A/i"} {
		if _, _, ok := splitQualified(s); ok {
			t.Errorf("%q parsed as region-qualified", s)
		}
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"G": "G", "TV-G": "G", "TV-Y": "G", "gb/U": "G", "de/0": "G",
		"TV-Y7": "TV-Y7", "de/6": "TV-Y7",
		"PG": "PG", "TV-PG": "PG", "gb/PG": "PG",
		"PG-13": "PG-13", "TV-14": "PG-13", "gb/12A": "PG-13",
		"R": "R", "TV-MA": "R", "gb/15": "R",
		"NC-17": "NC-17", "gb/18": "NC-17",
		"NR": "NR", "Not rated": "NR", "Unrated": "NR",
	} {
		if got, ok := Canonical(in); !ok || got != want {
			t.Errorf("Canonical(%q) = %q,%v, want %q", in, got, ok, want)
		}
	}
	if _, ok := Canonical("IMDB"); ok {
		t.Error("an unknown label has no canonical key")
	}
	for _, l := range Levels {
		if got, _ := Canonical(l.Key()); got != l.Key() {
			t.Errorf("level key %q is not canonical (%q)", l.Key(), got)
		}
	}
}

func TestExceedsComparesLevels(t *testing.T) {
	for _, tc := range []struct {
		rating, max string
		want        bool
	}{
		{"TV-PG", "PG", false},
		{"PG", "TV-PG", false},
		{"G", "TV-PG", false},
		{"TV-Y7", "PG", false},
		{"de/6", "PG", false},
		{"TV-14", "PG-13", false},
		{"PG-13", "TV-14", false},
		{"gb/15", "PG-13", true},
		{"TV-MA", "R", false},
		{"R", "TV-14", true},
		{"", "PG", true},   // unrated never fits a limit
		{"NR", "PG", true}, // explicit not rated likewise
		{"R", "", false},   // no limit
		{"G", "bogus", true},
	} {
		if got := Exceeds(tc.rating, tc.max); got != tc.want {
			t.Errorf("Exceeds(%q, %q) = %v, want %v", tc.rating, tc.max, got, tc.want)
		}
	}
}

func TestEveryTableLabelResolvesInItsCountry(t *testing.T) {
	for country, entries := range tables {
		for _, en := range entries {
			if got := Lookup(country, en.label); got != en.level {
				t.Errorf("%s %q = %v, want %v (normalised duplicates disagree?)", country, en.label, got, en.level)
			}
			if got := Parse(Qualify(country, en.label)); got.Level != en.level {
				t.Errorf("Parse(Qualify(%s, %q)) = %+v", country, en.label, got)
			}
		}
	}
}

func TestEquivalentsGroupByLevel(t *testing.T) {
	got := Equivalents(Parental)
	if !reflect.DeepEqual(got[:2], []string{"PG", "TV-PG"}) {
		t.Fatalf("US labels first: %v", got)
	}
	want := map[string]bool{"ca/PG": true, "gb/PG": true, "au/PG": true, "br/10": true, "se/11": true}
	for _, v := range got {
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("missing equivalents %v in %v", want, got)
	}
}

func TestPrefixedRoundTrip(t *testing.T) {
	for stored, prefixed := range map[string]string{
		"PG": "PG", "TV-14": "TV-14", "de/6": "DE-6", "gb/12A": "GB-12A", "ca-qc/13+": "CA-QC-13+", "in/U/A 13+": "IN-U/A 13+", "": "",
	} {
		if got := Prefixed(stored); got != prefixed {
			t.Errorf("Prefixed(%q) = %q, want %q", stored, got, prefixed)
		}
		if got := FromPrefixed(prefixed); got != stored {
			t.Errorf("FromPrefixed(%q) = %q, want %q", prefixed, got, stored)
		}
	}
	// US-style labels whose first part looks like a country code stay as
	// they are: TV, PG and NC are not boards Ether knows.
	for _, l := range []string{"TV-PG", "PG-13", "NC-17", "TV-Y7-FV"} {
		if got := FromPrefixed(l); got != l {
			t.Errorf("FromPrefixed(%q) = %q", l, got)
		}
	}
}
