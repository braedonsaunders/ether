package plex

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// nativeDeviceID derives a stable, synthetic device identity for a clientless
// media read. Native players (Samsung AVPlay) drop every Plex header on media
// URLs and carry only a token, so the request has no X-Plex-Client-Identifier.
// Such a read must still open an accounted session: the identity is keyed on
// the viewer, the credential and the network peer, so repeated range reads from
// the same player reuse one session instead of minting a new one per request,
// while a different viewer, token or address stays distinct.
func nativeDeviceID(viewer int64, token string, remoteIP string) string {
	sum := sha256.Sum256([]byte(token + "\x1f" + remoteIP + "\x1f" + strconv.FormatInt(viewer, 10)))
	return "native-" + hex.EncodeToString(sum[:8])
}

// nativePlatform names the platform of a clientless read from its User-Agent,
// so the session and status surfaces do not show an empty platform.
func nativePlatform(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "tizen"):
		return "Tizen"
	case strings.Contains(ua, "webos"):
		return "webOS"
	case strings.Contains(ua, "android"):
		return "Android"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "apple"):
		return "iOS"
	case strings.Contains(ua, "roku"):
		return "Roku"
	case ua == "":
		return ""
	}
	// The first product token is a coarse but honest fallback.
	first := userAgent
	if i := strings.IndexAny(first, " /"); i > 0 {
		first = first[:i]
	}
	if len(first) > 40 {
		first = first[:40]
	}
	return first
}

// adoptNativeDevice gives a clientless read a synthetic device identity so it
// can prepare and publish through the broker like any other client.
func (pr *Request) adoptNativeDevice(r *http.Request) {
	if pr.ClientID == "" {
		pr.ClientID = nativeDeviceID(pr.User.ID, pr.Token, pr.RemoteIP.String())
	}
	if pr.Product == "" {
		pr.Product = "Native player"
	}
	if pr.Platform == "" {
		pr.Platform = nativePlatform(r.UserAgent())
	}
}
