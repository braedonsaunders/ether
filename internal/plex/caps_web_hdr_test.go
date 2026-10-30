package plex

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func hdr10HEVCInfo() core.MediaInfo {
	return core.MediaInfo{Container: "mkv", BitrateKbps: 20000, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Width: 3840, Height: 2160, BitDepth: 10, ColorTransfer: "smpte2084"},
		{Index: 1, Type: "audio", Codec: "eac3", Channels: 6},
	}}
}

func webDecide(t *testing.T, product, platform, extra string) (core.ClientCaps, core.Decision) {
	t.Helper()
	caps := (&Request{Product: product, Platform: platform, Device: "OSX", ProfileExtra: extra}).ClientCaps(0)
	d := transcode.Decide(hdr10HEVCInfo(), caps, core.DecideOptions{
		DirectPlay: false, DirectStream: true,
		AudioStream: -1, SubtitleStream: -1,
	}, transcode.Policy{HEVCEncoder: true})
	return caps, d
}

// Unknown/unsupported: H.264-only HLS target with no HDR claim must not
// Direct Stream HDR10 (Braedon review of #136).
func TestWebHDRUnknownH264OnlyToneMaps(t *testing.T) {
	extra := "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts&videoCodec=h264&audioCodec=aac)"

	firefoxCaps, firefoxDec := webDecide(t, "Plex Web", "Firefox", extra)
	if firefoxCaps.HDR10 || firefoxCaps.SupportsVideo("hevc") {
		t.Fatalf("Firefox h264-only must not gain HEVC/HDR10: %+v", firefoxCaps)
	}
	if firefoxDec.VideoCopy || !firefoxDec.ToneMap {
		t.Fatalf("Firefox: want tone-map, got %+v", firefoxDec)
	}

	// Chrome lists hevc in base caps, but omission must not set HDR10.
	chromeCaps, chromeDec := webDecide(t, "Plex Web", "Chrome", extra)
	if !chromeCaps.SupportsVideo("hevc") {
		t.Fatalf("Chrome still lists base hevc: %+v", chromeCaps)
	}
	if chromeCaps.HDR10 || chromeDec.VideoCopy || !chromeDec.ToneMap {
		t.Fatalf("Chrome h264-only target must tone-map HDR: caps=%+v decision=%+v", chromeCaps, chromeDec)
	}
}

func TestWebHDRExplicitDenialToneMaps(t *testing.T) {
	// Safari base claims HDR10; denial must clear it even with hevc listed.
	base := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)"
	for _, deny := range []string{
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)",
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=notMatch&name=video.colorTrc&value=smpte2084)",
	} {
		caps, d := webDecide(t, "Plex Web", "Safari", base+deny)
		if caps.HDR10 || d.VideoCopy || !d.ToneMap {
			t.Fatalf("HDR denial ignored: caps=%+v decision=%+v", caps, d)
		}
		if !caps.SupportsVideo("hevc") {
			t.Fatalf("hevc should remain listed after denial: %+v", caps)
		}
	}
}

// HEVC listed without an HDR signal must tone-map: HEVC ≠ HDR10.
func TestWebHDRHEVCWithoutHDREvidenceToneMaps(t *testing.T) {
	for _, tc := range []struct {
		name, platform, extra string
	}{
		{
			name:     "firefox direct-play hevc",
			platform: "Firefox",
			extra:    "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)",
		},
		{
			name:     "chrome transcode-target hevc",
			platform: "Chrome",
			extra:    "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mp4&videoCodec=h264,hevc&audioCodec=aac)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps, d := webDecide(t, "Plex Web", tc.platform, tc.extra)
			if !caps.SupportsVideo("hevc") {
				t.Fatalf("expected hevc listed: %+v", caps)
			}
			if caps.HDR10 {
				t.Fatalf("hevc alone must not claim HDR10: %+v", caps)
			}
			if d.VideoCopy || !d.ToneMap {
				t.Fatalf("HDR unknown: want tone-map, got %+v", d)
			}
		})
	}
}

func TestWebHDRNoProfileExtraDoesNotPromote(t *testing.T) {
	caps := (&Request{Product: "Plex Web", Platform: "Chrome", Device: "OSX"}).ClientCaps(0)
	if caps.HDR10 {
		t.Fatalf("Chrome without profile-extra must not claim HDR10: %+v", caps)
	}
	caps = (&Request{Product: "Plex Web", Platform: "Firefox", Device: "Windows"}).ClientCaps(0)
	if caps.HDR10 || caps.SupportsVideo("hevc") {
		t.Fatalf("Firefox without profile-extra must stay conservative: %+v", caps)
	}
}
