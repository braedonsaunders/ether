package transcode

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestNativePlexAppleTVUsesCopyOnlyHDR10Base(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	for _, container := range []string{ContainerMatroska, ContainerFMP4} {
		t.Run(container, func(t *testing.T) {
			caps := appleTV
			caps.HLSContainers = []string{container}
			d := Decide(info, caps, allow(), Policy{})
			if d.Kind != core.DecisionRemux || !d.VideoCopy || !d.AudioCopy || !d.StripDolbyVision || d.ToneMap || d.MaxHeight != info.Video().Height || d.BitrateKbps != info.BitrateKbps {
				t.Fatalf("base-layer decision: %+v", d)
			}
			args := buildArgs(baseSpec(info, d))
			mustContain(t, args, "-c:v copy -bsf:v dovi_rpu=strip=1", "-c:a copy")
			mustNotContain(t, args, "-strict unofficial", "libx265", "libx264", "-filter_complex", "tonemap")
			attrs := HLSVariantAttributes(info, d)
			if strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS") || !strings.Contains(attrs, "VIDEO-RANGE=PQ") {
				t.Fatalf("incorrect HDR10 signaling: %s", attrs)
			}
		})
	}
}

func TestNativePlexAppleTVProfile7RemovesEnhancementLayer(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	info.Video().DolbyVisionProfile = 7
	info.Video().DolbyVisionCompatibilityID = 6
	for _, container := range []string{ContainerMatroska, ContainerFMP4} {
		t.Run(container, func(t *testing.T) {
			caps := appleTV
			caps.HLSContainers = []string{container}
			d := Decide(info, caps, allow(), Policy{})
			if d.Kind != core.DecisionRemux || !d.VideoCopy || !d.StripDolbyVision || d.ToneMap || d.MaxHeight != info.Video().Height {
				t.Fatalf("profile 7 must retain the HDR10 base: %+v", d)
			}
			args := buildArgs(baseSpec(info, d))
			mustContain(t, args, "-c:v copy -bsf:v dovi_rpu=strip=1,filter_units=remove_types=62|63")
			mustNotContain(t, args, "libx265", "libx264", "-filter_complex", "tonemap")
			attrs := HLSVariantAttributes(info, d)
			if strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS") || !strings.Contains(attrs, "VIDEO-RANGE=PQ") {
				t.Fatalf("profile 7 advertised as DV after stripping: %s", attrs)
			}
			// A replacement profile 7 source must retain the stripping plan.
			next := adaptDecision(d, info, info, container)
			if !next.VideoCopy || !next.StripDolbyVision {
				t.Fatalf("profile 7 failover lost base-layer plan: %+v", next)
			}
		})
	}
	// Do not mistake an unsupported or absent base for HDR10.
	info.Video().DolbyVisionCompatibilityID = 0
	if hdr10BaseReason(info.Video(), appleTV, allow()) != "" {
		t.Fatal("unknown profile 7 base accepted")
	}
	info.Video().DolbyVisionCompatibilityID = 6
	absent := false
	info.Video().DolbyVisionBLPresent = &absent
	if hdr10BaseReason(info.Video(), appleTV, allow()) != "" {
		t.Fatal("missing profile 7 base accepted")
	}
}

func TestHDR10BaseQuirkDoesNotAffectOtherPlayersOrFormats(t *testing.T) {
	base := mustParse(t, "mkv_dv_p8.json")
	for _, tc := range []struct {
		name   string
		caps   core.ClientCaps
		modify func(*core.StreamInfo)
	}{
		{"iOS", iOS, nil},
		{"Infuse on tvOS", core.ClientCaps{Product: "Infuse", Platform: "tvOS", HDR10: true, DolbyVision: true}, nil},
		{"native HDR10", appleTV, func(v *core.StreamInfo) { v.DolbyVision = false }},
		{"SDR HEVC", appleTV, func(v *core.StreamInfo) { v.DolbyVision = false; v.ColorTransfer = "bt709" }},
		{"DV5 no HDR base", appleTV, func(v *core.StreamInfo) { v.DolbyVisionProfile = 5; v.DolbyVisionCompatibilityID = 0 }},
		{"unknown compatibility", appleTV, func(v *core.StreamInfo) { v.DolbyVisionCompatibilityID = 0 }},
		{"HLG compatible DV", appleTV, func(v *core.StreamInfo) { v.DolbyVisionCompatibilityID = 4; v.ColorTransfer = "arib-std-b67" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := base
			info.Streams = append([]core.StreamInfo(nil), base.Streams...)
			if tc.modify != nil {
				tc.modify(info.Video())
			}
			d := Decide(info, tc.caps, allow(), Policy{HEVCEncoder: true})
			if d.StripDolbyVision {
				t.Fatalf("unrelated player/format changed: %+v", d)
			}
		})
	}
	caps := appleTV
	caps.MaxBitrateKbps = 2000
	d := Decide(base, caps, allow(), Policy{HEVCEncoder: true})
	if d.Kind != core.DecisionTranscode || d.VideoCopy || d.StripDolbyVision {
		t.Fatalf("compatibility bypassed bandwidth limit: %+v", d)
	}
}

func TestHDR10BaseFailoverDoesNotStripProfile5(t *testing.T) {
	original := mustParse(t, "mkv_dv_p8.json")
	old := Decide(original, appleTV, allow(), Policy{})
	for _, tc := range []struct {
		name            string
		profile         int
		dv, copy, strip bool
	}{
		{"same HDR10 base", 8, true, true, true},
		{"plain HDR10", 0, false, true, false},
		{"DV5 needs conversion", 5, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := original
			next.Streams = append([]core.StreamInfo(nil), original.Streams...)
			v := next.Video()
			v.DolbyVision = tc.dv
			v.DolbyVisionProfile = tc.profile
			if tc.profile == 5 {
				v.DolbyVisionCompatibilityID = 0
				v.ColorTransfer = "unknown"
			}
			d := adaptDecision(old, original, next, old.Container)
			if d.VideoCopy != tc.copy || d.StripDolbyVision != tc.strip {
				t.Fatalf("adapted decision: %+v", d)
			}
		})
	}
}

// A compatible base is an output contract: the decision, metadata and bytes
// must agree, regardless of the segmented container requested by the client.
func TestUnsupportedDVProfileDeliversHDR10Base(t *testing.T) {
	for _, profile := range []int{7, 8} {
		for _, container := range []string{ContainerMatroska, ContainerFMP4, ContainerMPEGTS} {
			info := mustParse(t, "mkv_dv_p8.json")
			info.Video().DolbyVisionProfile = profile
			if profile == 7 {
				info.Video().DolbyVisionCompatibilityID = 6
			}
			caps := hdr10TV
			caps.HLSContainers = []string{container}
			d := Decide(info, caps, allow(), Policy{})
			if d.Kind != core.DecisionRemux || !d.VideoCopy || !d.StripDolbyVision || d.ToneMap {
				t.Fatalf("profile=%d container=%s: %+v", profile, container, d)
			}
			mustContain(t, buildArgs(baseSpec(info, d)), "dovi_rpu=strip=1")
			attrs := HLSVariantAttributes(info, d)
			if strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS") || !strings.Contains(attrs, "VIDEO-RANGE=PQ") {
				t.Fatal(attrs)
			}
		}
	}
	info := mustParse(t, "mkv_dv_p8.json")
	caps := hdr10TV
	caps.Device, caps.DolbyVision = "SHIELD Android TV", true
	for _, profile := range []int{5, 8} {
		info.Video().DolbyVisionProfile = profile
		d := Decide(info, caps, allow(), Policy{})
		if d.Kind != core.DecisionDirectPlay || d.StripDolbyVision {
			t.Fatalf("supported profile %d: %+v", profile, d)
		}
	}
	caps.DolbyVision = false
	info.Video().DolbyVisionProfile = 5
	d := Decide(info, caps, allow(), Policy{})
	if d.VideoCopy || d.StripDolbyVision || !d.ToneMap {
		t.Fatalf("DV5 cannot strip to HDR10: %+v", d)
	}
}

// The profile, compatibility id, mux plan, and HLS advertisement must agree.
func TestAndroidDVVariantDelivery(t *testing.T) {
	for _, source := range []string{"mkv", "mp4"} {
		for _, output := range []string{ContainerMatroska, ContainerFMP4} {
			for _, tc := range []struct {
				name            string
				profile, compat int
				transfer        string
				dv, copy, strip bool
			}{
				{"P7 FEL or MEL", 7, 6, "smpte2084", true, true, true},
				{"P8.1", 8, 1, "smpte2084", true, true, false},
				{"P8.4", 8, 4, "arib-std-b67", true, false, false},
				{"P8 unknown base", 8, 0, "smpte2084", true, false, false},
				{"unknown profile", 0, 0, "unknown", true, false, false},
				{"P5 supported", 5, 0, "unknown", true, true, false},
				{"P5 denied", 5, 0, "unknown", false, false, false},
			} {
				t.Run(source+"/"+output+"/"+tc.name, func(t *testing.T) {
					info := mustParse(t, "mkv_dv_p8.json")
					info.Container = source
					v := info.Video()
					v.DolbyVisionProfile = tc.profile
					v.DolbyVisionCompatibilityID = tc.compat
					v.ColorTransfer = tc.transfer
					caps := hdr10TV
					caps.Platform = "Android"
					caps.Device = "SHIELD Android TV"
					caps.DolbyVision = tc.dv
					caps.HLSContainers = []string{output}
					d := Decide(info, caps, allow(), Policy{})
					if d.VideoCopy != tc.copy || d.StripDolbyVision != tc.strip {
						t.Fatalf("%+v", d)
					}
					if tc.strip || !tc.copy {
						if tc.strip {
							v.CodecString = "dvh1.07.06"
						}
						attrs := HLSVariantAttributes(info, d)
						if strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS") {
							t.Fatal(attrs)
						}
					}
				})
			}
		}
	}
}
