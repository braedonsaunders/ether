package resolver

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// The selection key is taken from the configured definition before
// client-cap clamping: the same configured profile under different
// caps (HDR TV at 2160, SDR web at 1080) yields the same key even
// though the effective profiles differ, and editing the definition
// changes it.
func TestSelectProfileSelectionKeyIsPreClamp(t *testing.T) {
	mkQuality := func(maxHeight int) config.Quality {
		return config.Quality{
			Default: "Main",
			Profiles: []core.QualityProfile{{
				Name:             "Main",
				MaxHeight:        maxHeight,
				MinHeight:        720,
				AllowHDR:         true,
				AllowDolbyVision: true,
				PreferredSources: []core.Source{core.SourceWEBDL},
				PreferredCodecs:  []string{"hevc", "h264"},
				PreferredAudio:   []string{"eac3", "aac"},
				MinSizeMBPerMin:  5,
				MaxSizeMBPerMin:  150,
			}},
		}
	}
	tv := core.ClientCaps{MaxHeight: 2160, HDR10: true, DolbyVision: true, VideoCodecs: []string{"hevc", "h264"}}
	web := core.ClientCaps{MaxHeight: 1080, VideoCodecs: []string{"h264"}}

	pTV := SelectProfile(mkQuality(2160), tv, nil)
	pWeb := SelectProfile(mkQuality(2160), web, nil)
	if pTV.MaxHeight != 2160 || pWeb.MaxHeight != 1080 || pTV.AllowHDR == pWeb.AllowHDR {
		t.Fatalf("caps did not clamp differently: tv %+v web %+v", pTV, pWeb)
	}
	if pTV.SelectionKey == "" || pTV.SelectionKey != pWeb.SelectionKey {
		t.Fatalf("SelectionKey tv %q web %q; want equal and non-empty", pTV.SelectionKey, pWeb.SelectionKey)
	}

	// Editing the configured definition (ceiling 2160 -> 1080) changes it.
	if edited := SelectProfile(mkQuality(1080), tv, nil); edited.SelectionKey == pTV.SelectionKey {
		t.Fatalf("edited definition kept SelectionKey %q", edited.SelectionKey)
	}
	// Editing any other selection field (audio here) also changes it.
	q := mkQuality(2160)
	q.Profiles[0].PreferredAudio = []string{"aac"}
	if edited := SelectProfile(q, tv, nil); edited.SelectionKey == pTV.SelectionKey {
		t.Fatalf("audio edit kept SelectionKey %q", edited.SelectionKey)
	}
}
