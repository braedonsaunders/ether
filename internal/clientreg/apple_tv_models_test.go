package clientreg

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Every Apple TV reports device "Apple TV"; only the hardware model tells
// the 1080p SDR Apple TV HD from a 4K box.
func TestAppleTVHDByModel(t *testing.T) {
	atv := func(model string) Identity {
		return Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", DeviceName: "Apple TV", Model: model}
	}
	hd := Playback(atv("AppleTV5,3"), nil)
	if hd.ID != "apple-tv-hd" || hd.MaxHeight != 1080 || hd.HDR10 || hd.DolbyVision {
		t.Fatalf("Apple TV HD playback = %+v", hd)
	}
	if p := Policy(atv("AppleTV5,3"), nil); p.ID != "apple-tv-hd" || len(p.DolbyVisionProfiles) != 0 {
		t.Fatalf("Apple TV HD policy = %+v", p)
	}
	for _, model := range []string{"", "AppleTV6,2", "AppleTV11,1", "AppleTV14,1"} {
		got := Playback(atv(model), nil)
		if got.ID != "apple-tv" || got.MaxHeight != 2160 || !got.HDR10 || !got.DolbyVision {
			t.Fatalf("model %q playback = %+v", model, got)
		}
		if p := Policy(atv(model), nil); p.ID != "apple-tv" {
			t.Fatalf("model %q policy = %s", model, p.ID)
		}
	}
	if got := DeviceType(atv("AppleTV5,3")); got != "Apple TV HD" {
		t.Fatalf("HD device type = %q", got)
	}
	// Rules saved before the split stored "Apple TV"; the 4K row keeps it.
	if got := DeviceType(atv("AppleTV14,1")); got != "Apple TV" {
		t.Fatalf("4K device type = %q", got)
	}
	if !DeviceTypeMatches("Apple TV HD", "Apple TV") || !DeviceTypeMatches("Apple TV HD", "Apple TV HD") {
		t.Fatal("an Apple TV rule must cover the HD box")
	}
	if DeviceTypeMatches("Apple TV", "Apple TV HD") || DeviceTypeMatches("", "Apple TV") {
		t.Fatal("an Apple TV HD rule must not cover other Apple TVs")
	}
}

// An owner edit to one Apple TV row leaves the other alone.
func TestAppleTVRowsEditSeparately(t *testing.T) {
	hd := Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", Model: "AppleTV5,3"}
	uhd := Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", Model: "AppleTV14,1"}
	edit := []core.ClientCapabilityOverride{{ID: "apple-tv", MaxHeight: 720}}
	if got := Playback(hd, edit).MaxHeight; got != 1080 {
		t.Fatalf("4K row edit reached the HD box: %d", got)
	}
	if got := Playback(uhd, edit).MaxHeight; got != 720 {
		t.Fatalf("4K row edit missing: %d", got)
	}
}

func TestModelName(t *testing.T) {
	for code, want := range map[string]string{
		"AppleTV5,3": "Apple TV HD", " appletv14,1 ": "Apple TV 4K (3rd gen)", "AFTMM": "Fire TV Stick 4K",
		"": "", "AppleTV99,9": "",
	} {
		if got := ModelName(code); got != want {
			t.Fatalf("ModelName(%q) = %q, want %q", code, got, want)
		}
	}
}

// The catalogue lists both Apple TV rows, each with its own type id.
func TestAppleTVCatalogueRows(t *testing.T) {
	ids := map[string]string{}
	for _, d := range Devices() {
		if prev, ok := ids[d.TypeID()]; ok {
			t.Fatalf("type id %q used by %q and %q", d.TypeID(), prev, d.Name)
		}
		ids[d.TypeID()] = d.Name
	}
	if ids["Apple TV"] != "Apple TV 4K" || ids["Apple TV HD"] != "Apple TV HD" {
		t.Fatalf("Apple TV rows = %v", ids)
	}
}

// Plex for Apple TV sends X-Plex-Model without the "AppleTV" prefix
// ("5,3"), as captured from a real Apple TV HD on tvOS 26.6.1.
func TestAppleTVBareModelCodes(t *testing.T) {
	atv := func(model string) Identity {
		return Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", DeviceName: "Apple TV", Model: model}
	}
	if got := Playback(atv("5,3"), nil); got.ID != "apple-tv-hd" || got.MaxHeight != 1080 || got.HDR10 || got.DolbyVision {
		t.Fatalf("bare 5,3 playback = %+v", got)
	}
	for _, model := range []string{"6,2", "11,1", "14,1"} {
		if got := Playback(atv(model), nil); got.ID != "apple-tv" || got.MaxHeight != 2160 {
			t.Fatalf("bare %s playback = %+v", model, got)
		}
	}
	if got := CanonicalModel(atv("5,3")); got != "AppleTV5,3" {
		t.Fatalf("CanonicalModel = %q", got)
	}
	if got := ModelName(CanonicalModel(atv("14,1"))); got != "Apple TV 4K (3rd gen)" {
		t.Fatalf("ModelName = %q", got)
	}
	// A bare code from another vendor is left alone.
	if got := CanonicalModel(Identity{Product: "Plex for Android (TV)", Platform: "Android", Model: "5,3"}); got != "5,3" {
		t.Fatalf("non-Apple bare code rewritten: %q", got)
	}
}
