package mediabrowser

// Ether does not support locking individual view metadata fields.
func (i *instance) embyViewDTO(view map[string]any) {
	view["LockData"] = false
	view["LockedFields"] = []string{}
}
