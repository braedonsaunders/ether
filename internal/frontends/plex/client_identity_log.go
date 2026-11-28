package plex

import (
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// maxLoggedClientIdentities bounds the "client identity" log dedupe set.
const maxLoggedClientIdentities = 4096

// loggedClientIdentities remembers which client identities were already
// logged, so each client is reported once per process rather than per play.
var loggedClientIdentities = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

// logClientIdentity records, once per distinct identity, the headers a Plex
// client sent and the height ceiling Ether derived from them. The hardware
// model is the only header that separates clients sharing a device name
// (every Apple TV reports "Apple TV"; X-Plex-Model says AppleTV5,3 for the
// HD box and AppleTV6,2/11,1/14,1 for the 4K ones).
func (s *Server) logClientIdentity(pr *Request, caps frontend.ClientCaps) {
	if s == nil || s.log == nil || pr == nil {
		return
	}
	key := pr.ClientID + "\x00" + pr.Product + "\x00" + pr.Version + "\x00" + pr.Device + "\x00" + pr.DeviceName + "\x00" + pr.Model
	loggedClientIdentities.Lock()
	if _, ok := loggedClientIdentities.seen[key]; ok {
		loggedClientIdentities.Unlock()
		return
	}
	if len(loggedClientIdentities.seen) >= maxLoggedClientIdentities {
		clear(loggedClientIdentities.seen)
	}
	loggedClientIdentities.seen[key] = struct{}{}
	loggedClientIdentities.Unlock()

	user := ""
	if pr.User != nil {
		user = pr.User.Username
	}
	s.log.Info("playback client identity",
		"client", pr.ClientID,
		"user", user,
		"product", pr.Product,
		"version", pr.Version,
		"platform", pr.Platform,
		"platformVersion", pr.PlatformVersion,
		"device", pr.Device,
		"deviceName", pr.DeviceName,
		"model", pr.Model,
		"vendor", pr.Vendor,
		"maxHeight", caps.MaxHeight,
		"maxHeightSource", caps.MaxHeightSource,
		"hdr10", caps.HDR10,
		"dolbyVision", caps.DolbyVision,
		"declaredProfile", pr.ProfileExtra != "" || pr.ClientCapabilities != "",
	)
}
