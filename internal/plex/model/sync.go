package model

// SetSyncProjection preserves a context's advertised capability independently
// from the download queue setting. Authorization is applied by the frontend.
func (m *MediaContainer) SetSyncProjection(allowed bool) {
	m.AllowSync = Ptr(allowed)
	m.syncProjection = true
}

func (m MediaContainer) HasSyncProjection() bool { return m.syncProjection }
