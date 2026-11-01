package releaseparse

import (
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSceneLanguageCodesAreAudio(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"The.Dog.Stars.2026.2160p.AMZN.WEB-DL.DV.HDR10+.LAT.DDP5.1.H265.MP4-BTM", LanguageSpanishLatino},
		{"Film.2026.1080p.WEB-DL.ESP.AC3.5.1.x264-GRP", LanguageSpanishSpain},
		{"Film.2026.1080p.BluRay.POL.DD5.1.x264-GRP", "polish"},
		{"Film.2026.1080p.WEB-DL.HUN.DDP5.1.H.264-GRP", "hungarian"},
	} {
		if got := Parse(tc.name).Languages; !slices.Contains(got, tc.want) {
			t.Errorf("%s: languages %v, want %s", tc.name, got, tc.want)
		}
	}
	// NORDiC marks Nordic subtitles over the original audio.
	if got := Parse("The.Dog.Stars.2026.NORDiC.2160p.WEB-DL.H.265.DV.HDR.DDP5.1.Atmos-ADDICTION").Languages; len(got) != 0 {
		t.Errorf("NORDiC parsed as audio %v", got)
	}
	// A title word is not a language tag.
	if got := Parse("Lat.Den.2026.1080p.WEB-DL.H.264-GRP").Languages; len(got) != 0 {
		t.Errorf("title word parsed as audio %v", got)
	}
}

func TestSubtitleLanguageTagsAreNotAudio(t *testing.T) {
	for _, tc := range []struct {
		name        string
		audio, subs []string
	}{
		{"Film.2026.2160p.WEB-DL.ITA.ENG.SUBS.H265-GRP", []string{"italian"}, []string{"english"}},
		{"Film 2026 1080p WEBDL x265 10bit iTA ENG EAC3 5 1 Sub ita eng", []string{"english", "italian"}, nil},
		{"Film.2026.1080p.BluRay.x264.Subs.ENG.FRE-GRP", nil, []string{"english", "french"}},
	} {
		info := Parse(tc.name)
		slices.Sort(info.Languages)
		if !slices.Equal(info.Languages, tc.audio) {
			t.Errorf("%s: audio %v, want %v", tc.name, info.Languages, tc.audio)
		}
		for _, s := range tc.subs {
			if !slices.Contains(info.SubtitleLanguages, s) {
				t.Errorf("%s: subtitles %v, want %s", tc.name, info.SubtitleLanguages, s)
			}
		}
	}
}

// REMUX counts only as a word: "~ RemuxDoc" is an uploader on a WEB-DL.
func TestRemuxIsAWord(t *testing.T) {
	for name, want := range map[string]core.Source{
		"Spider-Man- Brand New Day (2026) IMAX 2160p 4K HDR10+ DV WEB-DL Dual Audio [Hindi + English] ESub ~ RemuxDoc": core.SourceWEBDL,
		"Film.2026.2160p.UHD.BluRay.REMUX.DV.HDR.HEVC.TrueHD-FraMeSToR":                                                core.SourceRemux,
		"It.2017.4K.HDR.DV.2160p.BDRemux Ita Eng x265-NAHOM":                                                           core.SourceRemux,
		"Blade Runner (1982) {edition-Final Cut} [Remux-2160p][HDR10][HEVC]-FraMeSToR":                                 core.SourceRemux,
	} {
		if got := Parse(name).Source; got != want {
			t.Errorf("%s: source %q, want %q", name, got, want)
		}
	}
}

func TestRunningManPackAndKoreanVarietyEpisodeParsing(t *testing.T) {
	pack := Parse("Running Man Episode 1-129 English Subbed 720P")
	if !pack.FullSeason || len(pack.Episodes) != 0 || pack.Title != "Running Man" {
		t.Fatalf("pack = %+v", pack)
	}
	inPack := Parse("Running Man Episode 1-129 English Subbed 720P [iSUBS]Running.Man.E43.720P.avi")
	if inPack.FullSeason || !slices.Equal(inPack.Episodes, []int{43}) || EpisodeMatches(inPack, 1, 1) || !EpisodeMatches(inPack, 1, 43) {
		t.Fatalf("inPack = %+v", inPack)
	}
	e129 := Parse("[iSUBS]Running.Man.E129.720P.avi")
	if !slices.Equal(e129.Episodes, []int{129}) || !EpisodeMatches(e129, 1, 129) {
		t.Fatalf("e129 = %+v", e129)
	}
	e005 := Parse("Running.Man.E005.1080p.Kcw.WEB-DL.AAC.H.264-RSG")
	if !slices.Equal(e005.Episodes, []int{5}) || !EpisodeMatches(e005, 1, 5) {
		t.Fatalf("e005 = %+v", e005)
	}
}
