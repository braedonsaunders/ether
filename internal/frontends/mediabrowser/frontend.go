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
	r := &route.Router{}
	if err := publicRoutes(r, func() PublicInfo {
		cfg := frontend.Settings{}
		if host.Config != nil {
			cfg = host.Config()
		}
		current := info
		name, ok := cfg["serverName"].(string)
		if ok && name != "" {
			current.ServerName = name
		}
		return current
	}); err != nil {
		return nil, err
	}
	inst := &instance{host: host, info: info, router: r, port: 8096}
	if err := inst.playbackRoutes(); err != nil {
		return nil, err
	}
	if err := inst.mutationRoutes(); err != nil {
		return nil, err
	}
	if err := inst.browseRoutes(); err != nil {
		return nil, err
	}
	if err := inst.routes(); err != nil {
		return nil, err
	}
	return inst, nil
}

type instance struct {
	host   frontend.Host
	info   PublicInfo
	router *route.Router
	port   int
}

func (i *instance) Handler() http.Handler { return StripPrefix("/jellyfin", i.router) }
func (i *instance) Listeners() []frontend.ListenerSpec {
	settings := frontend.Settings{}
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	port := 8096
	if n, ok := settings["port"].(float64); ok && n > 0 && n <= 65535 {
		port = int(n)
	}
	return []frontend.ListenerSpec{{Address: fmt.Sprintf(":%d", port)}, {Shared: true, PathPrefix: "/jellyfin"}}
}
func (i *instance) Discovery() []frontend.Announcer {
	settings := frontend.Settings{}
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	if enabled, ok := settings["discovery"].(bool); ok && !enabled {
		return nil
	}
	port := 8096
	if n, ok := settings["port"].(float64); ok && n > 0 && n <= 65535 {
		port = int(n)
	}
	info := i.info
	if name, ok := settings["serverName"].(string); ok && name != "" {
		info.ServerName = name
	}
	return []frontend.Announcer{&discovery{info: info, port: port, listenPort: 7359}}
}
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: i.info.Version, Conformance: "in progress"}
}
func (i *instance) Close(context.Context) error { return nil }
