// Package webdav exposes a viewer-scoped, read-only virtual filesystem.
package webdav

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

type Frontend struct{}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "webdav", Name: "Files", Version: "1.0", Clients: []string{"Infuse", "VLC", "Kodi", "rclone"}}
}
func (Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("webdav requires identity, experience and sessions")
	}
	i := &instance{host: host}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i, nil
}

// RegisteredRoutes enumerates the same routes mounted at runtime.
func RegisteredRoutes() ([]route.Route, error) {
	i := &instance{}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i.router.Routes(), nil
}

func (i *instance) registerRoutes() error {
	i.router = &route.Router{}
	for _, method := range []string{"OPTIONS", "PROPFIND", "GET", "HEAD"} {
		if err := i.router.Handle(method, "/{path...}", http.HandlerFunc(i.serve)); err != nil {
			return err
		}
	}
	return nil
}

type instance struct {
	host   frontend.Host
	router *route.Router
}

func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/webdav" {
			http.Redirect(w, r, "/webdav/", http.StatusPermanentRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/webdav/") {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, "/webdav")
			r.URL = &u
		}
		i.router.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/webdav"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "RFC 4918", Conformance: "partial"}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }

func (i *instance) authenticate(r *http.Request) (*frontend.Viewer, frontend.Device, string, error) {
	username, token, ok := r.BasicAuth()
	if !ok || username == "" || token == "" {
		return nil, frontend.Device{}, "", errors.New("missing app password")
	}
	viewer, err := i.host.Identity.Resolve(r.Context(), token)
	if err != nil || viewer == nil || username != strconv.FormatInt(viewer.ID, 10) {
		return nil, frontend.Device{}, "", errors.New("invalid app password")
	}
	sum := sha256.Sum256([]byte(token))
	device := frontend.Device{ID: "webdav-" + hex.EncodeToString(sum[:16]), Client: "WebDAV", Product: "WebDAV"}
	return viewer, device, token, nil
}
func (i *instance) serve(w http.ResponseWriter, r *http.Request) {
	viewer, device, credential, err := i.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="Ether WebDAV", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(device.ID)
	}
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	i.handleFile(w, r, viewer, device, credential)
}
