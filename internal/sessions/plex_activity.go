package sessions

import (
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// PlexActivity is one Plex client's playback as the session list shows it.
// The broker is the only store. Cluster rows for these activities use
// protocol plex.
type PlexActivity struct {
	ID                                                     string
	SessionKey                                             int
	User                                                   core.User
	ClientID, Product, Version, Platform, PlatformVersion  string
	Device, DeviceName, Model, Vendor, SessionID, RemoteIP string
	Remote                                                 bool
	Key                                                    core.RatingKey
	State                                                  string
	Offset, Duration                                       time.Duration
	PlayQueueItemID                                        int64
	TranscodeID                                            string
	StartedAt, UpdatedAt                                   time.Time
}

func (b *Broker) HasPlexActivity(id string) bool {
	if b == nil || id == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.plexActivities[id]
	return ok
}

func (b *Broker) EnsurePlexActivity(id string, key int) {
	if b == nil || id == "" || key <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.plexActivities[id]; ok {
		return
	}
	b.plexActivities[id] = &PlexActivity{ID: id, SessionKey: key}
}

func (b *Broker) UpsertPlexActivity(id string, fn func(*PlexActivity)) PlexActivity {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.plexActivities[id]
	if !ok {
		b.plexActivityNext++
		e = &PlexActivity{ID: id, SessionKey: b.plexActivityNext}
		b.plexActivities[id] = e
	}
	if fn != nil {
		fn(e)
	}
	out := *e
	out.User = e.User
	return out
}

func (b *Broker) RemovePlexActivity(id string) (PlexActivity, bool) {
	if b == nil {
		return PlexActivity{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.plexActivities[id]
	if !ok {
		return PlexActivity{}, false
	}
	delete(b.plexActivities, id)
	return *e, true
}

func (b *Broker) ListPlexActivities() []PlexActivity {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PlexActivity, 0, len(b.plexActivities))
	for _, e := range b.plexActivities {
		out = append(out, *e)
	}
	return out
}

func (b *Broker) ExpirePlexActivities(now time.Time, ttl time.Duration) []PlexActivity {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []PlexActivity
	for id, e := range b.plexActivities {
		if now.Sub(e.UpdatedAt) > ttl {
			out = append(out, *e)
			delete(b.plexActivities, id)
		}
	}
	return out
}
