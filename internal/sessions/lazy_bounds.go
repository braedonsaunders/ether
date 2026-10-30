package sessions

// Durable limits span all protocols. Pressure refuses new capabilities; live
// playback is never selected as an eviction victim. Existing oversized stores
// are charged in full and must drain before admitting more capabilities.
const (
	maxLazyCapabilities       = 8192
	maxLazyViewerCapabilities = 256
	maxLazyBindings           = 1024
	maxLazyViewerBindings     = 128
)

// A placeholder charges source-open flights as well as reusable sessions.
// Caller holds the hash gate; broker.mu serializes different hashes.
func (b *Broker) reserveLazyBinding(hash string, viewer int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	if entry, ok := b.lazySessions[hash]; ok && (entry.id == "" || b.sessions[entry.id] != nil) {
		return false
	}
	count := 0
	for key, entry := range b.lazySessions {
		if entry.id != "" && b.sessions[entry.id] == nil {
			delete(b.lazySessions, key)
			continue
		}
		if entry.viewer == viewer {
			count++
		}
	}
	if len(b.lazySessions) >= maxLazyBindings || count >= maxLazyViewerBindings {
		return false
	}
	if b.lazySessions == nil {
		b.lazySessions = make(map[string]lazySession)
	}
	b.lazySessions[hash] = lazySession{viewer: viewer}
	return true
}

func (b *Broker) releaseLazyBindingFlight(hash string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.lazySessions[hash]; ok && entry.id == "" {
		delete(b.lazySessions, hash)
	}
}
