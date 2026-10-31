package resolver

import (
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMinimumResolutionIsAProfileGate(t *testing.T) {
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
		{"below minimum", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 1080, false, "below profile min height"},
		{"at minimum", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 2160, false, ""},
		{"unlabelled sd web", "The.Signal.1999.WEB-DL.H.264-GRP", 0, false, "below profile min height"},
		{"unlabelled sd webrip", "The.Signal.1999.WEBRip.x264-GRP", 0, false, "below profile min height"},
		{"unlabelled xvid rip", "The.Signal.1999.XviD-AFG", 0, false, "below profile min height"},
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
	if _, err := s.Resolve(t.Context(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("4K-only resolve: %v", err)
	}
	if known, _ := s.Available(t.Context(), matrixTitle().Key); known {
		t.Fatal("profile-specific floor marked the title unavailable for everyone")
	}
	if result, err := s.Resolve(t.Context(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil || result.Release.ID != "hd" {
		t.Fatalf("1080p profile result=%v err=%v", result, err)
	}
}

// A 4K floor must not fall back to an unlabelled (SD) WEB-DL when the 4K
// release is unplayable for the client: Dolby Vision on a non-DV TV, or a
// 1080p client. Reported as Coyote vs. Acme picking an SD WEB-DL.
func TestFourKFloorRejectsUnlabelledSDWhenFourKIsUnplayable(t *testing.T) {
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
			if err == nil {
				t.Fatalf("picked %q under a 4K floor", res.Release.ID)
			}
			if !errors.Is(err, core.ErrNotAvailable) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
