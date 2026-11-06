package state

// HasPendingWatch is a bounded in-memory check (the queue caps at4096 entries).
// Read adapters can retain immediate resume visibility without flushing SQLite.
func (s *State) HasPendingWatch(uid int64) bool {
	if s.pending == nil {
		return false
	}
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	for key := range s.pending.entries {
		if key.user == uid {
			return true
		}
	}
	return false
}
