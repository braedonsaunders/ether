package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSourceSnapshotSurvivesFailoverAndStop(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "history-snapshot")
	first, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || first.Revision == "" || first.ReleaseID != "A" {
		t.Fatalf("first source: %+v %v", first, err)
	}
	if _, err = h.m.Failover(context.Background(), ps.ID, 0); err != nil {
		t.Fatal(err)
	}
	second, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || second.ReleaseID != "B" || first.Revision == second.Revision || first.ReleaseID != "A" {
		t.Fatalf("source snapshots: %+v %+v %v", first, second, err)
	}
	h.m.Stop(ps.ID)
	if _, err = h.m.CurrentSource(context.Background(), ps.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("retired source: %v", err)
	}
}

func TestObservedHDRRequiresConsistentEvidence(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		video core.StreamInfo
		want  *bool
	}{
		{"unknown transfer", core.StreamInfo{}, nil},
		{"bt709 with unknown metadata", core.StreamInfo{ColorTransfer: "bt709"}, nil},
		{"bt709 with HDR10+", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &yes}, &yes},
		{"bt709 with DV configuration", core.StreamInfo{ColorTransfer: "bt709", DolbyVisionConfigurationPresent: &yes}, &yes},
		{"bt709 with DV enhancement", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no, DolbyVisionELPresent: &yes}, &yes},
		{"PQ", core.StreamInfo{ColorTransfer: "smpte2084"}, &yes},
		{"explicit absence", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no}, &no},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := observedHDR(tc.video)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
