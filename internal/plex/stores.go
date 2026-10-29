package plex

import (
	"context"
	"crypto/rand"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// ---------------------------------------------------------------------------
// Stream ids

// streamRef is what a Stream id points at.
type streamRef struct {
	key   core.RatingKey
	index int // position in MediaInfo.Streams when last rendered
	info  core.StreamInfo
	slot  string // identity within the title (see streamSlot)
}

// streamRegistry hands out stable Stream ids. Ids are a hash of (ratingKey,
// stream position) so they survive restarts; the registry resolves them back
// (for /library/streams/{id} and audioStreamID/subtitleStreamID params) and
// probes past the rare collision.
//
// External (sidecar) subtitles hash their source instead: they follow the
// embedded streams, whose count differs between the estimate shown before
// play and the probed release, so a position would name a different stream
// (or none) once playback starts.
type streamRegistry struct {
	mu   sync.Mutex
	byID map[int64]streamRef
}

const maxStreamRefs = 200_000

func newStreamRegistry() *streamRegistry { return &streamRegistry{byID: map[int64]streamRef{}} }

func streamHash(key core.RatingKey, index int) int64 {
	return slotHash(key, strconv.Itoa(index))
}

func slotHash(key core.RatingKey, slot string) int64 {
	h := fnv.New64a()
	h.Write([]byte(strconv.FormatInt(int64(key), 10) + "/" + slot))
	// Keep ids small and positive so every client parses them as int32.
	return int64(h.Sum64()%2_000_000_000) + 1
}

// streamSlot is a stream's identity within its title: its position, or
// for a sidecar with a known source, that source.
func streamSlot(index int, info core.StreamInfo) string {
	if info.External && info.ExternalURL != "" {
		return "external/" + info.ExternalURL
	}
	return strconv.Itoa(index)
}

func (r *streamRegistry) register(key core.RatingKey, index int, info core.StreamInfo) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byID) >= maxStreamRefs {
		r.byID = map[int64]streamRef{}
	}
	slot := streamSlot(index, info)
	id := slotHash(key, slot)
	for {
		ref, ok := r.byID[id]
		if !ok || (ref.key == key && ref.slot == slot) {
			r.byID[id] = streamRef{key: key, index: index, info: info, slot: slot}
			return id
		}
		id++ // collision: probe
	}
}

func (r *streamRegistry) lookup(id int64) (streamRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.byID[id]
	return ref, ok
}

// ---------------------------------------------------------------------------
// Stream selections (PUT /library/parts/{id}?audioStreamID=&subtitleStreamID=)

type selection struct {
	audio       int // position in MediaInfo.Streams
	audioSet    bool
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
	mu sync.Mutex
	m  map[string]*transcodeEntry
}

func newTranscodeTable() *transcodeTable { return &transcodeTable{m: map[string]*transcodeEntry{}} }

func (t *transcodeTable) put(e *transcodeEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.m[e.id]; ok {
		if old.userID != e.userID || old.clientID != e.clientID {
			return
		}
		e.streamToken = old.streamToken
	}
	if e.streamToken == "" {
		e.streamToken = rand.Text()
	}
	cp := *e
	t.m[e.id] = &cp
}

// claim reserves a client-chosen ID before any playback/transcoder work. A
// different viewer or device cannot replace, reuse, stop, or probe that binding.
func (t *transcodeTable) claim(id string, userID int64, clientID string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[id]; ok {
		return e.userID == userID && e.clientID == clientID
	}
	t.m[id] = &transcodeEntry{id: id, userID: userID, clientID: clientID, streamToken: rand.Text(), updatedAt: now}
	return true
}

func (t *transcodeTable) get(id string) (*transcodeEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[id]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// fail records a failed decision on the binding the decision claimed. A
// binding owned by another viewer or device is left alone.
func (t *transcodeTable) fail(id string, userID int64, clientID string, key core.RatingKey, err error, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[id]
	if !ok || e.userID != userID || e.clientID != clientID || err == nil {
		return
	}
	e.failKey, e.failErr, e.failedAt, e.updatedAt = key, err, now, now
}

// takeFailure returns and clears a recent failed decision for this viewer's
// binding and title.
func (t *transcodeTable) takeFailure(id string, userID int64, key core.RatingKey, now time.Time) (error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[id]
	if !ok || e.failErr == nil {
		return nil, false
	}
	err, match := e.failErr, e.userID == userID && e.failKey == key && now.Sub(e.failedAt) < decisionFailureTTL
	if e.userID == userID {
		e.failKey, e.failErr, e.failedAt = 0, nil, time.Time{}
	}
	if !match {
		return nil, false
	}
	return err, true
}

func (t *transcodeTable) touch(id string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[id]; ok {
		e.updatedAt = now
	}
}

func (t *transcodeTable) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, id)
}

// forClient lists the transcode sessions of a client for a title.
func (t *transcodeTable) forClient(userID int64, clientID string, key core.RatingKey) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for id, e := range t.m {
		if e.userID == userID && e.clientID == clientID && (key == 0 || e.key == key) {
			out = append(out, id)
		}
	}
	return out
}

func (t *transcodeTable) expire(now time.Time, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, e := range t.m {
		if now.Sub(e.updatedAt) > ttl {
			delete(t.m, id)
		}
	}
}

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
func (t *transcodeTable) byPlayID(playID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for id, e := range t.m {
		if e.playID == playID && e.started {
			ids = append(ids, id)
		}
	}
	return ids
}
