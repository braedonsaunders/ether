package resolver

import (
	"context"
	"testing"

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
