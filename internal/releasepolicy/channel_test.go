package releasepolicy

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestChannelSourcePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    core.ReleaseInfo
		size    int64
		allowed bool
	}{
		{name: "Film.1999.1080p.WEB-DL.x264-GRP", size: 3_000_000_000, allowed: true},
		{name: "Film.1999.1080p.BluRay.x265-GRP", size: 3_000_000_000, allowed: true},
		{name: "Film.1999.720p.BluRay.x264-GRP", size: 1_000_000_000, allowed: true},
		{name: "Film.1999.2160p.WEB-DL.x265-GRP", size: 1_000_000_000},
		{name: "Film.1999.1080p.BluRay.REMUX.x264-GRP", size: 1_000_000_000},
		{name: "Film.1999.1080p.BDMV-GRP", size: 1_000_000_000},
		{name: "Film.1999.1080p.BluRay.iso", size: 1_000_000_000},
		{name: "Film.1999.1080p.COMPLETE.BLURAY-GRP", size: 1_000_000_000},
		{name: "Film.1999.1080p.BD50-GRP", size: 1_000_000_000},
		{name: "Film.1999.1080p.WEB-DL.x264-GRP", info: core.ReleaseInfo{Height: 2160}, size: 1_000_000_000},
		{name: "Film.1999.1080p.WEB-DL.x264-GRP", info: core.ReleaseInfo{Source: core.SourceRemux}, size: 1_000_000_000},
		{name: "Film.1999.1080p.WEB-DL.x264-GRP", size: 4_500_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := core.Release{Name: tc.name, Info: tc.info, Size: tc.size}
			if got := ChannelSourceAllowed(r, 2*time.Hour, 6000); got != tc.allowed {
				t.Fatalf("allowed=%v want %v", got, tc.allowed)
			}
		})
	}
}
