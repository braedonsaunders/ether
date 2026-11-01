package contentrating

import "testing"

func TestSelect(t *testing.T) {
	type C = Certification
	for _, tc := range []struct {
		name   string
		region string
		certs  []C
		want   Selection
	}{
		{
			// A Hallmark TV movie: TMDB's only US release is the TV
			// premiere (type 6), certified with the MPAA label.
			name:  "hallmark TV movie",
			certs: []C{{"BR", "L", 6}, {"DE", "0", 6}, {"US", "G", 6}},
			want:  Selection{"G", "tmdb:US:6"},
		},
		{
			name:  "theatrical before digital and TV",
			certs: []C{{"US", "TV-PG", 6}, {"US", "PG-13", 4}, {"US", "R", 3}},
			want:  Selection{"R", "tmdb:US:3"},
		},
		{
			name:  "blank theatrical falls through to the TV release",
			certs: []C{{"US", "", 3}, {"US", "PG", 6}},
			want:  Selection{"PG", "tmdb:US:6"},
		},
		{
			name:  "festival premiere is the last US resort",
			certs: []C{{"US", "R", 1}, {"US", "", 3}},
			want:  Selection{"R", "tmdb:US:1"},
		},
		{
			// Write Before Christmas: US blank; Canada, Brazil and Germany
			// rate it. Canada comes first and stays qualified.
			name:  "foreign only stays region-qualified",
			certs: []C{{"BR", "L", 6}, {"CA", "G", 6}, {"DE", "6", 6}, {"US", "", 6}},
			want:  Selection{"ca/G", "tmdb:CA:6"},
		},
		{
			name:  "unknown foreign junk is skipped",
			certs: []C{{"XX", "Yes", 3}, {"SE", "7", 3}},
			want:  Selection{"se/7", "tmdb:SE:3"},
		},
		{
			name:   "owner region first",
			region: "GB",
			certs:  []C{{"US", "PG-13", 3}, {"GB", "12A", 3}},
			want:   Selection{"gb/12A", "tmdb:GB:3"},
		},
		{
			name:   "owner region, then US",
			region: "gb",
			certs:  []C{{"DE", "12", 3}, {"US", "PG-13", 3}},
			want:   Selection{"PG-13", "tmdb:US:3"},
		},
		{
			name:  "explicit US not rated yields to a rated country",
			certs: []C{{"US", "NR", 6}, {"NL", "16", 3}},
			want:  Selection{"nl/16", "tmdb:NL:3"},
		},
		{
			name:  "explicit US not rated is kept when nothing is rated",
			certs: []C{{"US", "NR", 6}},
			want:  Selection{"NR", "tmdb:US:6"},
		},
		{
			name:  "nothing at all",
			certs: []C{{"US", "", 6}},
			want:  Selection{},
		},
		{
			name:  "TV show content ratings",
			certs: []C{{"DE", "12", 0}, {"US", "TV-PG", 0}},
			want:  Selection{"TV-PG", "tmdb:US"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Select(tc.certs, tc.region); got != tc.want {
				t.Errorf("Select = %+v, want %+v", got, tc.want)
			}
		})
	}
}
