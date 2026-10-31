package core

import (
	"strconv"
	"strings"
	"time"
)

// ClassifyDevice maps a Plex client's X-Plex-Product, X-Plex-Platform and
// X-Plex-Device headers onto a hub form factor (DeviceTV, DeviceMobile,
// DeviceWeb, DeviceDesktop). Playback codecs, HDR and subtitle support are
// not decided here; those come from the client capability registry.
//
//   - TV: Apple TV, Android TV / Google TV / Fire TV / Shield, Roku, smart
//     TVs (Samsung Tizen, LG webOS, Vizio, Hisense, Sony...), game consoles
//     (Xbox, PlayStation) and set-top boxes.
//   - Desktop: Plex HTPC, Plex for Windows/Mac/Linux, Plex Media Player.
//   - Web: Plex Web in any browser.
//   - Mobile: Plex for iOS / Android phones and tablets.
//
// Unknown clients fall back on their platform, then on DeviceWeb.
func ClassifyDevice(product, platform, device string) string {
	p := strings.ToLower(strings.TrimSpace(product))
	pl := strings.ToLower(strings.TrimSpace(platform))
	d := strings.ToLower(strings.TrimSpace(device))
	all := p + " | " + pl + " | " + d
	has := func(s string, subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}

	// Desktop apps that are TV-shaped (HTPC) are still desktops: the owner
	// asked for HTPC to count as desktop.
	if has(p, "htpc", "plex media player", "plex for windows", "plex for mac", "plex for linux", "plex desktop") {
		return DeviceDesktop
	}
	// Living-room devices.
	if has(all, "apple tv", "appletv", "tvos", "android tv", "androidtv", "google tv", "fire tv", "firetv",
		"(tv)", "shield", "roku", "tizen", "webos", "netcast", "vizio", "smartcast", "hisense", "vidaa",
		"xbox", "playstation", "ps4", "ps5", "chromecast", "tivo", "xfinity", "comcast", "bravia",
		"smart tv", "smarttv", "plex for samsung", "plex for lg", "panasonic", "philips", "toshiba") {
		return DeviceTV
	}
	// Amazon Fire TV model numbers ("AFTMM", "AFTKA") and Android TV boxes.
	if strings.HasPrefix(d, "aft") && len(d) <= 8 {
		return DeviceTV
	}
	if has(p, "plex web") || has(pl, "chrome", "safari", "firefox", "edge", "opera", "web") {
		return DeviceWeb
	}
	if has(all, "ios", "iphone", "ipad", "ipod", "android", "mobile", "tablet") {
		return DeviceMobile
	}
	if has(pl, "windows", "macos", "mac os", "osx", "linux") {
		return DeviceDesktop
	}
	return DeviceWeb
}

// DaypartAt returns the daypart of t in t's own location: morning
// 05:00–11:59, afternoon 12:00–16:59, evening 17:00–21:59, late
// 22:00–04:59.
func DaypartAt(t time.Time) string {
	switch h := t.Hour(); {
	case h >= 5 && h < 12:
		return DaypartMorning
	case h >= 12 && h < 17:
		return DaypartAfternoon
	case h >= 17 && h < 22:
		return DaypartEvening
	default:
		return DaypartLate
	}
}

// IsWeekend reports whether t (in its own location) falls in the weekend
// viewing window: Friday from 17:00 through Sunday. Friday night behaves
// like a weekend night (films, longer sessions), Sunday daytime does too.
func IsWeekend(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return true
	case time.Friday:
		return t.Hour() >= 17
	}
	return false
}

// NewHubContext derives the request context for a hubs call at now (in the
// server's local time; pass time.Now() or a time already converted with
// In(loc)) from the client's X-Plex-* headers.
func NewHubContext(now time.Time, product, platform, device string) HubContext {
	return HubContext{
		Device:   ClassifyDevice(product, platform, device),
		Daypart:  DaypartAt(now),
		Weekend:  IsWeekend(now),
		Now:      now,
		Product:  product,
		Platform: platform,
	}
}

// Bucket is a stable cache key for the context's coarse shape:
// "section/device/daypart/weekday|weekend".
func (hc HubContext) Bucket() string {
	wk := "weekday"
	if hc.Weekend {
		wk = "weekend"
	}
	sec := "home"
	if hc.SectionID > 0 {
		sec = "s" + strconv.Itoa(hc.SectionID)
	}
	return sec + "/" + hc.Device + "/" + hc.Daypart + "/" + wk
}
