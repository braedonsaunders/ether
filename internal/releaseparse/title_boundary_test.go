package releaseparse

import "testing"

func TestTitleBoundaryKeywordsAreWholeTokensBeforeYear(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		year        int
		full        bool
	}{
		{"Doctor.Strange.in.the.Multiverse.of.Madness.2022.1080p.BluRay.x264-GRP", "Doctor Strange in the Multiverse of Madness", 2022, false},
		{"The.Cameraman.1928.1080p.BluRay.x264-GRP", "The Cameraman", 1928, false},
		{"The.Four.Seasons.1981.1080p.BluRay.x264-GRP", "The Four Seasons", 1981, false},
		{"Scam.2020.1080p.WEB-DL-GRP", "Scam", 2020, false},
		{"Charlottes.Web.2006.1080p.BluRay.x264-GRP", "Charlottes Web", 2006, false},
		{"A.Complete.Unknown.2024.1080p.WEB-DL.DDP5.1.H.264-GRP", "A Complete Unknown", 2024, false},
		{"Blade.Runner.2049.2017.1080p.BluRay.x264-GRP", "Blade Runner 2049", 2017, false},
		{"Wonder.Woman.1984.2020.2160p.WEB-DL-GRP", "Wonder Woman 1984", 2020, false},
		{"1917.2019.1080p.BluRay.x264-GRP", "1917", 2019, false},
		{"Movie.2010.1080p.BluRay.x264-2010", "Movie", 2010, false},
		{"Show.S01.COMPLETE.1080p.WEB-DL-GRP", "Show", 0, true},
		{"Show.Complete.Series.1080p.BluRay-GRP", "Show", 0, true},
		{"Show.Complete.Series.2005.1080p.BluRay-GRP", "Show", 2005, true},
		{"Movie.Trilogy.2010.COMPLETE.1080p-GRP", "Movie Trilogy", 2010, true},
		{"The.Webster.S01E02.720p.HDTV.x264-GRP", "The Webster", 0, false},
		{"Friends.1994-2004.Complete.Series.1080p.BluRay-GRP", "Friends", 1994, true},
	} {
		got := Parse(tc.name)
		if got.Title != tc.title || got.Year != tc.year || got.FullSeason != tc.full {
			t.Errorf("%s: title=%q year=%d full=%v; want %q %d %v", tc.name, got.Title, got.Year, got.FullSeason, tc.title, tc.year, tc.full)
		}
	}
}

func TestEpisodeSeriesYearKeepsSpanStart(t *testing.T) {
	if got := EpisodeSeriesYear("Show (2005-2013)/Season 1/Show S01E01.mkv", 0); got != 2005 {
		t.Fatalf("got %d", got)
	}
}

func TestTitleWordsAreNotAudioLanguages(t *testing.T) {
	for name, want := range map[string][]string{
		"The.French.Dispatch.2021.1080p.WEB-DL.DDP5.1-GRP": nil,
		"The.Danish.Girl.2015.MULTI.1080p.BluRay.x264-GRP": {"multi"},
		"The.Italian.Job.2003.ITALIAN.1080p.BluRay-GRP":    {"italian"},
		"Movie.2020.FRENCH.1080p.WEB-DL-GRP":               {"french"},
		"Show.S01E01.GERMAN.720p.WEB.h264-GRP":             {"german"},
		"Movie.Title.FRENCH.1080p.BluRay-GRP":              {"french"},
		"Movie.2021.PT-BR.1080p.WEB-DL-GRP":                {"portuguese-brazil"},
	} {
		got := Parse(name).Languages
		if len(got) != len(want) {
			t.Errorf("%s: languages %v, want %v", name, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: languages %v, want %v", name, got, want)
			}
		}
	}
}

// Resolution, source and codec tags end the title even before the episode
// marker or release year; only ordinary title words are protected there.
func TestTechnicalTagsBeforeMarkerEndTitle(t *testing.T) {
	for name, want := range map[string]string{
		"Show.Name.720p.S01E01-GROUP":               "Show Name",
		"Show.Name.1080p.BluRay.x264.S01-GROUP":     "Show Name",
		"Show.Name.HDTV.x264.S01E01-GRP":            "Show Name",
		"Show.Name.PROPER.S01E01.720p.HDTV-GRP":     "Show Name",
		"Movie.Title.1080p.BluRay.x264-S1":          "Movie Title",
		"The.Spanish.Princess.S01E01.1080p.WEB-GRP": "The Spanish Princess",
		"Charlottes.Web.S01E01.720p.HDTV-GRP":       "Charlottes Web",
		"Internal.Affairs.1990.1080p.BluRay-GRP":    "Internal Affairs",
	} {
		if got := Parse(name).Title; got != want {
			t.Errorf("%s: title %q, want %q", name, got, want)
		}
	}
}
