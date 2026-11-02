package resolver

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSelectProfileDownload(t *testing.T) {
	q := fixtureQuality()

	// A 4K/HDR Apple TV asking for a download gets the compact mobile
	// profile instead: SDR 1080p, WEB-DL first, MP4-friendly codecs.
	caps := appleTVCaps()
	caps.Download = true
	p := SelectProfile(q, caps, nil)
	if p.MaxHeight != 1080 || p.AllowHDR || p.AllowDolbyVision {
		t.Fatalf("download profile not clamped to SDR 1080p: %+v", p)
	}
	if len(p.PreferredSources) == 0 || p.PreferredSources[0] != core.SourceWEBDL {
		t.Fatalf("download sources = %v, want WEBDL first", p.PreferredSources)
	}
	if len(p.PreferredCodecs) == 0 || p.PreferredCodecs[0] != "h264" {
		t.Fatalf("download codecs = %v, want h264 first", p.PreferredCodecs)
	}
	for _, a := range p.PreferredAudio {
		if a == "truehd" || a == "dts" || a == "dtshd" {
			t.Fatalf("download audio %v contains non-MP4-friendly %q", p.PreferredAudio, a)
		}
	}
	if p.MaxSizeMBPerMin > 80 {
		t.Fatalf("download MaxSizeMBPerMin = %v, want <= 80", p.MaxSizeMBPerMin)
	}

	// A client without HEVC only gets H.264 for downloads.
	caps = webCaps()
	caps.Download = true
	p = SelectProfile(q, caps, nil)
	if len(p.PreferredCodecs) != 1 || p.PreferredCodecs[0] != "h264" {
		t.Fatalf("no-HEVC download codecs = %v", p.PreferredCodecs)
	}

	// The client's requested height (videoResolution) still clamps.
	caps.MaxHeight = 720
	if p = SelectProfile(q, caps, nil); p.MaxHeight != 720 {
		t.Fatalf("download MaxHeight = %d, want 720", p.MaxHeight)
	}

	// Non-download selection is untouched.
	if p = SelectProfile(q, appleTVCaps(), nil); p.Name != "4K" || !p.AllowHDR {
		t.Fatalf("playback profile changed: %+v", p)
	}
}
