package releaseparse

import (
	"slices"
	"testing"
)

func TestParseSeasonWithEpWordAndRepeatedSE(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seasons []int
		eps     []int
		full    bool
	}{
		{"Show.S01.EP05.HDTV", []int{1}, []int{5}, false},
		{"Show.S02.Ep.05.1080p.WEB-DL", []int{2}, []int{5}, false},
		{"Show.S02.Episode.05.1080p", []int{2}, []int{5}, false},
		{"Show.S02.EP05-EP06.1080p", []int{2}, []int{5, 6}, false},
		{"Show.S01E05.S01E06.1080p", []int{1}, []int{5, 6}, false},
		{"Show.S01E05.Episode.Title.S02.1080p", []int{1}, []int{5}, false},
		{"Show.S01.Complete.1080p", []int{1}, nil, true},
	} {
		got := Parse(tc.name)
		if !slices.Equal(got.Seasons, tc.seasons) || !slices.Equal(got.Episodes, tc.eps) || got.FullSeason != tc.full {
			t.Errorf("Parse(%q) = seasons:%v eps:%v full:%v, want seasons:%v eps:%v full:%v",
				tc.name, got.Seasons, got.Episodes, got.FullSeason, tc.seasons, tc.eps, tc.full)
		}
	}
}
