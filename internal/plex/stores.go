package plex

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// slotHash is the visible id for a subtitle search offer. Offers are not
// stream bindings; playback ids come from the broker.
func slotHash(key core.RatingKey, slot string) int64 { return sessions.PlexWireID(key, slot) }

func streamSlot(index int, info core.StreamInfo) string {
	return sessions.PlexStreamSlot(index, info)
}

// ---------------------------------------------------------------------------
// Stream selections (PUT /library/parts/{id}?audioStreamID=&subtitleStreamID=)

// Selection identity excludes mutable probe/default hints. It includes the
// distinguishing audio/subtitle metadata so an old ID cannot change language
// when a later release reuses the same stream position.
type selection struct {
	audio       int // position in MediaInfo.Streams
	audioSet    bool
	audioStream *core.StreamInfo
	subtitle    int // position, -1 = none
	subtitleSet bool
	// subtitleStream is the chosen subtitle when it was picked by stream
	// id, so the choice is found again in a differently shaped MediaInfo
	// (the estimate before play, the probed release after); see
	// resolveSubtitle.
	subtitleStream *core.StreamInfo
}

type selectionStore struct {
	store *sessions.Selections[selection]
}

func newSelectionStore() *selectionStore {
	return &selectionStore{store: sessions.NewSelections[selection]()}
}
func (s *selectionStore) get(userID, partID int64) selection { return s.store.Get(userID, partID) }
func (s *selectionStore) update(userID, partID int64, fn func(*selection)) {
	s.store.Update(userID, partID, fn)
}

// ---------------------------------------------------------------------------
// Transcode bindings: client transcode session id -> play session/decision

type transcodeEntry struct {
	id          string // client "session" param
	streamToken string // independent random capability; never a client-chosen ID
	playID      string // core.PlaySession.ID
	playbackID  string // explicit X-Plex-Session-Identifier owning this encoder
	userID      int64
	clientID    string
	key         core.RatingKey
	requested   playRequest // original decision/start inputs, before policy refinement
	decision    core.Decision
	info        core.MediaInfo
	subtitle    int // selected subtitle's position in info, -1 = none
	caps        core.ClientCaps
	opts        core.DecideOptions
	offset      time.Duration
	started     bool // StartHLS ran
	updatedAt   time.Time
	// A failed decision for this binding (failKey, failErr at failedAt):
	// the start request that follows it answers with the same error at
	// once instead of resolving again. One-shot and short-lived.
	failKey  core.RatingKey
	failErr  error
	failedAt time.Time
}

// decisionFailureTTL bounds how long a failed decision answers the start
// request that follows it; after that a start resolves afresh.
const decisionFailureTTL = 15 * time.Second

type transcodeTable struct {
	table *sessions.BindingTable[playRequest]
}

func newTranscodeTable() *transcodeTable {
	return &transcodeTable{table: sessions.NewBindingTable[playRequest]()}
}
func (e *transcodeEntry) binding() *sessions.Binding[playRequest] {
	return &sessions.Binding[playRequest]{ID: e.id, StreamToken: e.streamToken, PlayID: e.playID, PlaybackID: e.playbackID, UserID: e.userID, ClientID: e.clientID, Key: e.key, Requested: e.requested, Decision: e.decision, Info: e.info, Subtitle: e.subtitle, Caps: e.caps, Opts: e.opts, Offset: e.offset, Started: e.started, UpdatedAt: e.updatedAt, FailKey: e.failKey, FailErr: e.failErr, FailedAt: e.failedAt}
}
func plexBinding(b *sessions.Binding[playRequest]) *transcodeEntry {
	return &transcodeEntry{id: b.ID, streamToken: b.StreamToken, playID: b.PlayID, playbackID: b.PlaybackID, userID: b.UserID, clientID: b.ClientID, key: b.Key, requested: b.Requested, decision: b.Decision, info: b.Info, subtitle: b.Subtitle, caps: b.Caps, opts: b.Opts, offset: b.Offset, started: b.Started, updatedAt: b.UpdatedAt, failKey: b.FailKey, failErr: b.FailErr, failedAt: b.FailedAt}
}
func (t *transcodeTable) put(e *transcodeEntry) {
	b := e.binding()
	t.table.Put(b)
	e.streamToken = b.StreamToken
}
func (t *transcodeTable) claim(id string, userID int64, clientID string, now time.Time) bool {
	return t.table.Claim(id, userID, clientID, now)
}
func (t *transcodeTable) get(id string) (*transcodeEntry, bool) {
	b, ok := t.table.Get(id)
	if !ok {
		return nil, false
	}
	return plexBinding(b), true
}
func (t *transcodeTable) fail(id string, userID int64, clientID string, key core.RatingKey, err error, now time.Time) {
	t.table.Fail(id, userID, clientID, key, err, now)
}
func (t *transcodeTable) takeFailure(id string, userID int64, key core.RatingKey, now time.Time) (error, bool) {
	return t.table.TakeFailure(id, userID, key, now)
}
func (t *transcodeTable) touch(id string, now time.Time) { t.table.Touch(id, now) }
func (t *transcodeTable) remove(id string)               { t.table.Remove(id) }
func (t *transcodeTable) forClient(userID int64, clientID string, key core.RatingKey) []string {
	return t.table.ForClient(userID, clientID, key)
}
func (t *transcodeTable) expire(now time.Time, ttl time.Duration) { t.table.Expire(now, ttl) }

// ---------------------------------------------------------------------------
// Facet cache: genre/country keys -> titles for filter resolution.

type facetCache struct {
	mu sync.Mutex
	m  map[string]facetEntry
}

type facetEntry struct {
	vals []core.FacetValue
	at   time.Time
}

func newFacetCache() *facetCache { return &facetCache{m: map[string]facetEntry{}} }

const facetTTL = 10 * time.Minute

func (s *Server) facet(ctx context.Context, t core.MediaType, name string) ([]core.FacetValue, error) {
	if name == "firstCharacter" {
		return s.d.Catalog.Facet(ctx, t, name)
	}
	k := string(t) + "/" + name
	s.facets.mu.Lock()
	e, ok := s.facets.m[k]
	s.facets.mu.Unlock()
	if ok && s.now().Sub(e.at) < facetTTL {
		return e.vals, nil
	}
	vals, err := s.d.Catalog.Facet(ctx, t, name)
	if err != nil {
		return nil, err
	}
	s.facets.mu.Lock()
	s.facets.m[k] = facetEntry{vals: vals, at: s.now()}
	s.facets.mu.Unlock()
	return vals, nil
}

// facetTitle maps a filter value (a facet key or already a title) to the
// facet title the catalogue filters on.
func (s *Server) facetTitle(ctx context.Context, t core.MediaType, name, value string) string {
	vals, err := s.facet(ctx, t, name)
	if err != nil {
		return value
	}
	for _, v := range vals {
		if v.Key == value {
			return v.Title
		}
	}
	return value
}

// byPlayID returns the transcode session ids fed by a playback session.
func (t *transcodeTable) byPlayID(playID string) []string { return t.table.ByPlayID(playID) }
func (t *transcodeTable) ids() []string                   { return t.table.IDs() }
