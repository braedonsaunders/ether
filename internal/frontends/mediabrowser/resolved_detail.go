package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// applyResolvedDetail is only called for one explicitly requested leaf. The
// broker owns the deadline, policy checks, single-flight and source lifetime.
// On failure the caller retains its generic, unknown source without guessing.
func (i *instance) applyResolvedDetail(r *http.Request, viewer *frontend.Viewer, credential string, key frontend.RatingKey, version string, dto map[string]any) {
	planner, ok := i.host.Sessions.(frontend.DetailPlanner)
	if !ok || i.host.Capabilities == nil || i.host.LazyStreams == nil {
		return
	}
	dev := device(r)
	capability, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
	if err != nil {
		return
	}
	request := frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: i.protocol, Item: key, Version: version, Caps: capability.Client, Intent: frontend.PlayIntent{ExactVersion: version != "", Options: frontend.DecideOptions{DirectPlay: true, DirectStream: true}}}
	if version != "" {
		scope := frontend.VersionScope{Viewer: viewer, Device: dev, Protocol: i.protocol, Caps: capability.Client}
		item, kind, tag, err := i.resolveSource(r, scope, version)
		if err != nil || item != key {
			return
		}
		if kind == "detail" {
			request.Version, request.Intent.ExactVersion, request.DetailSource, request.ExpectedSourceETag = "", false, version, tag
		}
	}
	plan, err := planner.DetailPlan(i.identityContext(r), request)
	if err != nil || plan.Estimated || plan.Media.Estimated || plan.Media.Container == "" || plan.DetailSource == "" || plan.ETag == "" {
		return
	}
	id := plan.DetailSource
	request.DetailSource, request.ExpectedSourceETag = plan.DetailSource, plan.ETag
	request.Version, request.Intent.ExactVersion = "", false
	source := i.mediaSourceDTO(id, plan.Media)
	source["Protocol"], source["IsRemote"] = "File", false
	source["SupportsDirectPlay"], source["SupportsDirectStream"], source["SupportsTranscoding"] = plan.DirectPlay, plan.DirectStream, plan.Transcoding
	// Each delivery claim requires its own scoped grant. HLS eligibility is
	// advisory: capacity and current source/policy are rechecked on GET.
	source["SupportsDirectStream"], source["SupportsTranscoding"] = false, false
	if plan.ETag != "" {
		source["ETag"] = plan.ETag
	}
	if plan.DirectPlay {
		if path := i.lazySourceURL(r, credential, request); path != "" {
			source["Path"] = path
		} else {
			source["SupportsDirectPlay"] = false
		}
	}
	if container := hlsContainerDTO(plan); container != "" {
		if path := i.lazyHLSSourceURL(r, credential, request); path != "" {
			source["TranscodingUrl"] = path
			source["TranscodingSubProtocol"], source["TranscodingContainer"] = "hls", container
			source["SupportsDirectStream"], source["SupportsTranscoding"] = plan.DirectStream, plan.Transcoding
		}
	}
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, ids.RatingKeyGUID(key), plan.Media)
	}
	// The selected default precedes other scoped alternatives. Remove any
	// generic placeholder, retaining opaque exact-version entries.
	sources := []any{source}
	if existing, ok := dto["MediaSources"].([]any); ok {
		for _, entry := range existing {
			m, valid := entry.(map[string]any)
			if valid && m["Id"] != id && m["Id"] != ids.RatingKeyGUID(key) {
				sources = append(sources, m)
			}
		}
	}
	dto["MediaSources"] = sources
	dto["MediaStreams"] = i.mediaStreams(plan.Media.Streams)
	dto["Container"] = plan.Media.Container
	if plan.Media.Duration > 0 {
		dto["RunTimeTicks"] = plan.Media.Duration.Nanoseconds() / 100
	}
	if i.protocol != "jellyfin" {
		if plan.Media.Size > 0 {
			dto["Size"] = plan.Media.Size
		}
		if plan.Media.BitrateKbps > 0 {
			dto["Bitrate"] = plan.Media.BitrateKbps * 1000
		}
	}
	if plan.Media.Video() != nil {
		dto["VideoType"] = "VideoFile"
	}
	if i.protocol == "jellyfin" {
		dto["EnableMediaSourceDisplay"] = len(sources) > 1
	}
}
