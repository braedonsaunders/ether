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

	// Chrome lists hevc in base caps, but h264-only target must not promote HDR10.
	chromeCaps, chromeDec := webDecide(t, "Plex Web", "Chrome", extra)
	if !chromeCaps.SupportsVideo("hevc") {
		t.Fatalf("Chrome still lists base hevc: %+v", chromeCaps)
	}
	if chromeCaps.HDR10 || chromeDec.VideoCopy || !chromeDec.ToneMap {
		t.Fatalf("Chrome h264-only target must tone-map HDR: caps=%+v decision=%+v", chromeCaps, chromeDec)
	}
}

func TestWebHDRExplicitDenialToneMaps(t *testing.T) {
	base := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)"
	for _, platform := range []string{"Firefox", "Chrome", "Safari"} {
		for _, deny := range []string{
			"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)",
			"+add-limitation(scope=videoCodec&scopeName=hevc&type=notMatch&name=video.colorTrc&value=smpte2084)",
		} {
			caps, d := webDecide(t, "Plex Web", platform, base+deny)
			if caps.HDR10 || d.VideoCopy || !d.ToneMap {
				t.Fatalf("%s HDR denial ignored: caps=%+v decision=%+v", platform, caps, d)
			}
			if !caps.SupportsVideo("hevc") {
				t.Fatalf("%s: hevc should remain listed after denial: %+v", platform, caps)
			}
		}
	}
}

// Safari starts from HDR10. A PQ denial expressed only as list= must clear
// that base claim. value= already did; list= used to be ignored here.
func TestWebHDRListDenialClearsBaseHDR10(t *testing.T) {
	base := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)"
	deny := "+add-limitation(scope=videoCodec&scopeName=hevc&type=notMatch&name=video.colorTrc&list=smpte2084)"
	caps, d := webDecide(t, "Plex Web", "Safari", base+deny)
	if caps.HDR10 || d.VideoCopy || !d.ToneMap {
		t.Fatalf("list= PQ denial left Safari HDR10: caps=%+v decision=%+v", caps, d)
	}
	if !caps.SupportsVideo("hevc") {
		t.Fatalf("hevc should remain listed after list= denial: %+v", caps)
	}
}

// HEVC listed without an affirmative 10-bit/PQ claim must not promote HDR10
// (Braedon #136: hevc alone ≠ HDR10).
func TestWebHDRHEVCAloneDoesNotPromoteHDR10(t *testing.T) {
	for _, tc := range []struct {
		name, platform, extra string
	}{
		{
			name:     "firefox direct-play hevc",
			platform: "Firefox",
			extra:    "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)",
		},
		{
			name:     "firefox thin ether dash append",
			platform: "Firefox",
			extra:    "append-transcode-target-codec(type=videoProfile&context=streaming&videoCodec=h264%2Chevc&audioCodec=aac&protocol=dash)",
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
			if caps.HDR10 || d.VideoCopy || !d.ToneMap {
				t.Fatalf("hevc alone must tone-map HDR10: caps=%+v decision=%+v", caps, d)
			}
		})
	}
}

// Sanitized PMS Firefox Ghostbusters (2026-10-30) profile augmentation that
// Direct Streamed HDR10 HEVC (video copy, audio AAC). Affirmative signals:
// bitDepth upperBound=10 and colorTrc match list including smpte2084.
const pmsFirefoxHDR10Extra = "add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.width&value=4096&replace=true)" +
	"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.height&value=2160&replace=true)" +
	"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=10&replace=true)" +
	"+append-transcode-target-codec(type=videoProfile&context=streaming&protocol=dash&videoCodec=hevc)" +
	"+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=streaming&protocol=dash&type=match&name=video.colorTrc&list=bt709|bt470m|bt470bg|smpte170m|smpte240m|bt2020-10|smpte2084&isRequired=false)" +
	"+append-transcode-target-codec(type=videoProfile&context=streaming&videoCodec=h264%2Chevc&audioCodec=aac&protocol=dash)"

func TestWebHDRPMSFirefoxAffirmativeRemuxes(t *testing.T) {
	caps, d := webDecide(t, "Plex Web", "Firefox", pmsFirefoxHDR10Extra)
	if !caps.HDR10 || !caps.SupportsVideo("hevc") {
		t.Fatalf("expected HEVC+HDR10 from PMS Firefox signals: %+v", caps)
	}
	if !d.VideoCopy || d.ToneMap {
		t.Fatalf("expected video-copy Direct Stream, got %+v", d)
	}
}

func TestWebHDRBitDepth10AloneRemuxes(t *testing.T) {
	extra := "append-transcode-target-codec(type=videoProfile&context=streaming&protocol=dash&videoCodec=hevc)" +
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=10&replace=true)"
	caps, d := webDecide(t, "Plex Web", "Firefox", extra)
	if !caps.HDR10 || !d.VideoCopy || d.ToneMap {
		t.Fatalf("bitDepth=10 with hevc should remux: caps=%+v decision=%+v", caps, d)
	}
}

func TestWebHDRColorTrcPQAloneRemuxes(t *testing.T) {
	extra := "append-transcode-target-codec(type=videoProfile&context=streaming&protocol=dash&videoCodec=hevc)" +
		"+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&type=match&name=video.colorTrc&list=bt709|smpte2084&isRequired=false)"
	caps, d := webDecide(t, "Plex Web", "Chrome", extra)
	if !caps.HDR10 || !d.VideoCopy || d.ToneMap {
		t.Fatalf("smpte2084 allow-list with hevc should remux: caps=%+v decision=%+v", caps, d)
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

// Promotion is Plex Web only — desktop/other products keep product-base HDR.
func TestWebHDRPromotionScopedToPlexWeb(t *testing.T) {
	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=aac)"
	caps := (&Request{Product: "Plex for Windows", Platform: "Windows", Device: "standalone", ProfileExtra: extra}).ClientCaps(0)
	// Desktop already has HDR10 from product base; ensure we did not rely on web promotion alone.
	if !caps.HDR10 {
		t.Fatalf("desktop base should keep HDR10: %+v", caps)
	}
	// A non-web product that does not claim HDR in base must not gain it from hevc listing.
	caps = (&Request{Product: "Unknown Thing", Platform: "Linux", Device: "box", ProfileExtra: extra}).ClientCaps(0)
	if caps.HDR10 {
		t.Fatalf("non-web product must not gain HDR10 from hevc profile-extra: %+v", caps)
	}
}
