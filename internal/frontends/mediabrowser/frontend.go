package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// Jellyfin provides the native Jellyfin API on an independent surface.
type Jellyfin struct{ MachineID string }

func (Jellyfin) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "jellyfin", Name: "Jellyfin", Version: "12.4.0", DefaultPort: 8096}
}
func (f Jellyfin) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	if f.MachineID == "" || host.Identity == nil {
		return nil, fmt.Errorf("jellyfin requires server identity and authentication")
	}
	sum := sha256.Sum256([]byte("ether:jellyfin:" + f.MachineID))
	info := PublicInfo{ID: hex.EncodeToString(sum[:16]), ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}
	settings := frontend.Settings{}
	if host.Config != nil {
		settings = host.Config()
	}
	if s, ok := settings["serverName"].(string); ok && s != "" {
		info.ServerName = s
	}
	port := 8096
	if n, ok := settings["port"].(float64); ok && n > 0 && n <= 65535 {
		port = int(n)
	}
	r := &route.Router{}
	if err := PublicRoutes(r, info); err != nil {
		return nil, err
	}
	enabled, _ := settings["discovery"].(bool)
	inst := &instance{host: host, info: info, router: r, port: port, discoveryEnabled: enabled}
	if err := inst.browseRoutes(); err != nil {
		return nil, err
	}
	if err := inst.routes(); err != nil {
		return nil, err
	}
	return inst, nil
}

type instance struct {
	host             frontend.Host
	info             PublicInfo
	router           *route.Router
	port             int
	discoveryEnabled bool
}

func (i *instance) Handler() http.Handler { return StripPrefix("/jellyfin", i.router) }
func (i *instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Address: fmt.Sprintf(":%d", i.port)}, {Shared: true, PathPrefix: "/jellyfin"}}
}
func (i *instance) Discovery() []frontend.Announcer {
	if i.discoveryEnabled {
		return []frontend.Announcer{&discovery{info: i.info, port: i.port}}
	}
	return nil
}
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: i.info.Version, Conformance: "in progress"}
}
func (i *instance) Close(context.Context) error { return nil }
