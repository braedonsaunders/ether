package plex

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestFireTV4KMaxModelDoesNotUsePhoneHeight(t *testing.T) {
	quality := config.Quality{Default: "4K", Profiles: []core.QualityProfile{{Name: "4K", MaxHeight: 2160, AllowHDR: true, AllowDolbyVision: true}}}
	for _, model := range []string{"AFTKA", "AFTKRT"} {
		for _, header := range []string{"X-Plex-Device", "X-Plex-Model"} {
			t.Run(model+"/"+header, func(t *testing.T) {
				r := httptest.NewRequest("GET", "/library/metadata/1", nil)
				r.Header.Set("X-Plex-Product", "Plex for Android")
				r.Header.Set("X-Plex-Platform", "Android")
				r.Header.Set(header, model)
				caps := ParseRequest(r).ClientCaps(0)
				p := resolver.SelectProfile(quality, caps, nil)
				if p.MaxHeight != 2160 || !p.AllowHDR || p.AllowDolbyVision {
					t.Fatalf("model incorrectly clamped: %+v caps=%+v", p, caps)
				}
				r.Header.Set("X-Plex-Client-Profile-Extra", "add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.height&value=1080)+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)")
				limited := ParseRequest(r).ClientCaps(0)
				if limited.MaxHeight != 1080 || limited.HDR10 {
					t.Fatalf("explicit constraints lost: %+v", limited)
				}
				r.Header.Del("X-Plex-Client-Profile-Extra")
				r.Header.Set("X-Plex-Client-Capabilities", "videoDecoders=h264{resolution:720}")
				if legacy := ParseRequest(r).ClientCaps(8000); legacy.MaxHeight != 720 || legacy.MaxBitrateKbps != 8000 {
					t.Fatalf("legacy/remote limits lost: %+v", legacy)
				}
				caps.Remote = true
				applyDecisionQuery(&caps, url.Values{"videoResolution": {"1280x720"}, "maxVideoBitrate": {"3000"}})
				if caps.MaxHeight != 720 || caps.MaxBitrateKbps != 3000 {
					t.Fatalf("quality constraints lost: %+v", caps)
				}
			})
		}
	}
}

func TestFireTVModelRecognitionDoesNotPromoteHDOrFriendlyNames(t *testing.T) {
	for _, model := range []string{"AFTSS", "AFTSSS", "AFTKA002", "AFTKRT-new", "Pixel 8", ""} {
		pr := Request{Product: "Plex for Android", Platform: "Android", Device: model, Model: model, DeviceName: "Fire TV Stick 4K Max"}
		if c := pr.ClientCaps(0); c.MaxHeight != 1080 || c.HDR10 || c.DolbyVision {
			t.Fatalf("unverified model %q promoted: %+v", model, c)
		}
	}
}
