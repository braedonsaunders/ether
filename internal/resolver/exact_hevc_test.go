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
