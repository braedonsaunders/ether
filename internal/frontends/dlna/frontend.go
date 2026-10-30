// Package dlna presents a LAN-only UPnP MediaServer.
package dlna

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

type Frontend struct{ MachineID string }

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "dlna", Name: "DLNA", Version: "1.0", Clients: []string{"DLNA MediaServer"}}
}
func (f Frontend) ValidateSettings(settings frontend.Settings) error {
	for key, value := range settings {
		switch key {
		case "viewerId":
			if id, ok := value.(float64); !ok || math.IsNaN(id) || math.IsInf(id, 0) || id <= 0 || id > 9007199254740991 || id != math.Trunc(id) {
				return errors.New("invalid viewer")
			}
		case "serverName":
			if _, ok := value.(string); !ok {
				return errors.New("invalid server name")
			}
		case "discovery":
			if _, ok := value.(bool); !ok {
				return errors.New("invalid discovery setting")
			}
		default:
			return errors.New("unknown DLNA setting")
		}
	}
	return nil
}
func (f Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if f.MachineID == "" || host.LANViewer == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("dlna requires LAN viewer and host services")
	}
	sum := sha256.Sum256([]byte("ether:dlna:" + f.MachineID))
	i := &instance{host: host, id: hex.EncodeToString(sum[:16]), router: &route.Router{}}
	for _, item := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{http.MethodGet, "/description.xml", i.description},
		{http.MethodGet, "/ContentDirectory/description.xml", i.contentDescription},
		{http.MethodGet, "/ConnectionManager/description.xml", i.connectionDescription},
		{http.MethodPost, "/ContentDirectory/control", i.contentControl},
		{http.MethodPost, "/ConnectionManager/control", i.connectionControl},
		{http.MethodGet, "/sessions/lazy/{tail...}", i.serveLAN},
		{http.MethodHead, "/sessions/lazy/{tail...}", i.serveLAN},
	} {
		if err := i.router.Handle(item.method, item.path, item.handler); err != nil {
			return nil, err
		}
	}
	i.ssdp = &ssdp{host: host, id: i.id}
	return i, nil
}

func (i *instance) serveLAN(w http.ResponseWriter, r *http.Request) {
	if i.host.LANStreams == nil {
		http.NotFound(w, r)
		return
	}
	i.host.LANStreams.ServeHTTP(w, r)
}

type instance struct {
	host   frontend.Host
	id     string
	router *route.Router
	ssdp   *ssdp
}

func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dlna/") {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, "/dlna")
			r.URL = &u
		}
		viewer, err := i.host.LANViewer(r.Context(), r)
		if err != nil || viewer == nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if i.host.ClientSeen != nil && i.host.ClientIP != nil {
			if peer, ok := i.host.ClientIP(r); ok {
				if device := frontend.LANDevice(peer); device.ID != "" {
					i.host.ClientSeen(device.ID)
				}
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer))
		i.router.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/dlna"}}
}
func (i *instance) Discovery() []frontend.Announcer { return []frontend.Announcer{i.ssdp} }
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "UPnP AV 1.0", Conformance: "partial", Warnings: i.ssdp.warnings()}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }
