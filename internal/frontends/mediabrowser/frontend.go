package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/conformance"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// Jellyfin provides the native Jellyfin API on an independent surface.
type Jellyfin struct{ MachineID string }

func (Jellyfin) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "jellyfin", Name: "Jellyfin", Version: "12.4.0", DefaultPort: 8096}
}
func (f Jellyfin) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	return mount(ctx, host, f.MachineID, "jellyfin")
}

// Emby shares the MediaBrowser engine but has a separate identity and surface.
type Emby struct{ MachineID string }

func (Emby) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "emby", Name: "Emby", Version: "4.10.2.0", DefaultPort: 8097}
}
func (f Emby) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	return mount(ctx, host, f.MachineID, "emby")
}
func mount(ctx context.Context, host frontend.Host, machineID string, protocol frontend.Protocol) (frontend.Instance, error) {
	if machineID == "" || host.Identity == nil {
		return nil, fmt.Errorf("%s requires server identity and authentication", protocol)
	}
	sum := sha256.Sum256([]byte("ether:" + string(protocol) + ":" + machineID))
	info := PublicInfo{ID: hex.EncodeToString(sum[:16]), ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}
	if protocol == "emby" {
		info.Version = "4.10.2.0"
		info.ProductName = "Emby Server"
	}
	r := &route.Router{}

	life, cancel := context.WithCancel(ctx)
	inst := &instance{host: host, info: info, router: r, protocol: protocol, life: life, cancel: cancel}
	unmatched := &unmatchedLogger{}
	r.SetUnmatched(func(req *http.Request, status int) { unmatched.log(inst, req, status) })
	mounted := false
	defer func() {
		if !mounted {
			cancel()
		}
	}()
	if err := publicRoutes(r, func() PublicInfo {
		current := info
		if host.Config != nil {
			cfg := host.Config()
			if name, ok := cfg["serverName"].(string); ok && name != "" {
				current.ServerName = name
			}
		}
		return current
	}, inst.publicViewers); err != nil {
		return nil, err
	}

	for _, method := range []string{"GET", "HEAD"} {
		if err := r.Handle(method, "/Items/{itemId}/Images/{imageType}", http.HandlerFunc(inst.image)); err != nil {
			return nil, err
		}
		if err := r.Handle(method, "/Items/{itemId}/Images/{imageType}/{index}", http.HandlerFunc(inst.image)); err != nil {
			return nil, err
		}
	}
	for _, method := range []string{"GET", "POST"} {
		if err := r.Handle(method, "/DisplayPreferences/{id}", http.HandlerFunc(inst.displayPreferences)); err != nil {
			return nil, err
		}
	}
	if err := r.Handle("GET", "/LiveTv/Channels", http.HandlerFunc(inst.channels)); err != nil {
		return nil, err
	}
	if err := r.Handle("GET", "/LiveTv/Programs", http.HandlerFunc(inst.programmes)); err != nil {
		return nil, err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if err := r.Handle(method, "/Videos/{id}/{source}/Subtitles/{index}/{name}", http.HandlerFunc(inst.subtitle)); err != nil {
			return nil, err
		}
	}
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
	mounted = true
	return inst, nil
}

type instance struct {
	protocol frontend.Protocol
	life     context.Context
	cancel   context.CancelFunc
	host     frontend.Host
	info     PublicInfo
	router   *route.Router
}

func (i *instance) Handler() http.Handler {
	next := StripPrefix("/"+string(i.protocol), i.router)
	if i.protocol != "emby" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Accept, Accept-Language, Authorization, Cache-Control, Content-Disposition, Content-Encoding, Content-Language, Content-Length, Content-MD5, Content-Range, Content-Type, Date, Host, If-Match, If-Modified-Since, If-None-Match, If-Unmodified-Since, Origin, OriginToken, Pragma, Range, Slug, Transfer-Encoding, Want-Digest, X-MediaBrowser-Token, X-Emby-Token, X-Emby-Client, X-Emby-Client-Version, X-Emby-Device-Id, X-Emby-Device-Name, X-Emby-Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (i *instance) listenerPort() int {
	settings := frontend.Settings{}
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	if n, ok := settings["port"].(float64); ok && n > 0 && n <= 65535 {
		return int(n)
	}
	if i.protocol == "emby" {
		return 8097
	}
	return 8096
}
func (i *instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Address: fmt.Sprintf(":%d", i.listenerPort())}, {Shared: true, PathPrefix: "/" + string(i.protocol)}}
}

// UDP discovery is multiplexed by the host via DiscoveryResponder.
func (i *instance) Discovery() []frontend.Announcer { return nil }
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: i.info.Version, Conformance: conformance.Status(string(i.protocol), i.info.Version)}
}
func (i *instance) Close(context.Context) error { i.cancel(); return nil }
