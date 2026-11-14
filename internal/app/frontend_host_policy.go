package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
)

// frontendHostPolicy is protocol wiring; behavior is declared by the frontend.
func frontendHostPolicy(id core.Protocol) core.FrontendHostPolicy {
	switch id {
	case "plex": // Wire the account-managed frontend's host options.
		return plex.HostPolicy()
	case "jellyfin", "emby": // Wire the two native API frontends' host options.
		return mediabrowser.HostPolicy()
	case "vela":
		return core.FrontendHostPolicy{PublicURL: true, PlaybackTelemetry: true, NativePlaybackCapabilities: true}
	case "silo":
		return core.FrontendHostPolicy{PublicURL: true, InferBuffering: true, PlaybackTelemetry: true, BestEffortLiveStart: true}
	default:
		return core.FrontendHostPolicy{PublicURL: true}
	}
}
