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
	if i.host.Capabilities == nil || i.host.VersionHandles == nil {
		return scope, errors.New("version handles unavailable")
	}
	result, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
	scope.Caps = result.Client
	return scope, err
}

// The broker accepts both dashed and undashed UUID handles.
func sameVersionHandle(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "-", ""), strings.ReplaceAll(b, "-", ""))
}
