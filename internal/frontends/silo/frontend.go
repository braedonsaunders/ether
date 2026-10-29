// Package silo translates Silo's native API into the frontend host contracts.
package silo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/conformance"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// Frontend is the independent Silo surface.
type Frontend struct{ MachineID string }

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "silo", Name: "Silo", DefaultPort: 8090}
}
func (f Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if f.MachineID == "" || host.Identity == nil {
		return nil, errors.New("silo requires server identity and authentication")
	}
	sum := sha256.Sum256([]byte("ether:silo:" + f.MachineID))
	i := &instance{host: host, id: hex.EncodeToString(sum[:16]), router: &route.Router{}}
	if err := i.publicRoutes(); err != nil {
		return nil, err
	}
	if err := i.imageRoutes(); err != nil {
		return nil, err
	}
	if err := i.eventRoutes(); err != nil {
		return nil, err
	}
	if err := i.authRoutes(); err != nil {
		return nil, err
	}
	if err := i.playbackRoutes(); err != nil {
		return nil, err
	}
	if err := i.catalogRoutes(); err != nil {
		return nil, err
	}
	if err := i.mutationRoutes(); err != nil {
		return nil, err
	}
	if err := i.notInterestedRoutes(); err != nil {
		return nil, err
	}
	if err := i.settingsRoutes(); err != nil {
		return nil, err
	}
	if err := i.browseRoutes(); err != nil {
		return nil, err
	}
	return i, nil
}

type instance struct {
	host   frontend.Host
	id     string
	router *route.Router
}

func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/silo/") {
			r2 := r.Clone(context.WithValue(r.Context(), sharedPrefix{}, true))
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, "/silo")
			r2.URL = &u
			i.router.ServeHTTP(w, r2)
			return
		}
		i.router.ServeHTTP(w, r)
	})
}
func (i *instance) Listeners() []frontend.ListenerSpec {
	port := 8090
	if i.host.Config != nil {
		if n, ok := i.host.Config()["port"].(float64); ok && n >= 1 && n <= 65535 && n == float64(int(n)) {
			port = int(n)
		}
	}
	return []frontend.ListenerSpec{{Address: fmt.Sprintf(":%d", port)}, {Shared: true, PathPrefix: "/silo"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "2", Conformance: conformance.Status("silo", "2")}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }
