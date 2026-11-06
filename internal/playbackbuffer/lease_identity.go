package playbackbuffer

import "errors"

// RetainedIdentity describes an actual published entry and the live session
// state at inspection time. It is evidence, never a publication permission.
type RetainedIdentity struct {
	Key    Key
	Format Format
	Size   int64
	State  State
}

// Identity rechecks the pin, entry and current generation under the store lock.
// Old generation leases remain readable through ReadAt but cannot mint fresh
// recovery receipts after a transition. No parsed metadata is trusted here.
func (l *Lease) Identity() (RetainedIdentity, error) {
	if l == nil {
		return RetainedIdentity{}, errors.New("nil lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.s == nil || l.e == nil {
		return RetainedIdentity{}, errors.New("inactive lease")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	e := l.e
	k := e.meta.Key
	ss := l.s.sessions[k.Session]
	if ss == nil || ss.entries[k] != e || e.readers <= 0 || !e.meta.Published || k.Generation != ss.state.Generation || k.SeekEpoch != ss.state.SeekEpoch || e.meta.Size != int64(len(e.data)) {
		return RetainedIdentity{}, errors.New("stale published lease")
	}
	return RetainedIdentity{Key: k, Format: e.meta.Format, Size: e.meta.Size, State: ss.state}, nil
}
