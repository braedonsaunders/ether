package transcode

import (
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Dolby Vision profile 8.2 carries an SDR (BT.709) base layer, which is
// what ffmpeg decodes: an encode must neither tone map it nor tag it HDR.
func TestDolbyVisionSDRBaseIsNotToneMapped(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	info.Streams[0].ColorTransfer = "bt709"
	info.Streams[0].DolbyVisionCompatibilityID = 2
	if encodedRange(info.Video()) != rangeSDR {
		t.Fatalf("encoded range %q", encodedRange(info.Video()))
	}
	// A client without HEVC forces an H.264 transcode.
	d := Decide(info, plexWeb, allow(), Policy{HEVCEncoder: true})
	if d.VideoCopy || d.ToneMap {
		t.Fatalf("decision %+v", d)
	}
	args := strings.Join(buildArgs(baseSpec(info, d)), " ")
	for _, bad := range []string{"tonemap", "zscale=t=linear", "smpte2084", "bt2020"} {
		if strings.Contains(args, bad) {
			t.Errorf("SDR base encode contains %q: %s", bad, args)
		}
	}
	// Even an HEVC encode decision that does not ask for tone mapping must
	// not mark the SDR picture HDR10.
	hevc := d
	hevc.VideoCodec = "hevc"
	args = strings.Join(buildArgs(baseSpec(info, hevc)), " ")
	if strings.Contains(args, "hdr10-opt") || strings.Contains(args, "smpte2084") {
		t.Errorf("HEVC encode of an SDR base layer tagged HDR: %s", args)
	}
	if attrs := HLSVariantAttributes(info, d); !strings.Contains(attrs, "VIDEO-RANGE=SDR") {
		t.Errorf("attributes %s", attrs)
	}
}

// An encode decision that kept tone mapping off for an HDR10 picture in
// H.264 (which cannot carry it) still tone maps rather than emit washed
// out PQ tagged BT.709.
func TestH264EncodeOfHDRAlwaysToneMaps(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerMPEGTS, VideoCodec: "h264", MaxHeight: 1080, AudioStream: -1}
	args := strings.Join(buildArgs(baseSpec(info, d)), " ")
	if !strings.Contains(args, "tonemap") {
		t.Errorf("no tone map: %s", args)
	}
}

// Ladder rungs below a Dolby Vision profile 5 primary (no HDR10 base) must
// not be "HDR-kept" HEVC encodes of the IPT picture. Disabling
// abrLadderUnderCopy leaves a video-copy session on the primary alone.
func TestLadderRungsOfDolbyVisionProfile5(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	info.Streams[0].DolbyVisionProfile = 5
	info.Streams[0].ColorTransfer = ""
	copyPrimary := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, VideoCodec: "hevc", AudioCopy: true, AudioStream: 1}
	if rungs := buildRungs(info, copyPrimary, adaptiveFromConfig(config.Transcode{ABRLadder: true, ABRLadderUnderCopy: false})); len(rungs) != 1 {
		t.Fatalf("copy session ladder = %d", len(rungs))
	}
	primary := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCodec: "hevc", MaxHeight: 2160, BitrateKbps: 20000, AudioStream: 1, AudioCodec: "aac"}
	rungs := buildRungs(info, primary, adaptiveFromConfig(config.Transcode{ABRLadder: true}))
	if len(rungs) < 2 {
		t.Fatalf("no rungs: %d", len(rungs))
	}
	for _, r := range rungs[1:] {
		if r.decision.VideoCodec != "h264" || !r.decision.ToneMap {
			t.Errorf("rung %s: codec %s toneMap %v", r.id, r.decision.VideoCodec, r.decision.ToneMap)
		}
	}
	// An HDR10 HEVC encode primary keeps HDR in its HEVC rungs.
	hdr := mustParse(t, "mkv_hevc_hdr10.json")
	hdrPrimary := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCodec: "hevc", MaxHeight: 2160, BitrateKbps: 20000, AudioStream: 1, AudioCodec: "aac"}
	rungs = buildRungs(hdr, hdrPrimary, adaptiveFromConfig(config.Transcode{ABRLadder: true}))
	if len(rungs) < 2 || rungs[1].decision.VideoCodec != "hevc" || rungs[1].decision.ToneMap {
		t.Errorf("HDR10 rungs: %+v", rungs)
	}
}

// The variant CODECS level follows the encoded size: a 2160p source
// transcoded to 1080p H.264 is a level 4.x stream, not 5.1.
func TestVariantCodecsLevelUsesOutputHeight(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	v := info.Video()
	v.Height, v.Width = 2160, 3840
	d := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: 1080, ToneMap: true, AudioStream: -1}
	attrs := HLSVariantAttributes(info, d)
	want := estimateCodecs("h264", "", 1080, 8)
	if !strings.Contains(attrs, want) {
		t.Errorf("attributes %s, want codecs %s", attrs, want)
	}
	if strings.Contains(attrs, estimateCodecs("h264", "", 2160, 8)) && want != estimateCodecs("h264", "", 2160, 8) {
		t.Errorf("attributes carry the source level: %s", attrs)
	}
}

// Ladder bandwidths are what each stream carries: a copy's base rung
// counts the selected (possibly re-encoded) audio, not every track in the
// file, and a rung whose copied audio leaves its video nothing is dropped
// rather than advertised below what it sends.
func TestLadderBandwidthsAreHonest(t *testing.T) {
	info := core.MediaInfo{Duration: time.Hour, BitrateKbps: 12000, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "truehd", Channels: 8, BitrateKbps: 4000},
		{Index: 2, Type: "audio", Codec: "flac", Channels: 2, BitrateKbps: 1500},
	}}
	adapt := adaptiveFromConfig(config.Transcode{ABRLadder: true})
	// TrueHD converted to AAC: 12000 - 4000 - 1500 video + AAC.
	toAAC := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioStream: 1, AudioCodec: "aac"}
	want := 6500 + audioBitrateKbps("aac", outputChannels("aac", &info.Streams[1]))
	if got := buildRungs(info, toAAC, adapt)[0].avgKbps; got != want {
		t.Errorf("base rung %d kbps, want %d", got, want)
	}
	// FLAC copied into every rung under a video-copy ladder.
	flac := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioStream: 2, AudioCopy: true, AudioCodec: "flac"}
	under := adaptiveFromConfig(config.Transcode{ABRLadder: true, ABRLadderUnderCopy: true})
	rungs := buildRungs(info, flac, under)
	if len(rungs) < 2 {
		t.Fatalf("under-copy ladder = %+v", rungs)
	}
	prev := rungs[0].avgKbps
	for _, r := range rungs[1:] {
		if r.avgKbps >= prev || r.avgKbps < minRungVideoKbps+1500 || r.decision.BitrateKbps != r.avgKbps {
			t.Errorf("rung %s advertises %d kbps (decision %d) below %d", r.id, r.avgKbps, r.decision.BitrateKbps, prev)
		}
		prev = r.avgKbps
	}
	if last := rungs[len(rungs)-1]; last.id == "360p800k" {
		t.Errorf("a 360p rung cannot carry 1500 kbps FLAC in 800 kbps")
	}
}

// A corrupt container duration must read as unknown, not overflow into a
// negative or saturated Duration (a saturated one plans billions of
// segments).
func TestParseSecondsBounds(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"5400.5": 5400500 * time.Millisecond, "1e12": 0, "9.3e9": 0, "-3": 0, "nan": 0, "inf": 0, "": 0,
	} {
		if got := parseSeconds(in); got != want {
			t.Errorf("parseSeconds(%q) = %v, want %v", in, got, want)
		}
	}
}
