package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// defaultMediaSource does not identify a release. The broker is the sole
// authority for compatibility and lazy-stream issuance; failures leave the
// offer without playback claims or a stream URL.
func (i *instance) defaultMediaSource(r *http.Request, viewer *frontend.Viewer, credential string, key frontend.RatingKey) map[string]any {
	source := i.detailMediaSourceDTO(ids.RatingKeyGUID(key), frontend.MediaInfo{Estimated: true})
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, ids.RatingKeyGUID(key), frontend.MediaInfo{Estimated: true})
	}
	if i.protocol == "jellyfin" {
		i.jellyfinMediaSourceDTO(source, frontend.MediaInfo{Estimated: true}, false)
	}
	if i.host.Sessions == nil || i.host.Capabilities == nil || listMediaEstimate(r) {
		return source
	}
	dev := device(r)
	capability, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
	if err != nil {
		return source
	}
	planReq := frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: i.protocol, Item: key, Caps: capability.Client, Intent: frontend.PlayIntent{Estimate: true, Options: frontend.DecideOptions{DirectPlay: true, DirectStream: true}}}
	plan, err := i.host.Sessions.Plan(i.identityContext(r), planReq)
	if err != nil {
		return source
	}
	source["SupportsDirectPlay"], source["SupportsDirectStream"], source["SupportsTranscoding"] = plan.DirectPlay, plan.DirectStream, plan.Transcoding
	if plan.DirectPlay {
		if path := i.lazySourceURL(r, credential, planReq); path != "" {
			source["Path"] = path
		}
	}
	return source
}
