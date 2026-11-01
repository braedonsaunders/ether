package sessions

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type Binding[R any] struct {
	ID          string // client "session" param
	StreamToken string // independent random capability; never a client-chosen ID
	PlayID      string // core.PlaySession.ID
	PlaybackID  string // explicit X-Plex-Session-IDentifier owning this encoder
	UserID      int64
	ClientID    string
	Key         core.RatingKey
	Requested   R // original Decision/start inputs, before policy refinement
	Decision    core.Decision
	Info        core.MediaInfo
	Subtitle    int // selected Subtitle's position in Info, -1 = none
	Caps        core.ClientCaps
	Opts        core.DecideOptions
	Offset      time.Duration
	Started     bool // StartHLS ran
	UpdatedAt   time.Time
	// A failed Decision for this binding (FailKey, FailErr at FailedAt):
	// the start request that follows it answers with the same error at
	// once instead of resolving again. One-shot and short-lived.
	FailKey  core.RatingKey
	FailErr  error
	FailedAt time.Time
}

// decisionFailureTTL bounds how long a failed Decision answers the start
// request that follows it; after that a start resolves afresh.
const decisionFailureTTL = 15 * time.Second

type BindingTable[R any] struct {
	mu sync.Mutex
	m  map[string]*Binding[R]
}

func NewBindingTable[R any]() *BindingTable[R] { return &BindingTable[R]{m: map[string]*Binding[R]{}} }

func (t *BindingTable[R]) Put(e *Binding[R]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.m[e.ID]; ok {
		if old.UserID != e.UserID || old.ClientID != e.ClientID {
			return
		}
		e.StreamToken = old.StreamToken
	}
	if e.StreamToken == "" {
		e.StreamToken = rand.Text()
	}
	cp := *e
	t.m[e.ID] = &cp
}

// Claim reserves a client-chosen ID before any playback/transcoder work. A
// different viewer or device cannot replace, reuse, stop, or probe that binding.
func (t *BindingTable[R]) Claim(ID string, UserID int64, ClientID string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[ID]; ok {
		return e.UserID == UserID && e.ClientID == ClientID
	}
	t.m[ID] = &Binding[R]{ID: ID, UserID: UserID, ClientID: ClientID, StreamToken: rand.Text(), UpdatedAt: now}
	return true
}

func (t *BindingTable[R]) Get(ID string) (*Binding[R], bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// Fail records a failed Decision on the binding the Decision claimed. A
// binding owned by another viewer or device is left alone.
func (t *BindingTable[R]) Fail(ID string, UserID int64, ClientID string, Key core.RatingKey, err error, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok || e.UserID != UserID || e.ClientID != ClientID || err == nil {
		return
	}
	e.FailKey, e.FailErr, e.FailedAt, e.UpdatedAt = Key, err, now, now
}

// TakeFailure returns and clears a recent failed Decision for this viewer's
// binding and title.
func (t *BindingTable[R]) TakeFailure(ID string, UserID int64, Key core.RatingKey, now time.Time) (error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok || e.FailErr == nil {
		return nil, false
	}
	err, match := e.FailErr, e.UserID == UserID && e.FailKey == Key && now.Sub(e.FailedAt) < decisionFailureTTL
	if e.UserID == UserID {
		e.FailKey, e.FailErr, e.FailedAt = 0, nil, time.Time{}
	}
	if !match {
		return nil, false
	}
	return err, true
}

func (t *BindingTable[R]) Touch(ID string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[ID]; ok {
		e.UpdatedAt = now
	}
}

func (t *BindingTable[R]) Remove(ID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, ID)
}

// ForClient lists the transcode sessions of a client for a title.
func (t *BindingTable[R]) ForClient(UserID int64, ClientID string, Key core.RatingKey) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for ID, e := range t.m {
		if e.UserID == UserID && e.ClientID == ClientID && (Key == 0 || e.Key == Key) {
			out = append(out, ID)
		}
	}
	return out
}

func (t *BindingTable[R]) Expire(now time.Time, ttl time.Duration) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var gone []string
	for ID, e := range t.m {
		if now.Sub(e.UpdatedAt) > ttl {
			delete(t.m, ID)
			gone = append(gone, ID)
		}
	}
	return gone
}

// IDs lists every binding this table still holds.
func (t *BindingTable[R]) IDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.m))
	for id := range t.m {
		ids = append(ids, id)
	}
	return ids
}

// ByPlayID returns the transcode session ids fed by a playback session.
func (t *BindingTable[R]) ByPlayID(PlayID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for ID, e := range t.m {
		if e.PlayID == PlayID && e.Started {
			ids = append(ids, ID)
		}
	}
	return ids
}
