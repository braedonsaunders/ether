package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type deviceKey struct {
	viewer   int64
	protocol frontend.Protocol
	device   string
}

func copyCapabilities(in frontend.SessionCapabilities) frontend.SessionCapabilities {
	out := in
	out.PlayableMediaTypes = slices.Clone(in.PlayableMediaTypes)
	out.SupportedCommands = slices.Clone(in.SupportedCommands)
	out.DeviceProfile = slices.Clone(in.DeviceProfile)
	if in.SupportsMediaControl != nil {
		v := *in.SupportsMediaControl
		out.SupportsMediaControl = &v
	}
	if in.SupportsPersistentIdentifier != nil {
		v := *in.SupportsPersistentIdentifier
		out.SupportsPersistentIdentifier = &v
	}
	if in.SupportsRemoteControl != nil {
		v := *in.SupportsRemoteControl
		out.SupportsRemoteControl = &v
	}
	return out
}
func copySession(s frontend.Session) frontend.Session {
	if s.Capabilities != nil {
		c := copyCapabilities(*s.Capabilities)
		s.Capabilities = &c
	}
	return s
}
func (b *Broker) capabilityKey(ctx context.Context, a frontend.SessionAccess) (deviceKey, error) {
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return deviceKey{}, err
	}
	if a.Owner || a.Protocol == "" || len(a.Protocol) > 128 || a.Device.ID == "" || len(a.Device.ID) > 1024 {
		return deviceKey{}, core.ErrUnauthorized
	}
	return deviceKey{viewer: v.ID, protocol: a.Protocol, device: a.Device.ID}, nil
}

// UpdateCapabilities stores declarations for the authenticated device only.
// URL metadata is never dereferenced by the broker.
func (b *Broker) UpdateCapabilities(ctx context.Context, a frontend.SessionAccess, c frontend.SessionCapabilities) error {
	key, err := b.capabilityKey(ctx, a)
	if err != nil {
		return err
	}
	if len(c.PlayableMediaTypes) > 256 || len(c.SupportedCommands) > 256 || len(c.DeviceProfile) > 128<<10 || (len(c.DeviceProfile) > 0 && !json.Valid(c.DeviceProfile)) || len(c.AppStoreURL) > 4096 || len(c.IconURL) > 4096 {
		return core.ErrNotAvailable
	}
	for _, list := range [][]string{c.PlayableMediaTypes, c.SupportedCommands} {
		for _, v := range list {
			if len(v) > 1024 {
				return core.ErrNotAvailable
			}
		}
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > 256<<10 {
		return core.ErrNotAvailable
	}
	if b.deps.DB != nil {
		_, err = b.deps.DB.ExecContext(ctx, `INSERT INTO sessions_devices(viewer_id,protocol,device_id,capabilities,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(viewer_id,protocol,device_id) DO UPDATE SET capabilities=excluded.capabilities,updated_at=excluded.updated_at`, key.viewer, key.protocol, key.device, string(data), b.deps.Now().UnixMilli())
		if err != nil {
			return err
		}
	} else {
		b.mu.Lock()
		b.devices[key] = copyCapabilities(c)
		b.mu.Unlock()
	}
	b.mu.Lock()
	for _, s := range b.sessions {
		if s.public.ViewerID == key.viewer && s.public.Protocol == key.protocol && s.public.Device.ID == key.device {
			v := copyCapabilities(c)
			s.public.Capabilities = &v
		}
	}
	b.mu.Unlock()
	// Current sessions are snapshots; publish the new metadata for the owning
	// viewer after the durable update, including sessions served on other nodes.
	list, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: a.Viewer})
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.ViewerID == key.viewer && s.Protocol == key.protocol && s.Device.ID == key.device {
			b.publish(ctx, sessionEvent("SessionUpdated", s, nil))
		}
	}
	return nil
}

func (b *Broker) loadCapabilities(ctx context.Context, key deviceKey) (frontend.SessionCapabilities, bool, error) {
	if b.deps.DB == nil {
		b.mu.Lock()
		v, ok := b.devices[key]
		b.mu.Unlock()
		return copyCapabilities(v), ok, nil
	}
	var data string
	err := b.deps.DB.QueryRowContext(ctx, `SELECT capabilities FROM sessions_devices WHERE viewer_id=? AND protocol=? AND device_id=?`, key.viewer, key.protocol, key.device).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return frontend.SessionCapabilities{}, false, nil
	}
	if err != nil {
		return frontend.SessionCapabilities{}, false, err
	}
	var v frontend.SessionCapabilities
	if err = json.Unmarshal([]byte(data), &v); err != nil {
		return frontend.SessionCapabilities{}, false, err
	}
	return v, true, nil
}

func (b *Broker) Capabilities(ctx context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	key, err := b.capabilityKey(ctx, a)
	if err != nil {
		return frontend.SessionCapabilities{}, err
	}
	v, _, err := b.loadCapabilities(ctx, key)
	return v, err
}

func (b *Broker) enrichCapabilities(ctx context.Context, s *frontend.Session) error {
	v, ok, err := b.loadCapabilities(ctx, deviceKey{viewer: s.ViewerID, protocol: s.Protocol, device: s.Device.ID})
	if err != nil {
		return err
	}
	if ok {
		s.Capabilities = &v
	}
	return nil
}
