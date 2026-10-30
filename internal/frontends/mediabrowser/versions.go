package mediabrowser

import (
	"errors"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// versionScope uses the same host-verified network and device policy as playback.
func (i *instance) versionScope(r *http.Request, viewer *frontend.Viewer) (frontend.VersionScope, error) {
	dev := device(r)
	scope := frontend.VersionScope{Viewer: viewer, Device: dev, Protocol: i.protocol}
	if i.host.Capabilities == nil {
		return scope, errors.New("version handles unavailable")
	}
	result, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
	scope.Caps = result.Client
	return scope, err
}

func (i *instance) resolveSource(r *http.Request, scope frontend.VersionScope, handle string) (frontend.RatingKey, string, string, error) {
	if planner, ok := i.host.Sessions.(frontend.DetailPlanner); ok {
		if target, err := planner.ResolveDetailSource(i.identityContext(r), scope, handle); err == nil && target.Item != 0 && target.ETag != "" {
			return target.Item, "detail", target.ETag, nil
		}
	}
	if i.host.VersionHandles != nil {
		if target, err := i.host.VersionHandles.Resolve(i.identityContext(r), scope, handle); err == nil && target.Item != 0 {
			return target.Item, "version", "", nil
		}
	}
	return 0, "", "", frontend.ErrNotFound
}

// The broker accepts both dashed and undashed UUID handles.
func sameVersionHandle(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "-", ""), strings.ReplaceAll(b, "-", ""))
}
