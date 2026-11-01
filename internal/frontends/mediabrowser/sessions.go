package mediabrowser

import (
	"context"
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
	items, err := i.sessionRows(r.Context(), viewer, device(r))
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonReply(w, items)
}

func (i *instance) sessionRows(ctx context.Context, viewer *frontend.Viewer, device frontend.Device) ([]any, error) {
	sessions, err := i.host.Sessions.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: i.protocol, Owner: viewer.IsOwner && !viewer.Delegated})
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(sessions))
	for _, session := range sessions {
		item := i.brokerSessionDTO(viewer, session)
		if i.protocol == "emby" {
			i.embySessionDTO(ctx, item, session.Device.ID)
		}
		items = append(items, item)
	}
	return items, nil
}

func (i *instance) brokerSessionDTO(viewer *frontend.Viewer, session frontend.Session) map[string]any {
	userID, _ := ids.GUID(ids.User, session.ViewerID)
	item := map[string]any{
		"Id": session.ID, "UserId": userID, "ServerId": i.info.ID, "Client": session.Device.Client,
		"DeviceId": session.Device.ID, "DeviceName": session.Device.DeviceName, "ApplicationVersion": session.Device.Version,
		"LastActivityDate": wireTime(session.StartedAt), "PlayableMediaTypes": []string{}, "SupportedCommands": []string{},
		"SupportsRemoteControl": false,
	}
	if session.ViewerID == viewer.ID {
		item["UserName"] = viewer.Username
	}
	if i.protocol == "jellyfin" {
		item["LastPlaybackCheckIn"] = wireTime(time.Time{})
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
