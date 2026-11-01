package resolver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestExactHEVCTranscodeAdmissionDoesNotSubstitute(t *testing.T) {
	for _, tc := range []struct {
		name             string
		permit, download bool
		prefer, want     string
	}{
		{"selected HEVC for transcode", true, false, "hevc", "hevc"},
		{"legacy codec policy", false, false, "hevc", "h264"},
		{"download remains restricted", true, true, "hevc", "h264"},
		{"another choice is not relaxed", true, false, "h264", "h264"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := &fakeIndexer{releases: []core.Release{
				rel("hevc", "The.Signal.1999.1080p.WEB-DL.HEVC-GRP", 4000),
				rel("h264", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 4000),
			}}
			s := New(idx, fakeHealth{}, nil, testCfg(), Options{})
			defer s.Close()
			caps := core.ClientCaps{VideoCodecs: []string{"h264"}, Download: tc.download}
			got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps, Prefer: tc.prefer, ExactHEVCTranscode: tc.permit})
			if err != nil || got == nil || got.Release.ID != tc.want {
				t.Fatalf("selected %+v, error %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestInfuseAndTVOSPoolPicksHEVCOverAV1UnlessAdvertised(t *testing.T) {
	pool := []core.Release{
		rel("av1", "The.Signal.1999.1080p.WEB-DL.DDP5.1.AV1-GRP", 3500),
		rel("hevc", "The.Signal.1999.1080p.WEB-DL.DDP5.1.HEVC-GRP", 3500),
	}
	cfg := testCfg()
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].PreferredCodecs = []string{"av1", "hevc", "h264"}
	}

	for _, tc := range []struct {
		name string
		caps core.ClientCaps
		want string
	}{
		{
			name: "Infuse-Direct on AppleTV without declared codecs picks HEVC",
			caps: core.ClientCaps{Protocol: "jellyfin", Product: "Infuse-Direct", DeviceName: "AppleTV", MaxHeight: 2160},
			want: "hevc",
		},
		{
			name: "Swiftfin on tvOS picks HEVC",
			caps: core.ClientCaps{Protocol: "jellyfin", Product: "Swiftfin", Platform: "tvOS", MaxHeight: 2160},
			want: "hevc",
		},
		{
			name: "Infuse with apple-tv floor codecs picks HEVC",
			caps: core.ClientCaps{Protocol: "jellyfin", Product: "Infuse-Direct", DeviceName: "AppleTV", MaxHeight: 2160, VideoCodecs: []string{"h264", "hevc"}},
			want: "hevc",
		},
		{
			name: "Client that explicitly advertises AV1 can still get AV1",
			caps: core.ClientCaps{Protocol: "jellyfin", Product: "Infuse-Direct", DeviceName: "AppleTV", MaxHeight: 2160, VideoCodecs: []string{"h264", "hevc", "av1"}},
			want: "av1",
		},
		{
			name: "Plex Apple TV profile without AV1 picks HEVC unchanged",
			caps: core.ClientCaps{Protocol: "plex", Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", MaxHeight: 2160, VideoCodecs: []string{"h264", "hevc"}},
			want: "hevc",
		},
		{
			name: "Plex client with unrestricted codecs is unchanged",
			caps: core.ClientCaps{Protocol: "plex", Product: "Plex for Windows", Platform: "Windows", MaxHeight: 2160},
			want: "av1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := &fakeIndexer{releases: pool}
			s := New(idx, fakeHealth{}, nil, cfg, Options{})
			defer s.Close()
			got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: tc.caps})
			if err != nil || got == nil || got.Release.ID != tc.want {
				t.Fatalf("got %+v (err=%v), want %s", got, err, tc.want)
			}
		})
	}
}

func TestConcurrentExactHEVCAdmissionDoesNotShareLegacyDecision(t *testing.T) {
	idx := &fakeIndexer{delay: 50 * time.Millisecond, releases: []core.Release{
		rel("hevc", "The.Signal.1999.1080p.WEB-DL.HEVC-GRP", 4000),
		rel("h264", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 4000),
	}}
	s := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: core.ClientCaps{VideoCodecs: []string{"h264"}}, Prefer: "hevc"}
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	type result struct {
		id  string
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := req
			request.ExactHEVCTranscode = i == 1
			ready <- struct{}{}
			<-start
			resolution, err := s.Resolve(context.Background(), request)
			results[i].err = err
			if resolution != nil {
				results[i].id = resolution.Release.ID
			}
		}(i)
	}
	<-ready
	<-ready
	close(start)
	wg.Wait()
	if results[0].err != nil || results[0].id != "h264" || results[1].err != nil || results[1].id != "hevc" {
		t.Fatalf("legacy=%+v exact=%+v", results[0], results[1])
	}
}
