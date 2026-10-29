package resolver

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestAnimeBDPassesIdentityAndImportedQuality(t *testing.T) {
	cases := []struct {
		name, title string
		episode     int
	}{
		{"[Serendipity] Lovely Complex - S01E01 (BD 1080p HEVC Opus 2.0) [Dual Audio] [7BBF1985]", "Freshman Year Summer! I'll Definitely Find a Boyfriend!", 1},
		{"Lovely Complex S01E01 BD 1080p HEVC Opus 2.0 VOSTFR", "Freshman Year Summer! I'll Definitely Find a Boyfriend!", 1},
		{"Lovely.Complex.S01E01.Freshman.Year.Summer.I.ll.Definitely.Find.a.Boyfriend.ITA.ENG.JAP.1080p.BDMux.AC3.2.0.h265", "Freshman Year Summer! I'll Definitely Find a Boyfriend!", 1},
		{"Lovely.Complex.S01E02.The.Ex.Girlfriend.Love.Triangle.ITA.ENG.JAP.1080p.BDMux.AC3.2.0.h265", "The Ex-Girlfriend Love Triangle?!", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title := &core.Title{Key: core.EpisodeKey(100, 1, tc.episode), Type: core.TypeEpisode, Title: tc.title, ShowTitle: "Lovely★Complex", ShowYear: 2007, Season: 1, Episode: tc.episode, Runtime: 24 * time.Minute}
			cfg := testCfg()
			cfg.Quality.Profiles = []core.QualityProfile{{Name: "imported", AllowedQualities: []string{"bluray-1080"}, MaxHeight: 1080}}
			cfg.Quality.Default = "imported"
			cfg.Quality.Rules = nil
			cfg.Quality.Remote = ""
			r := rel("bd", tc.name, 500)
			idx := &fakeIndexer{releases: []core.Release{r}}
			svc := New(idx, fakeHealth{}, nil, cfg, Options{})
			defer svc.Close()
			got, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: core.ClientCaps{MaxHeight: 1080, VideoCodecs: []string{"hevc", "h264"}}})
			if err != nil || got.Release.ID != "bd" {
				t.Fatalf("resolve=%+v err=%v", got, err)
			}
			parsed := withParsed(r)
			req := searchRequestFor(title)
			req.Episode = 3
			if reason := rejectIdentity(parsed, title, req); reason != "season/episode mismatch" {
				t.Fatalf("wrong episode accepted: %q", reason)
			}
		})
	}
}
