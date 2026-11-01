package clientreg

import (
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestRegistryVersionAndFallback(t *testing.T) {
	if Version() != 1 {
		t.Fatal(Version())
	}
	unknown := Policy(Identity{Product: "Totally Unknown"}, nil)
	if unknown.Family != "generic" || len(unknown.Subtitles) == 0 {
		t.Fatalf("%+v", unknown)
	}
	play := Playback(Identity{Product: "Totally Unknown"}, nil)
	if play.ID != "generic" || play.MaxHeight != 1080 || play.HDR10 || play.DolbyVision || !slices.Equal(play.Video, []string{"h264"}) {
		t.Fatalf("%+v", play)
	}
}

func TestPolicyMatchesHistoricalFamilies(t *testing.T) {
	text := []string{"srt", "subrip", "webvtt", "vtt", "ass", "ssa"}
	image := append(append([]string{}, text...), "pgs", "hdmv_pgs_subtitle", "vobsub", "dvd_subtitle")
	genericNote := "Text is served separately; image subtitles require explicit client support."
	appleNote := "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported."
	browserNote := "Browser codec declarations govern playback; text subtitles remain sidecars."
	cases := []struct {
		name          string
		id            Identity
		family, notes string
		dv            []int
		subs          []string
	}{
		{"apple tv", Identity{Product: "Apple TV"}, "Apple TV", appleNote, []int{5, 8}, text},
		{"tvos infuse", Identity{Product: "Infuse", Platform: "tvOS"}, "Apple TV", appleNote, []int{5, 8}, text},
		{"appletv infuse-direct", Identity{Product: "Infuse-Direct", Device: "AppleTV"}, "Apple TV", appleNote, []int{5, 8}, text},
		{"ios", Identity{Product: "iOS"}, "iOS", genericNote, []int{5, 8}, text},
		{"ipados", Identity{Platform: "iPadOS"}, "iOS", genericNote, []int{5, 8}, text},
		{"plex web", Identity{Product: "Plex Web Chrome"}, "Plex Web", browserNote, nil, text},
		{"safari", Identity{Product: "Safari"}, "Plex Web", browserNote, nil, text},
		{"shield", Identity{Product: "NVIDIA Shield"}, "Android TV (Shield)", genericNote, []int{5, 8}, image},
		{"android tv", Identity{Product: "Android TV"}, "Android / Google TV", genericNote, nil, text},
		{"google tv", Identity{Product: "Google TV"}, "Android / Google TV", genericNote, nil, text},
		{"fire tv", Identity{Product: "Fire TV"}, "Fire TV", genericNote, nil, text},
		{"roku", Identity{Product: "Roku"}, "Roku", genericNote, nil, text},
		{"webos", Identity{Product: "LG webOS"}, "LG webOS", genericNote, nil, text},
		{"tizen", Identity{Product: "Samsung Tizen"}, "Samsung Tizen", genericNote, nil, text},
		{"xbox", Identity{Product: "Xbox"}, "Console", genericNote, nil, text},
		{"playstation", Identity{Product: "PlayStation"}, "Console", genericNote, nil, text},
		{"htpc", Identity{Product: "HTPC"}, "HTPC", genericNote, []int{5, 7, 8}, image},
		{"kodi", Identity{Product: "Kodi"}, "HTPC", genericNote, []int{5, 7, 8}, image},
		{"infuse", Identity{Product: "Infuse"}, "HTPC", genericNote, []int{5, 7, 8}, image},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Policy(tc.id, nil)
			if got.Family != tc.family || got.Notes != tc.notes || !slices.Equal(got.DolbyVisionProfiles, tc.dv) || !slices.Equal(got.Subtitles, tc.subs) {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestPlaybackMatchesHistoricalProducts(t *testing.T) {
	cases := []struct {
		product, platform, device, model string
		hevc, hdr, dv                    bool
		height                           int
		mkv                              bool
	}{
		{"Plex for Apple TV", "tvOS", "Apple TV", "", true, true, true, 2160, true},
		{"Plex for iOS", "iOS", "iPhone", "", true, true, true, 2160, true},
		{"Plex Web", "Chrome", "OSX", "", true, false, false, 2160, true},
		{"Plex Web", "Edge", "Windows", "", true, false, false, 2160, true},
		{"Plex Web", "Safari", "OSX", "", true, true, false, 2160, false},
		{"Plex Web", "Firefox", "Windows", "", false, false, false, 2160, true},
		{"Plex Web", "Linux", "standalone", "", false, false, false, 2160, true},
		{"Plex Web", "Opera", "Windows", "", true, false, false, 2160, true},
		{"Plex for Linux", "Linux", "standalone", "", true, true, false, 2160, true},
		{"PLEX FOR LINUX", "linux", "standalone", "", true, true, false, 2160, true},
		{"Plex for Windows", "Windows", "standalone", "", true, true, false, 2160, true},
		{"Plex for Mac", "macOS", "standalone", "", true, true, false, 2160, true},
		{"Plex Desktop", "Linux", "standalone", "", true, true, false, 2160, true},
		{"Plezy", "Linux", "Linux", "", true, true, false, 2160, true},
		{"Plezy", "Generic", "Linux", "", true, true, false, 2160, true},
		{"Plezy", "tvOS", "Apple TV", "", true, true, true, 2160, true},
		{"Plezy", "iOS", "iPhone", "", true, true, true, 2160, true},
		{"Plezy", "Android", "Pixel 8", "", true, false, false, 1080, true},
		{"Plezy", "Android", "Android", "", true, false, false, 1080, true},
		{"Plezy", "Android", "SHIELD Android TV", "", true, true, false, 2160, true},
		{"Plex for Android (TV)", "Android", "SHIELD Android TV", "", true, true, false, 2160, true},
		{"Plex for Android (Mobile)", "Android", "Pixel 8", "", true, false, false, 1080, true},
		{"Plex for Android", "Android", "AFTKA", "AFTKA", true, true, false, 2160, true},
		{"Plex for Android", "Android", "Living Room", "AFTKRT", true, true, false, 2160, true},
		{"Plex for Android", "Android", "AFTSS", "AFTSS", true, false, false, 1080, true},
		{"Plex for Roku", "Roku", "Roku Ultra", "", true, true, false, 2160, true},
		{"Infuse", "tvOS", "Apple TV", "", true, true, true, 2160, true},
		{"Infuse-Direct", "", "AppleTV", "", true, true, true, 2160, true},
		{"Infuse", "", "", "", true, true, false, 2160, true},
		{"Swiftfin tvOS", "", "", "", true, true, true, 2160, true},
		{"Swiftfin", "tvOS", "Apple TV", "", true, true, true, 2160, true},
		{"Fire TV", "", "", "", true, true, false, 2160, true},
		{"Something", "Opera", "", "", false, false, false, 1080, false},
		{"Something", "Chrome", "", "", true, false, false, 2160, true},
		{"Unknown Thing", "", "", "", false, false, false, 1080, false},
		{"Unknown Thing", "Linux", "standalone", "", false, false, false, 1080, false},
	}
	for _, tc := range cases {
		t.Run(tc.product+"/"+tc.platform+"/"+tc.device, func(t *testing.T) {
			got := Playback(Identity{Product: tc.product, Platform: tc.platform, Device: tc.device, Model: tc.model}, nil)
			hevc := slices.Contains(got.Video, "hevc")
			mkv := slices.Contains(got.Containers, "mkv")
			if hevc != tc.hevc || got.HDR10 != tc.hdr || got.DolbyVision != tc.dv || got.MaxHeight != tc.height || mkv != tc.mkv || !slices.Contains(got.Video, "h264") || !slices.Contains(got.Audio, "aac") {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestInfuseAndAppleTVExcludeAV1ByDefault(t *testing.T) {
	for _, id := range []Identity{
		{Product: "Infuse-Direct", Device: "AppleTV"},
		{Product: "Infuse", Platform: "tvOS", Device: "Apple TV"},
		{Product: "Infuse"},
		{Product: "Swiftfin", Platform: "tvOS"},
		{Product: "Swiftfin tvOS"},
	} {
		got := Playback(id, nil)
		if slices.Contains(got.Video, "av1") {
			t.Fatalf("%+v unexpectedly includes av1: %v", id, got.Video)
		}
		if !slices.Contains(got.Video, "h264") || !slices.Contains(got.Video, "hevc") {
			t.Fatalf("%+v missing h264/hevc: %v", id, got.Video)
		}
	}
}

func TestOverrideChangesPlaybackAndLeavesDeclaredProfiles(t *testing.T) {
	id := Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV"}
	base := Playback(id, nil)
	if !slices.Contains(base.Video, "hevc") || !slices.Contains(Policy(id, nil).DolbyVisionProfiles, 8) {
		t.Fatal(base)
	}
	off := false
	next := Playback(id, []core.ClientCapabilityOverride{{
		ID:                  "apple-tv",
		VideoCodecs:         []string{"h264"},
		DolbyVisionProfiles: []int{5, 7, 8},
		DolbyVision:         &off,
		MaxHeight:           720,
	}})
	if slices.Contains(next.Video, "hevc") || next.DolbyVision || next.MaxHeight != 720 || !next.HeightLocked || !slices.Equal(next.DolbyVisionProfiles, []int{5, 7, 8}) {
		t.Fatalf("%+v", next)
	}
	policy := Policy(id, []core.ClientCapabilityOverride{{ID: "apple-tv", DolbyVisionProfiles: []int{7}}})
	if !slices.Equal(policy.DolbyVisionProfiles, []int{7}) {
		t.Fatal(policy.DolbyVisionProfiles)
	}
	other := Playback(Identity{Product: "Plex for iOS", Platform: "iOS"}, []core.ClientCapabilityOverride{{ID: "apple-tv", VideoCodecs: []string{"h264"}}})
	if !slices.Contains(other.Video, "hevc") {
		t.Fatal("apple tv override applied to iOS")
	}
}

func TestVersionMatchAndQuirks(t *testing.T) {
	rule := Match{Field: "version", Op: "semverAtLeast", Value: "1.4.0"}
	if !Matches(rule, Identity{Version: "1.4.1"}) || Matches(rule, Identity{Version: "1.3.9"}) || Matches(rule, Identity{}) {
		t.Fatal("semver")
	}
	if !Matches(Match{Field: "version", Op: "semverBelow", Value: "2"}, Identity{Version: "1.9.0"}) {
		t.Fatal("below")
	}
	apple := Identity{Product: "Plex for Apple TV", Platform: "tvOS"}
	if !HasQuirk(apple, "plex-appletv-hdr10-base") || HasQuirk(Identity{Product: "Infuse", Platform: "tvOS"}, "plex-appletv-hdr10-base") {
		t.Fatal("hdr10 base quirk")
	}
	phone := Identity{Product: "Plex for Android (Mobile)", Platform: "Android", Device: "Pixel 8"}
	tv := Identity{Product: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV"}
	web := Identity{Product: "Plex Web", Platform: "Android"}
	if !HasQuirk(phone, "android-mobile-ceiling") || HasQuirk(tv, "android-mobile-ceiling") || HasQuirk(web, "android-mobile-ceiling") {
		t.Fatal("android ceiling")
	}
	if !HasQuirk(phone, "plex-list-all-tracks") || !HasQuirk(tv, "plex-list-all-tracks") || HasQuirk(web, "plex-list-all-tracks") {
		t.Fatal("track list quirk")
	}
	if !HasQuirk(Identity{Platform: "iOS", Product: "Plex"}, "plex-native-live-tune") || HasQuirk(Identity{Product: "Plex Web", Platform: "Chrome"}, "plex-native-live-tune") {
		t.Fatal("live tune quirk")
	}
}

func TestSwiftfinPlaybackIncludesMP3AndAC3(t *testing.T) {
	got := Playback(Identity{Product: "Swiftfin tvOS"}, nil)
	if got.ID != "apple-tv" || !slices.Contains(got.Audio, "mp3") || !slices.Contains(got.Audio, "ac3") || got.MaxHeight != 2160 {
		t.Fatalf("%+v", got)
	}
}
