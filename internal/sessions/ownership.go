package sessions

import (
	"github.com/braedonsaunders/ether/internal/core"
	"slices"
	"sync"
	"time"
)

// playbackOwners records explicit playback identifiers observed on source requests.
// Only a known superseded identifier is stale: clients can omit identifiers or
// use different identifiers on their timeline and media requests.
type playbackOwners struct {
	mu      sync.Mutex
	entries map[playbackOwnerKey]*playbackOwner
}
type playbackOwnerKey struct {
	user    int64
	client  string
	key     core.RatingKey
	surface string
}
type playbackOwner struct {
	current string
	playID  string
	prior   []string
	updated time.Time
}

func newPlaybackOwners() *playbackOwners {
	return &playbackOwners{entries: make(map[playbackOwnerKey]*playbackOwner)}
}

// A decision is an authoritative handoff. A part request can establish direct
// playback, but an old range on the same source cannot undo that handoff.
const playbackOwnerHistoryLimit = 32

func (o *playbackOwners) bind(user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time) {
	o.bindFor("", user, client, key, id, playID, decision, now)
}
func (o *playbackOwners) bindFor(surface string, user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time) {
	if id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	k := playbackOwnerKey{user: user, client: client, key: key, surface: surface}
	e := o.entries[k]
	if e == nil {
		e = &playbackOwner{}
		o.entries[k] = e
	}
	if !decision && e.playID == playID && e.current != id && slices.Contains(e.prior, id) {
		return
	}
	e.prior = slices.DeleteFunc(e.prior, func(v string) bool { return v == id || v == e.current })
	if e.current != "" && e.current != id {
		e.prior = append(e.prior, e.current)
		if len(e.prior) > playbackOwnerHistoryLimit {
			e.prior = e.prior[len(e.prior)-playbackOwnerHistoryLimit:]
		}
	}
	e.current = id
	e.playID = playID
	e.updated = now
}
func (o *playbackOwners) stale(user int64, client string, key core.RatingKey, id string) bool {
	return o.staleFor("", user, client, key, id)
}
func (o *playbackOwners) staleFor(surface string, user int64, client string, key core.RatingKey, id string) bool {
	if id == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.entries[playbackOwnerKey{user: user, client: client, key: key, surface: surface}]
	return e != nil && e.current != "" && e.current != id && slices.Contains(e.prior, id)
}
func (o *playbackOwners) expire(now time.Time, ttl time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, e := range o.entries {
		if now.Sub(e.updated) > ttl {
			delete(o.entries, k)
		}
	}
}

// PlaybackOwners retains the v39 playback handoff and supersession rules.
type PlaybackOwners = playbackOwners

func NewPlaybackOwners() *PlaybackOwners { return newPlaybackOwners() }
func (o *playbackOwners) Bind(user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time) {
	o.bind(user, client, key, id, playID, decision, now)
}
func (o *playbackOwners) Stale(user int64, client string, key core.RatingKey, id string) bool {
	return o.stale(user, client, key, id)
}

// BindFor and StaleFor isolate frontend namespaces even when a client chooses
// an identifier that looks like another protocol's internal client key.
func (o *playbackOwners) BindFor(surface string, user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time) {
	o.bindFor(surface, user, client, key, id, playID, decision, now)
}
func (o *playbackOwners) StaleFor(surface string, user int64, client string, key core.RatingKey, id string) bool {
	return o.staleFor(surface, user, client, key, id)
}
func (o *playbackOwners) Expire(now time.Time, ttl time.Duration) { o.expire(now, ttl) }
