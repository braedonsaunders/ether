package vela

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

// Subscribe only after a viewer opens playback. The broker owns stop and remote
// control events; disabling the frontend cancels every subscription.
func (i *instance) watchSession(viewer *frontend.Viewer, device frontend.Device, id, token string, s *playSession) {
	ctx, cancel := context.WithCancel(i.ctx)
	events, err := i.host.Sessions.Events(ctx, frontend.EventScope{Viewer: viewer, Protocol: "vela", SessionID: id, Device: device})
	if err != nil {
		cancel()
		return
	}
	go func() {
		defer cancel()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				resolver, ok := i.host.Identity.(frontend.DeviceIdentity)
				if !ok {
					return
				}
				_, _, err := resolver.ResolveDevice(ctx, token, device)
				s.mu.Lock()
				ended := !s.ended.IsZero()
				if err != nil && !ended {
					s.ended = time.Now()
					s.plan["state"] = "ended"
					s.publish("ended")
				}
				s.mu.Unlock()
				if err != nil || ended {
					return
				}
			case e, ok := <-events:
				if !ok {
					return
				}
				if e.SessionID != id {
					continue
				}
				s.mu.Lock()
				if !s.ended.IsZero() {
					s.mu.Unlock()
					return
				}
				switch e.Type {
				case "SessionStopped":
					s.plan["state"] = "ended"
					s.ended = time.Now()
					s.publish("ended")
					s.mu.Unlock()
					return
				case "SessionCommand":
					if e.Command != nil {
						switch e.Command.Kind {
						case frontend.CommandPause:
							s.plan["state"] = "paused"
							s.publish("plan")
						case frontend.CommandUnpause:
							s.plan["state"] = "playing"
							s.publish("plan")
						}
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}
