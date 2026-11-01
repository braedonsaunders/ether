package mediabrowser

// Ether does not support locking individual view metadata fields.
// Empty image and trailer collections are the absence of those assets, not
// invented artwork.
func (i *instance) embyViewDTO(view map[string]any) {
	view["LockData"] = false
	view["LockedFields"] = []string{}
	view["BackdropImageTags"] = []any{}
	view["ImageTags"] = map[string]any{}
	view["ExternalUrls"] = []any{}
	view["RemoteTrailers"] = []any{}
	view["Taglines"] = []any{}
	view["UserData"] = map[string]any{"IsFavorite": false, "PlaybackPositionTicks": 0, "Played": false}
	if name, ok := view["Name"].(string); ok && view["ForcedSortName"] == nil {
		view["ForcedSortName"] = name
	}
	id, _ := view["Id"].(string)
	if id != "" {
		view["Guid"] = id
		view["PresentationUniqueKey"] = id
		view["Etag"] = embyETag(id)
	}
}
