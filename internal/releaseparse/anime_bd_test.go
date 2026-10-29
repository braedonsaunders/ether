package releaseparse

import (
	"github.com/braedonsaunders/ether/internal/core"
	"slices"
	"testing"
)

func TestAnimeBDEpisodeTags(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"[Serendipity] Lovely Complex - S01E01 (BD 1080p HEVC Opus 2.0) [Dual Audio] [7BBF1985]", "Freshman Year Summer! I'll Definitely Find a Boyfriend!"},
		{"Lovely Complex S01E01 BD 1080p HEVC Opus 2.0 VOSTFR", "Freshman Year Summer! I'll Definitely Find a Boyfriend!"},
		{"Lovely.Complex.S01E01.Freshman.Year.Summer.I.ll.Definitely.Find.a.Boyfriend.ITA.ENG.JAP.1080p.BDMux.AC3.2.0.h265", "Freshman Year Summer! I'll Definitely Find a Boyfriend!"},
		{"Lovely.Complex.S01E02.The.Ex.Girlfriend.Love.Triangle.ITA.ENG.JAP.1080p.BDMux.AC3.2.0.h265", "The Ex-Girlfriend Love Triangle?!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !EpisodeTitleMatches(tc.name, tc.want) {
				t.Error("episode title rejected")
			}
			got := Parse(tc.name)
			if got.Source != core.SourceBluRay || got.Height != 1080 {
				t.Errorf("source=%q height=%d", got.Source, got.Height)
			}
		})
	}
}

func TestAnimeBDGuards(t *testing.T) {
	for _, tag := range []string{"BD", "BDMux", "BD-Rip", "BDRip"} {
		name := "Show.S01E01.The.Right.Title." + tag + ".1080p.HEVC"
		if Parse(name).Source != core.SourceBluRay || !EpisodeTitleMatches(name, "The Right Title") {
			t.Errorf("tag failed: %s", name)
		}
	}
	for _, pair := range [][2]string{{"I.ll Be There", "I'll Be There"}, {"Ill Be There", "I'll Be There"}, {"I.ll Be There", "I’ll Be There"}, {"Don.t Go", "Don't Go"}, {"We.re Home", "We're Home"}} {
		if !EpisodeTitleMatches("Show.S01E01."+pair[0]+".BD.1080p", pair[1]) {
			t.Errorf("contraction mismatch %v", pair)
		}
	}
	for _, tc := range []struct{ name, want string }{
		{"Show.S01E01.The.Wrong.Title.ITA.1080p.BDMux", "The Right Title"},
		{"Show.S01E01.The.Wrong.Title.(BD.1080p)", "The Right Title"},
		{"Show.S01E01.The.Wrong.Title.[BD.1080p]", "The Right Title"},
		{"Show.S01E01.The.Right.Other.Title.BD.1080p", "The Right Title"},
		{"Show.S01E01.Stone.and.Sky.(1).BD.1080p", "Stone and Sky (2)"},
		{"Show.S01E01.I.ll.Be.There.BD.1080p", "I'll Be Gone"},
	} {
		if EpisodeTitleMatches(tc.name, tc.want) {
			t.Errorf("wrong episode accepted: %s vs %s", tc.name, tc.want)
		}
	}
	if got := Parse("The.BD.Story.2020.1080p.WEB-DL"); got.Title != "The BD Story" || got.Source != core.SourceWEBDL {
		t.Fatalf("title/source: %+v", got)
	}
	if got := Parse("The.BD.Story.2020.1080p"); got.Source != core.SourceUnknown {
		t.Fatalf("title became source: %+v", got)
	}
	if got := Parse("Show.S01E01.BDMux.1080p.HDCAM"); got.Source != core.SourceCam {
		t.Fatalf("cam override lost: %+v", got)
	}
	got := Parse("Show.S01E01.ITA.ENG.JAP.JPN.1080p.BDMux.VOSTFR")
	for _, lang := range []string{"italian", "english", "japanese"} {
		if !slices.Contains(got.Languages, lang) {
			t.Errorf("missing audio %s: %+v", lang, got)
		}
	}
	if slices.Contains(got.Languages, "french") || !slices.Contains(got.SubtitleLanguages, "french") {
		t.Fatalf("subtitle became audio: %+v", got)
	}
}
