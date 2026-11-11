package mediabrowser

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// sessionList projects broker-owned sessions without exposing stream grants.
func (i *instance) sessionList(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items, err := i.sessionRows(r.Context(), viewer, device(r), clientAddress(r, i.host.ClientIP))
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonReply(w, items)
}

func (i *instance) sessionRows(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, remote string) ([]any, error) {
	sessions, err := i.host.Sessions.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: i.protocol, Owner: viewer.IsOwner && !viewer.Delegated})
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(sessions))
	for _, session := range sessions {
		item := i.brokerSessionDTO(viewer, session, device.ID, remote)
		if i.protocol == "emby" {
			i.embySessionDTO(ctx, item, session.Device.ID)
		}
		items = append(items, item)
	}
	return items, nil
}

func (i *instance) brokerSessionDTO(viewer *frontend.Viewer, session frontend.Session, requestDeviceID, remote string) map[string]any {
	userID, _ := ids.GUID(ids.User, session.ViewerID)
	item := map[string]any{
		"Id": session.ID, "UserId": userID, "ServerId": i.info.ID, "Client": session.Device.Client,
		"DeviceId": session.Device.ID, "DeviceName": session.Device.DeviceName, "ApplicationVersion": session.Device.Version,
		"LastActivityDate": wireTime(session.StartedAt), "PlayableMediaTypes": []string{}, "SupportedCommands": []string{},
		"SupportsRemoteControl": false, "AdditionalUsers": []any{},
	}
	// RemoteEndPoint is the requesting device's address; Ether records no
	// per-session address, so another viewer's session reports the empty
	// endpoint the sign-in session already uses.
	endpoint := ""
	if remote != "" && requestDeviceID != "" && session.Device.ID == requestDeviceID {
		endpoint = remote
	}
	item["RemoteEndPoint"] = endpoint
	if session.ViewerID == viewer.ID {
		item["UserName"] = viewer.Username
	}
	item["PlayState"] = i.playStateDTO(session)
	if i.protocol == "jellyfin" {
		item["LastPlaybackCheckIn"] = wireTime(time.Time{})
		item["IsActive"] = true
		item["SupportsMediaControl"] = false
		item["HasCustomDeviceName"] = false
		item["NowPlayingQueue"] = []any{}
	}
	if session.Capabilities != nil {
		caps := i.sessionCapabilitiesDTO(*session.Capabilities)
		if i.protocol == "jellyfin" {
			if session.Protocol != "jellyfin" {
				// Another surface's device profile is not a Jellyfin
				// DeviceProfile; a Kotlin client fails the list on it.
				delete(caps, "DeviceProfile")
			}
			item["Capabilities"] = caps
		}
		for _, key := range []string{"PlayableMediaTypes", "SupportedCommands", "SupportsRemoteControl", "SupportsMediaControl"} {
			if i.protocol == "emby" && key == "SupportsMediaControl" {
				continue
			}
			if value, ok := caps[key]; ok {
				item[key] = value
			}
		}
		if session.Capabilities.SupportsRemoteControl != nil {
			item["SupportsRemoteControl"] = *session.Capabilities.SupportsRemoteControl
		}
	}
	if i.protocol == "jellyfin" && item["Capabilities"] == nil {
		item["Capabilities"] = i.sessionCapabilitiesDTO(frontend.SessionCapabilities{})
	}
	if i.protocol == "emby" {
		// Emby has no Capabilities object on a session; it carries the
		// playable media types and commands inline plus playlist counters
		// and the request protocol.
		item["PlaylistIndex"] = 0
		item["PlaylistLength"] = 0
		item["Protocol"] = "HTTP/1.1"
	}
	return item
}

// playStateDTO mirrors a MediaBrowser PlayerStateInfo. Ether's broker exposes
// the delivery decision but not a live position, so the idle shape is emitted
// while nothing plays and the decision fills PlayMethod while it does.
func (i *instance) playStateDTO(session frontend.Session) map[string]any {
	state := map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone"}
	if i.protocol == "jellyfin" {
		state["PlaybackOrder"] = "Default"
	} else {
		state["PlaybackRate"] = 1
		state["Shuffle"] = false
		state["SleepTimerMode"] = "None"
		state["SubtitleOffset"] = 0
	}
	if session.Item != 0 || session.Channel != "" {
		state["CanSeek"] = true
		if session.PlayMethod != "" {
			state["PlayMethod"] = session.PlayMethod
		}
	}
	return state
}

// clientAddress resolves the requesting peer through the host's trusted-proxy
// resolution, falling back to the raw remote address.
func clientAddress(r *http.Request, clientIP func(*http.Request) (netip.Addr, bool)) string {
	if clientIP != nil {
		if ip, ok := clientIP(r); ok {
			return ip.Unmap().String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
