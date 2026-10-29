package mediabrowser

// These Emby operations are not implemented by the frontend. Playback and
// authorization decisions remain with the Host, not these descriptive fields.
func (i *instance) embyUserDTO(user map[string]any) {
	policy, ok := user["Policy"].(map[string]any)
	if !ok {
		return
	}
	policy["AllowCameraUpload"] = false
	policy["AllowSharingPersonalItems"] = false
	policy["EnableSubtitleDownloading"] = false
}
