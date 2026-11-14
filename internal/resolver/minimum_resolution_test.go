package resolver

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMinimumResolutionIsAPreference(t *testing.T) {
	s := New(&fakeIndexer{}, nil, nil, testCfg(), Options{})
	defer s.Close()
	profile := s.ProfileFor(appleTVCaps(), nil)
	profile.MinHeight = 2160
	for _, tc := range []struct {
		name      string
		release   string
		height    int
		preferred bool
		want      string
	}{
		{"below minimum", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 1080, false, ""},
		{"at minimum", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 2160, false, ""},
		{"unlabelled sd web", "The.Signal.1999.WEB-DL.H.264-GRP", 0, false, ""},
		{"unlabelled sd webrip", "The.Signal.1999.WEBRip.x264-GRP", 0, false, ""},
		{"unlabelled xvid rip", "The.Signal.1999.XviD-AFG", 0, false, ""},
		{"unlabelled legacy hdtv", "The.Signal.1999.HDTV.x264-GRP", 0, false, ""},
		{"unlabelled dvd rip", "The.Signal.1999.DVDRip.XviD-GRP", 0, false, ""},
		{"explicit version", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 1080, true, ""},
		{"explicit unlabelled web", "The.Signal.1999.WEB-DL.H.264-GRP", 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := withParsed(rel("candidate", tc.release, 8000))
			r.Info.Height = tc.height
			got := s.rejectReason(r, matrixTitle(), searchRequestFor(matrixTitle()), profile, s.resolverCfg(), appleTVCaps(), nil, tc.preferred)
			if got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMinimumResolutionDoesNotHideTitleForOtherProfiles(t *testing.T) {
	cfg := testCfg()
	for i := range cfg.Quality.Profiles {
		if cfg.Quality.Profiles[i].Name == "4K" {
			cfg.Quality.Profiles[i].MinHeight = 2160
		}
	}
	s := New(&fakeIndexer{releases: []core.Release{rel("hd", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 8000)}}, fakeHealth{}, nil, cfg, Options{})
	defer s.Close()
	if res, err := s.Resolve(t.Context(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}); err != nil || res.Release.ID != "hd" || res.QualityFallback == "" {
		t.Fatalf("4K-only resolve: %v", err)
	}
	if available, known := s.Available(t.Context(), matrixTitle().Key); !known || !available {
		t.Fatal("profile-specific floor marked the title unavailable for everyone")
	}
	if result, err := s.Resolve(t.Context(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil || result.Release.ID != "hd" {
		t.Fatalf("1080p profile result=%v err=%v", result, err)
	}
}

// A soft 4K preference still permits an SD release when hard DV/device gates reject 4K.
func TestFourKPreferenceFallsBackWhenFourKIsUnplayable(t *testing.T) {
	sd := rel("sd", "The.Signal.1999.WEB-DL.AAC2.0.H.264-GRP", 1500)
	dv := rel("dv", "The.Signal.1999.2160p.WEB-DL.DDP5.1.DV.H.265-GRP", 18000)
	hdr10TV := core.ClientCaps{Product: "Plex for Android (TV)", MaxHeight: 2160, HDR10: true,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}}
	for name, caps := range map[string]core.ClientCaps{"hdr10 tv": hdr10TV, "1080p client": webCaps()} {
		t.Run(name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Quality.Rules = nil
			cfg.Quality.Default = "4K"
			for i := range cfg.Quality.Profiles {
				if cfg.Quality.Profiles[i].Name == "4K" {
					cfg.Quality.Profiles[i].MinHeight = 2160
				}
			}
			s := New(&fakeIndexer{releases: []core.Release{sd, dv}}, fakeHealth{}, nil, cfg, Options{})
			defer s.Close()
			res, err := s.Resolve(t.Context(), core.ResolveRequest{Title: matrixTitle(), Client: caps})
			if err != nil || res.Release.ID != "sd" || res.QualityFallback == "" {
				t.Fatalf("fallback result=%+v err=%v", res, err)
			}
			traces := s.Traces()
			if len(traces) == 0 {
				t.Fatal("expected resolution trace")
			}
			var sdTrace *CandidateTrace
			for i := range traces[0].Candidates {
				if traces[0].Candidates[i].ReleaseID == "sd" {
					sdTrace = &traces[0].Candidates[i]
				}
			}
			if sdTrace == nil || sdTrace.Height != 0 || sdTrace.Rejected || sdTrace.Reason != "Best available — below this profile's preferred quality" {
				t.Fatalf("expected unknown height 480p floor note in trace, got %+v", sdTrace)
			}
		})
	}
}
