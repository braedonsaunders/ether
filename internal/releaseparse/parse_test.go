package releaseparse

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type expect struct {
	title    string
	year     int
	height   int
	source   core.Source
	codec    string
	audio    string
	channels string
	group    string
	seasons  []int
	episodes []int
	full     bool
	hdr      bool
	dv       bool
	atmos    bool
	proper   bool
	repack   bool
	lang     string
	cont     string
}

func check(t *testing.T, name string, e expect) {
	t.Helper()
	got := Parse(name)
	if e.title != "" && got.Title != e.title {
		t.Errorf("%q title = %q, want %q", name, got.Title, e.title)
	}
	if e.year != 0 && got.Year != e.year {
		t.Errorf("%q year = %d, want %d", name, got.Year, e.year)
	}
	if e.height != 0 && got.Height != e.height {
		t.Errorf("%q height = %d, want %d", name, got.Height, e.height)
	}
	if e.source != "" && got.Source != e.source {
		t.Errorf("%q source = %q, want %q", name, got.Source, e.source)
	}
	if e.codec != "" && got.VideoCodec != e.codec {
		t.Errorf("%q codec = %q, want %q", name, got.VideoCodec, e.codec)
	}
	if e.audio != "" && got.AudioCodec != e.audio {
		t.Errorf("%q audio = %q, want %q", name, got.AudioCodec, e.audio)
	}
	if e.channels != "" && got.Channels != e.channels {
		t.Errorf("%q channels = %q, want %q", name, got.Channels, e.channels)
	}
	if e.group != "" && got.Group != e.group {
		t.Errorf("%q group = %q, want %q", name, got.Group, e.group)
	}
	if e.seasons != nil && !reflect.DeepEqual(got.Seasons, e.seasons) {
		t.Errorf("%q seasons = %v, want %v", name, got.Seasons, e.seasons)
	}
	if e.episodes != nil && !reflect.DeepEqual(got.Episodes, e.episodes) {
		t.Errorf("%q episodes = %v, want %v", name, got.Episodes, e.episodes)
	}
	if got.FullSeason != e.full {
		t.Errorf("%q fullSeason = %v, want %v", name, got.FullSeason, e.full)
	}
	if got.HDR10 != e.hdr {
		t.Errorf("%q hdr = %v, want %v", name, got.HDR10, e.hdr)
	}
	if got.DolbyVision != e.dv {
		t.Errorf("%q dv = %v, want %v", name, got.DolbyVision, e.dv)
	}
	if got.Atmos != e.atmos {
		t.Errorf("%q atmos = %v, want %v", name, got.Atmos, e.atmos)
	}
	if got.Proper != e.proper {
		t.Errorf("%q proper = %v, want %v", name, got.Proper, e.proper)
	}
	if got.Repack != e.repack {
		t.Errorf("%q repack = %v, want %v", name, got.Repack, e.repack)
	}
	if e.lang != "" {
		found := false
		for _, l := range got.Languages {
			if l == e.lang {
				found = true
			}
		}
		if !found {
			t.Errorf("%q languages = %v, want %q", name, got.Languages, e.lang)
		}
	}
	if e.cont != "" && got.Container != e.cont {
		t.Errorf("%q container = %q, want %q", name, got.Container, e.cont)
	}
}

func TestParseGolden(t *testing.T) {
	cases := []struct {
		name string
		e    expect
	}{
		{"The.Signal.1999.1080p.BluRay.x264-GRP", expect{title: "The Signal", year: 1999, height: 1080, source: core.SourceBluRay, codec: "h264", group: "GRP"}},
		{"Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-FLUX", expect{title: "Dune Part Two", year: 2024, height: 2160, source: core.SourceWEBDL, codec: "hevc", audio: "eac3", channels: "5.1", dv: true, hdr: true, atmos: true, group: "FLUX"}},
		{"Oppenheimer.2023.2160p.UHD.BluRay.REMUX.HEVC.TrueHD.7.1-FraMeSToR", expect{title: "Oppenheimer", year: 2023, height: 2160, source: core.SourceRemux, codec: "hevc", audio: "truehd", channels: "7.1", group: "FraMeSToR"}},
		{"Boundary.Bay.S01E01.Pilot.720p.HDTV.x264-CTU", expect{title: "Boundary Bay", height: 720, source: core.SourceHDTV, codec: "h264", seasons: []int{1}, episodes: []int{1}, group: "CTU"}},
		{"Game.of.Thrones.S08E06.1080p.WEB.H264-MEMENTO", expect{title: "Game of Thrones", height: 1080, source: core.SourceWEBDL, codec: "h264", seasons: []int{8}, episodes: []int{6}, group: "MEMENTO"}},
		{"The.Office.S03E04E05.720p.WEB-DL.AAC2.0.H.264-PLAYWEB", expect{title: "The Office", height: 720, source: core.SourceWEBDL, seasons: []int{3}, episodes: []int{4, 5}, group: "PLAYWEB"}},
		{"Show.Name.S01E02-E04.1080p.WEB-DL.x265-GRP", expect{title: "Show Name", seasons: []int{1}, episodes: []int{2, 3, 4}, height: 1080, codec: "hevc"}},
		{"Old Show 1x02 Episode Title HDTV XviD-LOL", expect{title: "Old Show", seasons: []int{1}, episodes: []int{2}, source: core.SourceHDTV, codec: "xvid"}},
		{"Some.Show.S02.COMPLETE.720p.BluRay.x264-DEMAND", expect{title: "Some Show", seasons: []int{2}, full: true, height: 720, source: core.SourceBluRay}},
		{"Some Show Season 3 Complete 1080p WEB-DL DD5.1 H 264-EVO", expect{title: "Some Show", seasons: []int{3}, full: true, height: 1080, source: core.SourceWEBDL, audio: "ac3", channels: "5.1"}},
		{"Movie.Name.2020.CAM.x264-GRP", expect{title: "Movie Name", year: 2020, source: core.SourceCam}},
		{"Movie.Name.2020.HDCAM.720p-GRP", expect{source: core.SourceCam, height: 720}},
		{"Movie.Name.2020.HDTS.x264-GRP", expect{source: core.SourceCam}},
		{"Movie.Name.2020.TELESYNC-GRP", expect{source: core.SourceCam}},
		{"Movie.Name.2019.DVDSCR.x264-GRP", expect{source: core.SourceScreener, year: 2019}},
		{"Movie.Name.2018.DVDRip.XviD.AC3-GRP", expect{source: core.SourceDVD, audio: "ac3", codec: "xvid"}},
		{"Movie.Name.2021.720p.WEBRip.x264.AAC-GRP", expect{source: core.SourceWEBRip, audio: "aac", height: 720}},
		{"Movie.Name.2021.1080p.BRRip.x264-GRP", expect{source: core.SourceBluRay}},
		{"Movie.Name.2021.1080p.BDRip.x265-GRP", expect{source: core.SourceBluRay, codec: "hevc"}},
		{"1917.2019.2160p.WEB-DL.DDP5.1.Atmos.HEVC-GRP", expect{title: "1917", year: 2019, height: 2160, atmos: true}},
		{"2001.A.Space.Odyssey.1968.1080p.BluRay.x264-GRP", expect{title: "2001 A Space Odyssey", year: 1968}},
		{"Amelie.2001.FRENCH.1080p.BluRay.x264-GRP", expect{title: "Amelie", year: 2001, lang: "french"}},
		{"Movie.2022.MULTi.2160p.WEB-DL.H265-GRP", expect{lang: "multi", height: 2160}},
		{"Movie.2022.GERMAN.DL.1080p.BluRay.x264-GRP", expect{lang: "german"}},
		{"Movie.2023.PROPER.1080p.WEB-DL.x264-GRP", expect{proper: true}},
		{"Movie.2023.REPACK.720p.BluRay.x264-GRP", expect{repack: true}},
		{"Movie.2023.480p.WEB-DL.x264-GRP", expect{height: 480}},
		{"Movie.2023.576p.BluRay.x264-GRP", expect{height: 576}},
		{"Movie.2023.4K.UHD.BluRay.HEVC-GRP", expect{height: 2160, codec: "hevc"}},
		{"Movie.2023.2160p.HDR10Plus.HEVC.DTS-HD.MA.5.1-GRP", expect{hdr: true, audio: "dtshd", channels: "5.1"}},
		{"Movie.2023.2160p.DoVi.HEVC-GRP", expect{dv: true}},
		{"Movie.2023.2160p.Dolby.Vision.HEVC-GRP", expect{dv: true}},
		{"Movie.2023.1080p.BluRay.AVC.DTS.5.1-GRP", expect{codec: "h264", audio: "dts", channels: "5.1"}},
		{"Movie.2023.1080p.WEB-DL.AV1.Opus-GRP", expect{codec: "av1", audio: "opus"}},
		{"Movie.2023.1080p.BluRay.x264.FLAC.2.0-GRP", expect{audio: "flac", channels: "2.0"}},
		{"Movie.2023.1080p.WEB-DL.EAC3.5.1-GRP", expect{audio: "eac3", channels: "5.1"}},
		{"The.Signal.1999.1080p.BluRay.x264-GRP.mkv", expect{title: "The Signal", cont: "mkv", group: "GRP"}},
		{"Spider-Man.No.Way.Home.2021.1080p.WEB-DL.x264-GRP.mp4", expect{title: "Spider-Man No Way Home", cont: "mp4"}},
		{"Show.S01-S03.COMPLETE.720p.WEB-DL-GRP", expect{title: "Show", seasons: []int{1, 2, 3}, full: true}},
		{"[HorribleSubs] Some Anime - S01E03 [720p].mkv", expect{group: "HorribleSubs", seasons: []int{1}, episodes: []int{3}, height: 720, cont: "mkv"}},
		{"Movie Name (2020) [1080p] [YTS.MX]", expect{title: "Movie Name", year: 2020, height: 1080, group: "YTS.MX"}},
		{"WALL-E.2008.2160p.UHD.BluRay.x265-GRP", expect{title: "WALL-E", year: 2008, height: 2160}},
		{"Se7en.1995.1080p.BluRay.DTS.x264-GRP", expect{title: "Se7en", year: 1995, audio: "dts"}},
		{"Show.S10E123.720p.HDTV-GRP", expect{seasons: []int{10}, episodes: []int{123}}},
		{"Movie.2024.TC.720p-GRP", expect{source: core.SourceCam}},
		{"Movie.2024.1080p.WEB.H.265.DD.5.1-GRP", expect{source: core.SourceWEBDL, codec: "hevc", channels: "5.1"}},
		{"Movie.2024.2160P.WEB-DL.HDR.H265-GRP", expect{hdr: true, height: 2160}},
		{"Movie.2024.1080p.WEB-DL.DDP7.1.H264-GRP", expect{audio: "eac3", channels: "7.1"}},
		{"Show.S01E02-04.720p.WEB-DL-GRP", expect{seasons: []int{1}, episodes: []int{2, 3, 4}}},
	}
	for _, c := range cases {
		check(t, c.name, c.e)
	}
}

// TestParseCombinatorial generates a large matrix of realistic names so
// the table coverage comfortably exceeds 150 distinct releases.
func TestParseCombinatorial(t *testing.T) {
	titles := []string{"Alpha Movie", "Beta Story", "Gamma Nights", "Delta Dawn Rising", "Echo Falls"}
	heights := []struct {
		tok string
		h   int
	}{{"480p", 480}, {"720p", 720}, {"1080p", 1080}, {"2160p", 2160}}
	sources := []struct {
		tok string
		s   core.Source
	}{{"WEB-DL", core.SourceWEBDL}, {"BluRay", core.SourceBluRay}, {"HDTV", core.SourceHDTV}, {"WEBRip", core.SourceWEBRip}}
	codecs := []struct {
		tok string
		c   string
	}{{"x264", "h264"}, {"x265", "hevc"}, {"H.264", "h264"}, {"HEVC", "hevc"}}
	count := 0
	year := 2000
	for _, ti := range titles {
		for _, h := range heights {
			for _, s := range sources {
				for _, c := range codecs {
					year++
					if year > 2024 {
						year = 2000
					}
					name := fmt.Sprintf("%s.%d.%s.%s.%s-GRP%d", strings.ReplaceAll(ti, " ", "."), year, h.tok, s.tok, c.tok, count%7)
					check(t, name, expect{title: ti, year: year, height: h.h, source: s.s, codec: c.c, group: fmt.Sprintf("GRP%d", count%7)})
					count++
				}
			}
		}
	}
	// TV matrix.
	for s := 1; s <= 5; s++ {
		for e := 1; e <= 8; e++ {
			name := fmt.Sprintf("My.Show.S%02dE%02d.720p.WEB-DL.x264-GRP", s, e)
			check(t, name, expect{title: "My Show", seasons: []int{s}, episodes: []int{e}, height: 720})
			count++
		}
	}
	if count < 150 {
		t.Fatalf("only %d generated cases, want >=150 combined with golden handled separately", count)
	}
}

func TestTitleMatches(t *testing.T) {
	if !TitlesMatch("The Signal", "Signal") {
		t.Error("article")
	}
	if !TitlesMatch("Amélie", "Amelie") {
		t.Error("diacritics")
	}
	if !TitlesMatch("Law & Order", "Law and Order") {
		t.Error("ampersand")
	}
	if !TitlesMatch("Spider-Man: No Way Home", "spider man no way home") {
		t.Error("punctuation/case")
	}
	if TitlesMatch("The Signal", "The Signal Reloaded") {
		t.Error("different titles matched")
	}
	p := Parse("The.Signal.1999.1080p.BluRay.x264-GRP")
	if !TitleMatches(p, "Signal") || !TitleYearMatches(p, "The Signal", 2000) || TitleYearMatches(p, "The Signal", 2005) {
		t.Errorf("title/year match wrong for %+v", p)
	}
	ep := Parse("Show.S02E03.720p-GRP")
	if !EpisodeMatches(ep, 2, 3) || EpisodeMatches(ep, 2, 4) || EpisodeMatches(ep, 1, 3) {
		t.Error("episode matches")
	}
	pack := Parse("Show.S02.COMPLETE.720p-GRP")
	if EpisodeMatches(pack, 2, 3) {
		t.Error("season pack should not episode-match")
	}
	if !IsObfuscated("a1b2c3d4e5f6a7b8") || IsObfuscated("A Normal Title") {
		t.Error("obfuscated detection")
	}
}

func TestExplicitRegionalAudioTags(t *testing.T) {
	for _, tc := range []struct{ name, language string }{
		{"Harbor.Show.S02E18.Beach.Day.576p.WEB-DL.AAC2.0.H.264.pt-BR-GROUP", "portuguese-brazil"},
		{"Harbor.Show.S02E18.1080p.WEB.pt-PT-GROUP", "portuguese-portugal"},
		{"Harbor.Show.S02E18.1080p.WEB.EN-US-GROUP", "english"},
		{"Harbor.Show.S02E18.1080p.WEB.es-419-GROUP", "spanish-latino"},
		{"Harbor.Show.S02E18.1080p.WEB.es-ES-GROUP", "spanish-spain"},
		{"Harbor.Show.S02E18.1080p.WEB.BR-GROUP", ""},
	} {
		got := Parse(tc.name)
		found := false
		for _, l := range got.Languages {
			if l == tc.language {
				found = true
			}
		}
		if tc.language == "" && len(got.Languages) != 0 || tc.language != "" && !found {
			t.Errorf("%s: languages=%v want=%q", tc.name, got.Languages, tc.language)
		}
	}
}

func TestObscureAndLegacyReleaseParsing(t *testing.T) {
	cases := []struct {
		name string
		e    expect
	}{
		{"The.Twilight.Zone.S01.E02.DVDRip.XviD-GRP", expect{title: "The Twilight Zone", seasons: []int{1}, episodes: []int{2}, full: false, source: core.SourceDVD, codec: "xvid", group: "GRP"}},
		{"Classic_Show_S03_E09_PDTV_XviD-FOV", expect{title: "Classic Show", seasons: []int{3}, episodes: []int{9}, full: false, source: core.SourceHDTV, codec: "xvid", group: "FOV"}},
		{"Fawlty.Towers.Season.2.Episode.5.The.Anniversary.576p.PAL.DVDRip.x264-GRP", expect{title: "Fawlty Towers", seasons: []int{2}, episodes: []int{5}, full: false, height: 576, source: core.SourceDVD, codec: "h264"}},
		{"Babylon.5.S01-05.COMPLETE.480p.DVDRip.x264-GRP", expect{title: "Babylon 5", seasons: []int{1, 2, 3, 4, 5}, full: true, height: 480, source: core.SourceDVD}},
		{"Babylon.5.Seasons.1-5.COMPLETE.DVDRip-GRP", expect{title: "Babylon 5", seasons: []int{1, 2, 3, 4, 5}, full: true, source: core.SourceDVD}},
		{"Band.of.Brothers.E01.Currahee.1080p.BluRay.x264-GRP", expect{title: "Band of Brothers", episodes: []int{1}, full: false, height: 1080, source: core.SourceBluRay}},
		{"Chernobyl.2019.Ep.02.1080p.BluRay.x265-GRP", expect{title: "Chernobyl", year: 2019, episodes: []int{2}, full: false, height: 1080}},
		{"[SubsPlease] Cowboy Bebop - 05 (1080p) [ABCDEF12].mkv", expect{title: "Cowboy Bebop", group: "SubsPlease", episodes: []int{5}, height: 1080, cont: "mkv"}},
		{"Roundhay.Garden.Scene.1888.1080p.BluRay.x264-GRP", expect{title: "Roundhay Garden Scene", year: 1888, height: 1080}},
		{"Obscure.Film.1974.VHSRip.XviD.MP3-GRP", expect{title: "Obscure Film", year: 1974, source: core.SourceDVD, codec: "xvid", audio: "mp3"}},
		{"Classic.Film.2006.1080p.HDDVD.VC-1.DD5.1-GRP", expect{title: "Classic Film", year: 2006, height: 1080, source: core.SourceBluRay, codec: "vc1", audio: "ac3"}},
		{"Pal.Joey.1957.1080p.BluRay.x264-GRP", expect{title: "Pal Joey", year: 1957, height: 1080}},
	}
	for _, c := range cases {
		check(t, c.name, c.e)
	}
	if !EpisodeTitleMatches("Obscure.Show.S01E01.The.Pilot.PDTV.XviD.AC3-GRP", "The Pilot") {
		t.Fatal("legacy PDTV.XviD.AC3 tags polluted episode title matching")
	}
	if title, part, ok := MiniseriesPart("Band.of.Brothers.Part.3.Carentan.1080p.BluRay.x264-GRP"); !ok || title != "Band of Brothers" || part != 3 {
		t.Fatalf("MiniseriesPart = (%q, %d, %v)", title, part, ok)
	}
	if !EpisodeTitleMatches("Band.of.Brothers.Part.3.Carentan.1080p.BluRay.x264-GRP", "Carentan") {
		t.Fatal("miniseries episode title mismatch")
	}
	if !EpisodeTitleMatches("Band.of.Brothers.Part.3.Carentan.1080p.BluRay.x264-GRP", "Part 3") {
		t.Fatal("miniseries generic Part N title mismatch")
	}
}

func TestObscureTitleEquivalenceAndAliases(t *testing.T) {
	pairs := [][2]string{
		{"L.A. Confidential", "LA Confidential"},
		{"M*A*S*H", "MASH"},
		{"S.W.A.T.", "SWAT"},
		{"A.I. Artificial Intelligence", "AI Artificial Intelligence"},
		{"The Man from U.N.C.L.E.", "The Man from UNCLE"},
		{"Le Samouraï AKA The Godson", "Le Samourai"},
		{"Le Samouraï AKA The Godson", "The Godson"},
		{"Sen to Chihiro no Kamikakushi A.K.A. Spirited Away", "Spirited Away"},
		{"Dr. Strangelove or: How I Learned to Stop Worrying and Love the Bomb", "Dr Strangelove"},
		{"Birdman or (The Unexpected Virtue of Ignorance)", "Birdman"},
		{"The Godfather Part II", "The Godfather Part 2"},
		{"Dune: Part Two", "Dune Part 2"},
		{"Rocky IV", "Rocky 4"},
		{"The Office (US)", "The Office US"},
		{"The Office", "The Office US"},
		{"Ghosts (UK)", "Ghosts"},
	}
	for _, p := range pairs {
		if !TitlesMatch(p[0], p[1]) || !TitlesMatch(p[1], p[0]) {
			t.Errorf("TitlesMatch(%q, %q) = false, want true", p[0], p[1])
		}
	}
	aliases := TitleAliases("L.A. Confidential")
	if len(aliases) == 0 || aliases[0] != "LA Confidential" {
		t.Fatalf("TitleAliases(L.A. Confidential) = %v", aliases)
	}
	if got := TitleAliases("The Signal"); len(got) != 0 {
		t.Fatalf("ordinary title produced extra aliases: %v", got)
	}
}
