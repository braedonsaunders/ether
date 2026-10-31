package plex

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestParseRequestHeadersAndQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/library/sections?X-Plex-Token=qtok&X-Plex-Product=Plex%20Web&x-plex-client-identifier=cid&X-Plex-Container-Start=20&X-Plex-Container-Size=10", nil)
	r.Header.Set("X-Plex-Platform", "Chrome")
	r.Header.Set("X-Plex-Device-Name", "Office")
	r.Header.Set("X-Plex-Session-Identifier", "sess")
	r.Header.Set("Accept", "text/html, application/json;q=0.9")
	r.RemoteAddr = "203.0.113.9:4444"
	pr := ParseRequest(r)
	if pr.Token != "qtok" || pr.Product != "Plex Web" || pr.ClientID != "cid" || pr.Platform != "Chrome" || pr.DeviceName != "Office" || pr.SessionID != "sess" {
		t.Fatalf("headers: %+v", pr)
	}
	if !pr.HasPage || pr.Start != 20 || pr.Size != 10 {
		t.Fatalf("paging: %+v", pr)
	}
	if pr.Format != model.JSON {
		t.Fatal("json not detected")
	}
	if !pr.Remote {
		t.Fatal("public address should be remote")
	}
	if lo, hi := pr.window(25); lo != 20 || hi != 25 {
		t.Fatalf("window %d %d", lo, hi)
	}
}

func TestRemoteDetection(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.5:1":     false,
		"10.0.0.2:1":        false,
		"172.20.1.1:1":      false,
		"100.101.102.103:1": false, // tailscale CGNAT
		"127.0.0.1:1":       false,
		"[::1]:1":           false,
		"[fd00::1]:1":       false,
		"8.8.8.8:1":         true,
		"[2001:db8::1]:1":   true,
	}
	for addr, remote := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		if got := ParseRequest(r).Remote; got != remote {
			t.Errorf("%s: remote=%v", addr, got)
		}
	}
	// A loopback reverse proxy forwards the real client.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "8.8.4.4, 10.0.0.1")
	if !ParseRequest(r).Remote {
		t.Error("forwarded public client should be remote")
	}
	// But a remote peer can't spoof being local.
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "8.8.8.8:1"
	r.Header.Set("X-Forwarded-For", "192.168.1.1")
	if !ParseRequest(r).Remote {
		t.Error("spoofed XFF honoured")
	}
}

func TestForwardedLocalPrefixCannotHideRemoteClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		remote bool
	}{
		{"spoofed loopback", []string{"127.0.0.1, 198.51.100.7"}, true},
		{"spoofed private through proxies", []string{"10.0.0.7, 198.51.100.7, 10.0.0.2"}, true},
		{"separate header lines", []string{"127.0.0.1", "198.51.100.7"}, true},
		{"mapped IPv4", []string{"::1, ::ffff:198.51.100.7"}, true},
		{"malformed", []string{"not-an-ip"}, true},
		{"empty", []string{""}, true},
		{"local chain", []string{"192.168.1.7, 127.0.0.1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "127.0.0.1:32400"
			for _, value := range tc.values {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := ParseRequest(r).Remote; got != tc.remote {
				t.Fatalf("remote=%v, want %v", got, tc.remote)
			}
		})
	}
}

func TestClientCapsByProduct(t *testing.T) {
	cases := []struct {
		product, platform, device string
		hevc, hdr, dv             bool
		maxHeight                 int
		mkv                       bool
	}{
		{"Plex for Apple TV", "tvOS", "Apple TV", true, true, true, 2160, true},
		{"Plex for iOS", "iOS", "iPhone", true, true, true, 2160, true},
		{"Plex Web", "Chrome", "OSX", true, false, false, 2160, true},
		{"Plex Web", "Edge", "Windows", true, false, false, 2160, true},
		{"Plex Web", "Safari", "OSX", true, true, false, 2160, false},
		{"Plex Web", "Firefox", "Windows", false, false, false, 2160, true},
		{"Plex Web", "Linux", "standalone", false, false, false, 2160, true},
		{"Plex for Linux", "Linux", "standalone", true, true, false, 2160, true},
		{"PLEX FOR LINUX", "linux", "standalone", true, true, false, 2160, true},
		{"Plex for Windows", "Windows", "standalone", true, true, false, 2160, true},
		{"Plex for Mac", "macOS", "standalone", true, true, false, 2160, true},
		{"Plex Desktop", "Linux", "standalone", true, true, false, 2160, true},
		{"Plezy", "Linux", "Linux", true, true, false, 2160, true},
		{"Plezy", "Generic", "Linux", true, true, false, 2160, true},
		{"Plezy", "tvOS", "Apple TV", true, true, true, 2160, true},
		{"Plezy", "iOS", "iPhone", true, true, true, 2160, true},
		{"Plezy", "Android", "Pixel 8", true, false, false, 1080, true},
		{"Plezy", "Android", "Android", true, false, false, 1080, true},
		{"Plezy", "Android", "SHIELD Android TV", true, true, false, 2160, true},
		{"Plex for Android (TV)", "Android", "SHIELD Android TV", true, true, false, 2160, true},
		{"Plex for Android (Mobile)", "Android", "Pixel 8", true, false, false, 1080, true},
		{"Plex for Roku", "Roku", "Roku Ultra", true, true, false, 2160, true},
		{"Unknown Thing", "", "", false, false, false, 1080, false},
		{"Unknown Thing", "Linux", "standalone", false, false, false, 1080, false},
	}
	for _, c := range cases {
		pr := &Request{Product: c.product, Platform: c.platform, Device: c.device}
		caps := pr.ClientCaps(0)
		if caps.SupportsVideo("hevc") != c.hevc || caps.HDR10 != c.hdr || caps.DolbyVision != c.dv || caps.MaxHeight != c.maxHeight || caps.SupportsContainer("mkv") != c.mkv {
			t.Errorf("%s/%s: %+v", c.product, c.platform, caps)
		}
		if !caps.SupportsVideo("h264") || !caps.SupportsAudio("aac") {
			t.Errorf("%s: missing baseline codecs", c.product)
		}
	}
}

func TestLinuxDesktopProfileLimits(t *testing.T) {
	quality := config.Quality{
		Default:  "4K",
		Profiles: []core.QualityProfile{{Name: "4K", MinHeight: 1080, MaxHeight: 2160}},
	}
	for _, tc := range []struct {
		name, header, value string
		wantHeight          int
	}{
		{"desktop default", "", "", 2160},
		{"profile height limit", "X-Plex-Client-Profile-Extra", "add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.height&value=1080)", 1080},
		{"legacy resolution limit", "X-Plex-Client-Capabilities", "videoDecoders=h264{resolution:720}", 720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/library/metadata/1000000000950028", nil)
			r.Header.Set("X-Plex-Product", "Plex for Linux")
			r.Header.Set("X-Plex-Platform", "Linux")
			r.Header.Set("X-Plex-Model", "standalone")
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			caps := ParseRequest(r).ClientCaps(0)
			profile := resolver.SelectProfile(quality, caps, nil)
			if profile.MaxHeight != tc.wantHeight {
				t.Fatalf("profile max height = %d, want %d (client caps: %+v)", profile.MaxHeight, tc.wantHeight, caps)
			}
		})
	}
}

func TestProfileExtra(t *testing.T) {
	raw := "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts&videoCodec=h264,hevc&audioCodec=aac,ac3,eac3)" +
		"+append-transcode-target-codec(type=videoProfile&context=streaming&protocol=hls&audioCodec=dca)" +
		"+add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=av1&audioCodec=opus&subtitleCodec=*)" +
		"+add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.height&value=720)" +
		"+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)" +
		"+add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.bitrate&value=6000)"
	for name, extra := range map[string]string{
		"raw":     raw,
		"encoded": url.PathEscape(raw),
		"query":   func() string { v, _ := url.QueryUnescape(url.QueryEscape(raw)); return v }(),
	} {
		pr := &Request{Product: "Plex for Apple TV", Platform: "tvOS", ProfileExtra: extra}
		c := pr.ClientCaps(0)
		if !c.SupportsVideo("av1") || !c.SupportsAudio("dts") || !c.SupportsAudio("opus") || !c.SupportsContainer("mkv") {
			t.Errorf("%s: codecs %+v", name, c)
		}
		if c.MaxHeight != 720 || c.MaxBitrateKbps != 6000 || c.HDR10 || c.DolbyVision {
			t.Errorf("%s: limits %+v", name, c)
		}
	}
	ds := ParseProfileExtra(raw)
	if len(ds) != 6 || ds[0].Verb != "add-transcode-target" || ds[0].Args["videoCodec"] != "h264,hevc" {
		t.Fatalf("directives: %+v", ds)
	}
}

func TestLegacyCapabilities(t *testing.T) {
	pr := &Request{Product: "Old Client", ClientCapabilities: "protocols=http-live-streaming,http-mp4-streaming;videoDecoders=h264{profile:high&resolution:720&level:51},h265{resolution:720};audioDecoders=mp3,aac{bitrate:160000},ac3"}
	c := pr.ClientCaps(0)
	if !c.SupportsVideo("hevc") || !c.SupportsAudio("ac3") || c.MaxHeight != 720 {
		t.Fatalf("legacy: %+v", c)
	}
}

func TestRemoteBitrateCap(t *testing.T) {
	c := (&Request{Product: "Plex for iOS", Platform: "iOS", Remote: true}).ClientCaps(8000)
	if c.MaxBitrateKbps != 8000 || !c.Remote {
		t.Fatalf("remote cap: %+v", c)
	}
	c = (&Request{Product: "Plex for iOS", Platform: "iOS"}).ClientCaps(8000)
	if c.MaxBitrateKbps != 0 {
		t.Fatalf("local capped: %+v", c)
	}
	q := url.Values{"maxVideoBitrate": {"3000"}, "videoResolution": {"1280x720"}, "location": {"cellular"}}
	applyDecisionQuery(&c, q)
	if c.MaxBitrateKbps != 3000 || c.MaxHeight != 720 || !c.Remote {
		t.Fatalf("decision query: %+v", c)
	}
	for in, want := range map[string]int{"1920x1080": 1080, "1080x1920": 1080, "720": 720, "1080p": 1080, "bad": 0, "": 0} {
		if got := parseResolutionHeight(in); got != want {
			t.Errorf("%q: %d", in, got)
		}
	}
	if !slices.Contains(c.VideoCodecs, "hevc") {
		t.Error("caps lost")
	}
}

func TestPickAudioPrefersLanguageOverDefaultFlag(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264"},
		{Index: 1, Type: "audio", Codec: "eac3", Language: "tur", Default: true, Channels: 2},
		{Index: 2, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6},
	}}
	if got := pickAudio(info, []string{"en"}); got != 2 {
		t.Fatalf("pickAudio(en) = %d, want the English track", got)
	}
	if got := pickAudio(info, nil); got != 1 {
		t.Fatalf("pickAudio(no prefs) = %d, want the default-flagged track", got)
	}
}

func TestDecisionVideoBitrateRefinesSourceCapabilities(t *testing.T) {
	caps := core.ClientCaps{MaxBitrateKbps: 8000}
	applyDecisionQuery(&caps, url.Values{"videoBitrate": {"2000"}, "maxVideoBitrate": {"4000"}})
	if caps.MaxBitrateKbps != 2000 {
		t.Fatalf("resolver would ignore requested output bitrate: %+v", caps)
	}
	applyDecisionQuery(&caps, url.Values{"videoBitrate": {"12000"}})
	if caps.MaxBitrateKbps != 2000 {
		t.Fatal("query raised an existing limit")
	}
}

func TestAndroidMobileClientRuleOverridesDefault1080pCap(t *testing.T) {
	q := config.DefaultQuality()
	// Default: no Android mobile rule -> capped at 1080p.
	pr := &Request{Product: "Plex for Android (Mobile)", Platform: "Android", Device: "Pixel 8"}
	base := resolver.ApplyClientRules(q, pr.ClientCaps(0), nil)
	if base.MaxHeight != 1080 || base.HDR10 {
		t.Fatalf("default Android mobile caps = %+v, want MaxHeight=1080 HDR10=false", base)
	}
	prof := resolver.SelectProfile(q, base, nil)
	if prof.MaxHeight != 1080 {
		t.Fatalf("default Android mobile profile MaxHeight = %d, want 1080", prof.MaxHeight)
	}

	// Rule assigning 4K profile to Plex for Android (Mobile) lifts ceiling to 2160p + HDR10.
	q.Rules = append(q.Rules, config.QualityRule{Product: "Plex for Android (Mobile)", Profile: "4K"})
	lifted := resolver.ApplyClientRules(q, pr.ClientCaps(0), nil)
	if lifted.MaxHeight != 2160 || !lifted.HDR10 {
		t.Fatalf("rule-overridden Android mobile caps = %+v, want MaxHeight=2160 HDR10=true", lifted)
	}
	prof4K := resolver.SelectProfile(q, pr.ClientCaps(0), nil)
	if prof4K.MaxHeight != 2160 || !prof4K.AllowHDR {
		t.Fatalf("rule-overridden Android mobile profile = %+v, want MaxHeight=2160 AllowHDR=true", prof4K)
	}

	// Explicit client rule MaxHeight=1440 or 2160 on a 1080p profile is respected.
	qCap := config.DefaultQuality()
	qCap.Rules = []config.QualityRule{{Product: "Plex for Android (Mobile)", Profile: "4K", MaxHeight: 720}}
	capped := resolver.ApplyClientRules(qCap, pr.ClientCaps(0), nil)
	if capped.MaxHeight != 720 {
		t.Fatalf("explicit rule MaxHeight caps = %+v, want 720", capped)
	}

	// Per-request explicit video.height limitation from the client still wins over the rule.
	prLimited := &Request{
		Product:      "Plex for Android (Mobile)",
		Platform:     "Android",
		Device:       "Pixel 8",
		ProfileExtra: "add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.height&value=720)",
	}
	reqLimited := resolver.ApplyClientRules(q, prLimited.ClientCaps(0), nil)
	if reqLimited.MaxHeight != 720 {
		t.Fatalf("per-request limitation should cap at 720, got %+v", reqLimited)
	}
}

func TestRemote4KProfileClearsDefault8MbpsRemoteBitrateCap(t *testing.T) {
	q := config.DefaultQuality()
	q.Remote = "4K"
	pr := &Request{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", Remote: true}
	raw := pr.ClientCaps(8000)
	if raw.MaxBitrateKbps != 8000 || !raw.DefaultRemoteBitrate {
		t.Fatalf("raw remote caps = %+v", raw)
	}
	applied := resolver.ApplyClientRules(q, raw, nil)
	if applied.MaxBitrateKbps != 0 || applied.DefaultRemoteBitrate {
		t.Fatalf("4K remote profile should clear default 8000 kbps cap, got %+v", applied)
	}

	// Explicit client bitrate limit is preserved even on a 4K profile.
	explicit := pr.ClientCaps(8000)
	applyDecisionQuery(&explicit, url.Values{"maxVideoBitrate": {"8000"}})
	appliedExplicit := resolver.ApplyClientRules(q, explicit, nil)
	if appliedExplicit.MaxBitrateKbps != 8000 {
		t.Fatalf("explicit client maxVideoBitrate should be preserved, got %+v", appliedExplicit)
	}
}
