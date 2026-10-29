package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// frontendControl persists desired state and reconciles it on each node.
type frontendControl struct {
	registry     *frontend.Registry
	listeners    *frontendListeners
	config       *config.Store
	mu           sync.Mutex
	applied      map[frontend.Protocol]listenerSettings
	clientCount  func(context.Context, frontend.Protocol) (int, bool)
	sharedStatus func(frontend.Protocol, *frontend.Status)
}
type listenerSettings struct{ Port, Discovery any }

func newFrontendControl(r *frontend.Registry, c *config.Store) *frontendControl {
	return &frontendControl{registry: r, listeners: newFrontendListeners(r), config: c, applied: make(map[frontend.Protocol]listenerSettings)}
}
func (f *frontendControl) descriptor(id frontend.Protocol) (frontend.Descriptor, bool) {
	for _, d := range f.registry.Descriptors() {
		if d.ID == id {
			return d, true
		}
	}
	return frontend.Descriptor{}, false
}
func (f *frontendControl) List(ctx context.Context) ([]admin.FrontendInfo, error) {
	cfg := f.config.Get()
	out := make([]admin.FrontendInfo, 0)
	for _, d := range f.registry.Descriptors() {
		enabled := cfg.Frontends[string(d.ID)].Enabled
		if cfg.Frontends == nil && d.ID == "plex" {
			enabled = true
		}
		status := f.listeners.status(ctx, d.ID)
		if f.sharedStatus != nil {
			f.sharedStatus(d.ID, &status)
		}
		if f.clientCount != nil {
			status.Clients24h, status.Clients24hKnown = f.clientCount(ctx, d.ID)
		}
		out = append(out, admin.FrontendInfo{Descriptor: d, Enabled: enabled, Status: status})
	}
	return out, nil
}
func (f *frontendControl) Settings(_ context.Context, id frontend.Protocol) (frontend.Settings, error) {
	if _, ok := f.descriptor(id); !ok {
		return nil, frontend.ErrNotRegistered
	}
	cfg := f.config.Get()
	if id == "plex" {
		return frontend.Settings{"port": float64(cfg.Server.Port), "serverName": cfg.Server.FriendlyName}, nil
	}
	settings := frontend.Settings{}
	for k, v := range cfg.Frontends[string(id)].Settings {
		settings[k] = v
	}
	if port := cfg.FrontendPorts[string(id)]; port > 0 {
		settings["port"] = float64(port)
	} else if port, ok := frontend.DefaultPort(id); ok {
		settings["port"] = float64(port)
	}
	return settings, nil
}
func validateFrontendSettings(s frontend.Settings, protocolValidator bool) error {
	for k, v := range s {
		switch k {
		case "port":
			n, ok := v.(float64)
			if !ok || n < 1 || n > 65535 || math.Trunc(n) != n {
				return errors.New("invalid port")
			}
		case "serverName":
			if _, ok := v.(string); !ok {
				return errors.New("invalid server name")
			}
		case "discovery", "quickConnect":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("invalid %s", k)
			}
		default:
			if !protocolValidator {
				return fmt.Errorf("unknown setting %s", k)
			}
		}
	}
	return nil
}
func (f *frontendControl) PutSettings(_ context.Context, id frontend.Protocol, s frontend.Settings) error {
	if _, ok := f.descriptor(id); !ok {
		return frontend.ErrNotRegistered
	}
	supported, err := f.registry.ValidateSettings(id, s)
	if err != nil {
		return err
	}
	if err := validateFrontendSettings(s, supported); err != nil {
		return err
	}
	if id == "plex" {
		for key := range s {
			if key != "port" && key != "serverName" {
				return fmt.Errorf("unsupported Plex setting %s", key)
			}
		}
		return f.config.Update(func(c *config.Config) error {
			if port, ok := s["port"].(float64); ok {
				c.Server.Port = int(port)
			}
			if name, ok := s["serverName"].(string); ok {
				c.Server.FriendlyName = name
			}
			return nil
		})
	}
	return f.config.Update(func(c *config.Config) error {
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
			c.Frontends["plex"] = config.Frontend{Enabled: true}
		}
		v := c.Frontends[string(id)]
		v.Settings = make(map[string]any, len(s))
		for k, val := range s {
			if k != "port" {
				v.Settings[k] = val
			}
		}
		if c.FrontendPorts == nil {
			c.FrontendPorts = make(map[string]int)
		}
		if port, ok := s["port"].(float64); ok {
			c.FrontendPorts[string(id)] = int(port)
		} else {
			delete(c.FrontendPorts, string(id))
		}
		c.Frontends[string(id)] = v
		return nil
	})
}
func (f *frontendControl) Toggle(_ context.Context, id frontend.Protocol, enabled bool) error {
	if _, ok := f.descriptor(id); !ok {
		return frontend.ErrNotRegistered
	}
	return f.config.Update(func(c *config.Config) error {
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
			if id != "plex" {
				c.Frontends["plex"] = config.Frontend{Enabled: true}
			}
		}
		v := c.Frontends[string(id)]
		v.Enabled = enabled
		c.Frontends[string(id)] = v
		return nil
	})
}

// Reconcile is called on startup and after a shared configuration update.
func (f *frontendControl) Reconcile(ctx context.Context, c config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.registry.Descriptors() {
		desired := c.Frontends[string(d.ID)].Enabled
		if c.Frontends == nil && d.ID == "plex" {
			desired = true
		}
		settings := c.Frontends[string(d.ID)].Settings
		current := listenerSettings{Port: c.FrontendPorts[string(d.ID)], Discovery: settings["discovery"]}
		_, mounted := f.registry.Instance(d.ID)
		if desired && mounted && !reflect.DeepEqual(f.applied[d.ID], current) {
			_ = f.listeners.stop(ctx, d.ID)
			_, mounted = f.registry.Instance(d.ID)
		}
		if desired && !mounted {
			if f.listeners.start(ctx, d.ID) == nil {
				f.applied[d.ID] = current
			}
		} else if !desired && mounted {
			if f.listeners.stop(ctx, d.ID) == nil {
				delete(f.applied, d.ID)
			}
		}
	}
}

var _ admin.FrontendAdmin = (*frontendControl)(nil)
