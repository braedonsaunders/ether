package mediabrowser

import "context"

func (i *instance) embySessionDTO(ctx context.Context, session map[string]any, deviceID string) {
	if deviceID == "" {
		return
	}
	id, err := i.host.IDs.Compact(ctx, "emby", "device", deviceID)
	if err == nil && id > 0 {
		session["InternalDeviceId"] = id
	}
}
