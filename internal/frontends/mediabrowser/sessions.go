package mediabrowser

import (
	"net/http"
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
	sessions, err := i.host.Sessions.Sessions(r.Context(), frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol, Owner: viewer.IsOwner && !viewer.Delegated})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, i.brokerSessionDTO(viewer, session))
	}
	jsonReply(w, items)
}

func (i *instance) brokerSessionDTO(viewer *frontend.Viewer, session frontend.Session) map[string]any {
	userID, _ := ids.GUID(ids.User, session.ViewerID)
	item := map[string]any{
		"Id": session.ID, "UserId": userID, "ServerId": i.info.ID, "Client": session.Device.Client,
		"DeviceId": session.Device.ID, "DeviceName": session.Device.DeviceName, "ApplicationVersion": session.Device.Version,
		"LastActivityDate": session.StartedAt.UTC(), "PlayableMediaTypes": []string{}, "SupportedCommands": []string{},
		"SupportsRemoteControl": false,
	}
	if session.ViewerID == viewer.ID {
		item["UserName"] = viewer.Username
	}
	if i.protocol == "jellyfin" {
		item["LastPlaybackCheckIn"] = time.Time{}
		item["IsActive"] = true
		item["SupportsMediaControl"] = false
		item["HasCustomDeviceName"] = false
	}
	if session.Capabilities != nil {
		caps := sessionCapabilitiesDTO(*session.Capabilities)
		if i.protocol == "jellyfin" {
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
	return item
}
