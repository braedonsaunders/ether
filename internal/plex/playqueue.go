package plex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Play queues are what every Plex app plays from: the app POSTs
// /playQueues with a content URI, then plays the selected item and walks
// the queue for "next". They are per-device and short-lived, so a
// standalone server keeps them in memory. A clustered node stores them in
// plex_playqueues (read-through/write-through, with this map as a hot
// cache) so a client that moves to another node keeps its queue: IDs come
// from the table's identity column and the plex_counters row, edits are
// optimistic (a row revision, retried on conflict) and other nodes drop
// their cached copy when told over the bus.

type pqItem struct {
	id    int64
	key   core.RatingKey
	added bool // explicit Up Next addition, distinct from original queue members
}

type playQueue struct {
	id               int64
	userID           int64
	clientID         string
	items            []pqItem
	original         []pqItem // order before shuffle
	selected         int      // index into items
	shuffled         bool
	sourceURI        string
	playlistID       core.RatingKey
	playlistTitle    string
	version          int
	lastAdded        int64
	updatedAt        time.Time
	suppressItem     int64
	suppressRows     bool // mutation response only
	suppressMetadata bool
	forceOffsetZero  bool
	liveSession      string // live queues resolve through LiveTV, never the catalogue

	rev      int64     // clustered: stored row revision
	loadedAt time.Time // clustered: when the cached copy was read or written
}

type playQueueStore struct {
	mu     sync.Mutex
	nextID int64
	nextIt int64
	queues map[int64]*playQueue // standalone: the queues; clustered: a cache
	cl     *clusterState        // nil on a standalone server
	now    func() time.Time
}

const maxQueues = 2000

// queueCacheTTL bounds how long a clustered node trusts its cached copy of
// a queue without re-reading it (bus invalidations normally refresh it
// sooner).
const queueCacheTTL = 30 * time.Second

// maxQueueAttempts bounds optimistic retries of one clustered edit.
const maxQueueAttempts = 6

func newPlayQueueStore() *playQueueStore {
	return &playQueueStore{nextID: 1, nextIt: 1, queues: map[int64]*playQueue{}, now: time.Now}
}

func (s *playQueueStore) newItemsLocked(keys []core.RatingKey) []pqItem {
	out := make([]pqItem, len(keys))
	for i, k := range keys {
		out[i] = pqItem{id: s.nextIt, key: k}
		s.nextIt++
	}
	return out
}

// preallocate reserves item IDs for keys before an edit on a clustered
// node (nil on a standalone server, which numbers items inside the edit).
func (s *playQueueStore) preallocate(keys []core.RatingKey) ([]pqItem, error) {
	if s.cl == nil || len(keys) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	first, err := s.cl.counter(ctx, "playqueue_item", len(keys))
	if err != nil {
		return nil, err
	}
	out := make([]pqItem, len(keys))
	for i, k := range keys {
		out[i] = pqItem{id: first + int64(i), key: k}
	}
	return out, nil
}

// createWith creates a queue of keys for the user's client, applies fn to it
// and returns a snapshot.
func (s *playQueueStore) createWith(userID int64, clientID string, keys []core.RatingKey, now time.Time, fn func(q *playQueue)) (playQueue, error) {
	if s.cl != nil {
		return s.createShared(userID, clientID, keys, now, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireClientLocked(userID, clientID, 0)
	if len(s.queues) >= maxQueues {
		s.evictOldestLocked()
	}
	q := &playQueue{id: s.nextID, userID: userID, clientID: clientID, items: s.newItemsLocked(keys), version: 1, updatedAt: now}
	s.nextID++
	s.queues[q.id] = q
	if fn != nil {
		fn(q)
	}
	return q.snapshot(), nil
}

// A client owns one active queue; an absent identifier is still an identity.
func (s *playQueueStore) retireClientLocked(userID int64, clientID string, keep int64) {
	for id, q := range s.queues {
		if id != keep && q.userID == userID && q.clientID == clientID {
			delete(s.queues, id)
		}
	}
}

func queueFlag(q url.Values, name string) (bool, bool) {
	if !q.Has(name) {
		return false, true
	}
	raw := q.Get(name)
	digits := strings.TrimPrefix(raw, "+")
	if digits == "" {
		return false, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false, false
		}
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	return n == 1, err == nil && n <= 1
}

func (s *playQueueStore) evictOldestLocked() {
	var oldest *playQueue
	for _, q := range s.queues {
		if oldest == nil || q.updatedAt.Before(oldest.updatedAt) ||
			(q.updatedAt.Equal(oldest.updatedAt) && q.id < oldest.id) {
			oldest = q
		}
	}
	if oldest != nil {
		delete(s.queues, oldest.id)
	}
}

// with runs fn on the queue and returns a snapshot (fn nil: read only).
func (s *playQueueStore) with(id, userID int64, now time.Time, fn func(q *playQueue)) (playQueue, bool) {
	if s.cl != nil {
		return s.withShared(id, userID, now, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok || q.userID != userID {
		return playQueue{}, false
	}
	if fn != nil {
		fn(q)
		q.updatedAt = now
	}
	return q.snapshot(), true
}

func (q *playQueue) snapshot() playQueue {
	cp := *q
	cp.items = append([]pqItem(nil), q.items...)
	cp.original = append([]pqItem(nil), q.original...)
	return cp
}

func (s *playQueueStore) get(id, userID int64) (playQueue, bool) {
	return s.with(id, userID, time.Time{}, nil)
}

// selectItem marks the item a client reports playing (timeline).
func (s *playQueueStore) selectItem(userID, itemID int64) {
	if s.cl != nil {
		s.selectShared(userID, itemID)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queues {
		if q.userID != userID {
			continue
		}
		for i, it := range q.items {
			if it.id == itemID {
				q.selected = i
				q.updatedAt = s.now()
				return
			}
		}
	}
}

func (s *playQueueStore) expire(now time.Time, ttl time.Duration) {
	s.mu.Lock()
	for id, q := range s.queues {
		if now.Sub(q.updatedAt) > ttl {
			delete(s.queues, id)
		}
	}
	s.mu.Unlock()
	if s.cl != nil {
		// Idempotent on every node; no leader needed.
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		if _, err := s.cl.db.ExecContext(ctx, `DELETE FROM plex_playqueues WHERE updated_at<?`, now.Add(-ttl).UnixMilli()); err != nil {
			s.cl.expireFail.Fail(err)
		} else {
			s.cl.expireFail.OK()
		}
	}
}

// addItems appends items for keys (or inserts them after the selected
// item). pre holds IDs preallocated on a clustered node; otherwise the
// caller holds s.mu (it runs inside with) and IDs are numbered here.
func (s *playQueueStore) addItems(q *playQueue, keys []core.RatingKey, pre []pqItem, next bool) {
	items := pre
	if items == nil {
		items = s.newItemsLocked(keys)
	}
	if len(items) == 0 {
		return // a no-op must not invalidate the client's queue version
	}
	items = append([]pqItem(nil), items...)
	for i := range items {
		items[i].added = true
	}
	at := min(q.selected+1, len(q.items))
	if !next {
		for at < len(q.items) && q.items[at].added {
			at++
		}
		q.lastAdded = items[len(items)-1].id
	}
	q.items = append(q.items[:at], append(items, q.items[at:]...)...)
	if q.shuffled {
		q.original = append(q.original, items...)
	}
	q.version++
}

// forget drops a cached queue another node changed.
func (s *playQueueStore) forget(id int64) {
	s.mu.Lock()
	delete(s.queues, id)
	s.mu.Unlock()
}

// dropCache forgets every cached queue (bus resync).
func (s *playQueueStore) dropCache() {
	if s.cl == nil {
		return
	}
	s.mu.Lock()
	s.queues = map[int64]*playQueue{}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Clustered storage

type storedItem struct {
	ID    int64          `json:"i"`
	Key   core.RatingKey `json:"k"`
	Added bool           `json:"a,omitempty"`
}

type storedQueue struct {
	ClientID      string         `json:"clientID,omitempty"`
	Items         []storedItem   `json:"items"`
	Original      []storedItem   `json:"original,omitempty"`
	Selected      int            `json:"selected"`
	Shuffled      bool           `json:"shuffled,omitempty"`
	SourceURI     string         `json:"sourceURI,omitempty"`
	PlaylistID    core.RatingKey `json:"playlistID,omitempty"`
	PlaylistTitle string         `json:"playlistTitle,omitempty"`
	Version       int            `json:"version"`
	LastAdded     int64          `json:"lastAdded,omitempty"`
	LiveSession   string         `json:"liveSession,omitempty"`
}

func toStored(items []pqItem) []storedItem {
	out := make([]storedItem, len(items))
	for i, it := range items {
		out[i] = storedItem{ID: it.id, Key: it.key, Added: it.added}
	}
	return out
}

func fromStored(items []storedItem) []pqItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]pqItem, len(items))
	for i, it := range items {
		out[i] = pqItem{id: it.ID, key: it.Key, added: it.Added}
	}
	return out
}

func encodeQueue(q *playQueue) (string, error) {
	b, err := json.Marshal(storedQueue{ClientID: q.clientID, Items: toStored(q.items), Original: toStored(q.original),
		Selected: q.selected, Shuffled: q.shuffled, SourceURI: q.sourceURI, PlaylistID: q.playlistID,
		PlaylistTitle: q.playlistTitle, Version: q.version, LastAdded: q.lastAdded, LiveSession: q.liveSession})
	return string(b), err
}

func decodeQueue(id, userID, rev, updated int64, data string) (*playQueue, error) {
	var sq storedQueue
	if err := json.Unmarshal([]byte(data), &sq); err != nil {
		return nil, err
	}
	return &playQueue{id: id, userID: userID, clientID: sq.ClientID, items: fromStored(sq.Items),
		original: fromStored(sq.Original), selected: sq.Selected, shuffled: sq.Shuffled, sourceURI: sq.SourceURI,
		playlistID: sq.PlaylistID, playlistTitle: sq.PlaylistTitle, version: sq.Version, lastAdded: sq.LastAdded, liveSession: sq.LiveSession,
		updatedAt: time.UnixMilli(updated), rev: rev}, nil
}

func (s *playQueueStore) cache(q *playQueue) {
	cp := q.snapshot()
	cp.loadedAt = s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queues[cp.id]; !ok && len(s.queues) >= maxQueues {
		s.evictOldestLocked()
	}
	s.queues[cp.id] = &cp
}

func (s *playQueueStore) cached(id int64) (*playQueue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok || s.now().Sub(q.loadedAt) > queueCacheTTL {
		return nil, false
	}
	cp := q.snapshot()
	return &cp, true
}

func (s *playQueueStore) load(ctx context.Context, id int64) (*playQueue, error) {
	var userID, rev, updated int64
	var data string
	if err := s.cl.db.QueryRowContext(ctx, `SELECT user_id, rev, data, updated_at FROM plex_playqueues WHERE id=?`, id).
		Scan(&userID, &rev, &data, &updated); err != nil {
		return nil, queueRowError(err)
	}
	q, err := decodeQueue(id, userID, rev, updated, data)
	if err != nil {
		return nil, err
	}
	s.cache(q)
	return q, nil
}

func (s *playQueueStore) createShared(userID int64, clientID string, keys []core.RatingKey, now time.Time, fn func(q *playQueue)) (playQueue, error) {
	items, err := s.preallocate(keys)
	if err != nil {
		return playQueue{}, err
	}
	q := &playQueue{userID: userID, clientID: clientID, items: items, version: 1, updatedAt: now, rev: 1}
	if fn != nil {
		fn(q)
	}
	data, err := encodeQueue(q)
	if err != nil {
		return playQueue{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	tx, err := s.cl.db.BeginTx(ctx, nil)
	if err != nil {
		return playQueue{}, err
	}
	defer tx.Rollback()
	// Lock a common queue row before deciding which client queue survives.
	// This works with SQLite and PostgreSQL and closes concurrent-create races.
	if _, err = tx.ExecContext(ctx, `UPDATE plex_counters SET value=value WHERE name='playqueue_item'`); err != nil {
		return playQueue{}, err
	}
	if err = tx.QueryRowContext(ctx, `INSERT INTO plex_playqueues(user_id, rev, data, updated_at) VALUES (?, ?, ?, ?) RETURNING id`, userID, q.rev, data, now.UnixMilli()).Scan(&q.id); err != nil {
		return playQueue{}, err
	}
	retired, err := retireQueueClient(ctx, tx, userID, clientID, q.id)
	if err != nil {
		return playQueue{}, err
	}
	if err = tx.Commit(); err != nil {
		return playQueue{}, err
	}
	for _, id := range retired {
		s.forget(id)
		s.cl.publish("invalidate:playqueue", queueEvent{ID: id})
	}
	s.cache(q)
	return q.snapshot(), nil
}

func (s *playQueueStore) withShared(id, userID int64, now time.Time, fn func(q *playQueue)) (playQueue, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbTimeout)
	defer cancel()
	for attempt := 0; attempt < maxQueueAttempts; attempt++ {
		q, ok := s.cached(id)
		if !ok || attempt > 0 {
			var err error
			if q, err = s.load(ctx, id); err != nil {
				if !errors.Is(err, core.ErrNotFound) {
					s.cl.warn("read play queue", err)
				}
				return playQueue{}, false
			}
		}
		if q.userID != userID {
			return playQueue{}, false
		}
		if fn == nil {
			return q.snapshot(), true
		}
		fn(q)
		q.updatedAt = now
		data, err := encodeQueue(q)
		if err != nil {
			return playQueue{}, false
		}
		res, err := s.cl.db.ExecContext(ctx, `UPDATE plex_playqueues SET data=?, rev=?, updated_at=? WHERE id=? AND rev=?`,
			data, q.rev+1, now.UnixMilli(), id, q.rev)
		if err != nil {
			s.cl.warn("save play queue", err)
			return playQueue{}, false
		}
		if n, _ := res.RowsAffected(); n == 0 {
			s.forget(id) // another node edited it (or it expired): re-read and retry
			continue
		}
		q.rev++
		s.cache(q)
		s.cl.publish("invalidate:playqueue", queueEvent{ID: id})
		return q.snapshot(), true
	}
	s.cl.warn("save play queue", errQueueConflict)
	return playQueue{}, false
}

// selectShared records the playing item and periodically renews its idle TTL.
// Repeated timelines for the same item must keep long sessions alive, but
// need not write the shared database on every client heartbeat.
func (s *playQueueStore) selectShared(userID, itemID int64) {
	if itemID <= 0 {
		return
	}
	find := func(q *playQueue) int {
		if q.userID != userID {
			return -1
		}
		return indexOfItem(q.items, itemID)
	}
	var qid int64
	s.mu.Lock()
	for _, q := range s.queues {
		if i := find(q); i >= 0 {
			if q.selected == i && s.now().Sub(q.updatedAt) <= queueCacheTTL {
				s.mu.Unlock()
				return
			}
			qid = q.id
			break
		}
	}
	s.mu.Unlock()
	if qid == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		rows, err := s.cl.db.QueryContext(ctx, `SELECT id, rev, data, updated_at FROM plex_playqueues WHERE user_id=? ORDER BY updated_at DESC LIMIT 50`, userID)
		if err != nil {
			cancel()
			s.cl.warn("find play queue item", err)
			return
		}
		for rows.Next() {
			var id, rev, updated int64
			var data string
			if rows.Scan(&id, &rev, &data, &updated) != nil {
				continue
			}
			q, err := decodeQueue(id, userID, rev, updated, data)
			if err != nil {
				continue
			}
			if i := find(q); i >= 0 {
				s.cache(q)
				if q.selected != i || s.now().Sub(q.updatedAt) > queueCacheTTL {
					qid = id
				}
				break
			}
		}
		if err := rows.Err(); err != nil {
			s.cl.warn("find play queue item", err)
		}
		rows.Close()
		cancel()
		if qid == 0 {
			return
		}
	}
	s.withShared(qid, userID, s.now(), func(q *playQueue) {
		if i := indexOfItem(q.items, itemID); i >= 0 {
			q.selected = i
		}
	})
}

// ---------------------------------------------------------------------------
// Handlers

func (s *Server) handlePlayQueueCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	if pr.User == nil {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	shuffle, valid := queueFlag(q, "shuffle")
	_, repeatValid := queueFlag(q, "repeat")
	if !valid || !repeatValid || !validQueueWindow(q) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if t := q.Get("type"); t != "video" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	var keys []core.RatingKey
	var plKey core.RatingKey
	var plTitle string
	uri := q.Get("uri")
	switch {
	case q.Get("playlistID") != "":
		k, err := core.ParseRatingKey(q.Get("playlistID"))
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		pl, err := s.playlist(ctx, pr.User, k)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		keys, err = s.expandKey(ctx, pr, pl.Key)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		plKey, plTitle = pl.Key, pl.Title
		if uri == "" {
			uri = "library://playlist/item/%2Fplaylists%2F" + pl.Key.String()
		}
	case uri != "":
		var err error
		keys, err = s.continuousKeys(ctx, pr, uri, q.Get("continuous") == "1")
		if path, e := uriPath(uri); e == nil {
			if m := rePlaylistPath.FindStringSubmatch(path); len(m) > 0 {
				if k, e := core.ParseRatingKey(m[1]); e == nil {
					if pl, e := s.playlist(ctx, pr.User, k); e == nil {
						plKey, plTitle = pl.Key, pl.Title
					}
				}
			}
		}
		if err != nil {
			s.log.Debug("play queue uri", "uri", uri, "err", err)
			if errors.Is(err, core.ErrNotFound) {
				writeHTTPError(w, http.StatusNotFound)
			} else {
				writeHTTPError(w, http.StatusBadRequest)
			}
			return
		}
	default:
		writeHTTPError(w, http.StatusBadRequest)
		return
	}

	// A show queue follows regular episodes, including specials explicitly
	// placed inside a regular presented season, but not the Specials season.
	if plKey == 0 {
		if path, err := uriPath(uri); err == nil {
			path, _, _ = strings.Cut(path, "?")
			if m := reMetadataPath.FindStringSubmatch(path); len(m) > 0 {
				if k, err := core.ParseRatingKey(m[1]); err == nil && k.Kind() == core.KindShow {
					filtered := keys[:0]
					for _, item := range keys {
						if !s.queueSpecial(ctx, pr, item) {
							filtered = append(filtered, item)
						}
					}
					keys = filtered
				}
			}
		}
	}

	// Cinema Trailers and the owner's pre-roll: prepend them before the
	// feature and warm the feature while they play. A pre-roll needs no
	// extrasPrefixCount; it never precedes a resume.
	prefixAt := -1
	if !shuffle && len(keys) > 0 {
		n, err := strconv.Atoi(q.Get("extrasPrefixCount"))
		cinema := err == nil && n > 0
		featureIdx := 0
		if start := keyFromPath(q.Get("key")); start != 0 {
			for i, k := range keys {
				if k == start {
					featureIdx = i
					break
				}
			}
		}
		feature := keys[featureIdx]
		var prefix []core.RatingKey
		if cinema && feature.Kind() == core.KindMovie {
			prefix = s.cinemaTrailerKeys(ctx, pr, feature, n)
		}
		if preroll, order := s.prerollKey(ctx, pr, feature, cinema); preroll != 0 {
			if order == core.PrerollAfterTrailers {
				prefix = append(prefix, preroll)
			} else {
				prefix = append([]core.RatingKey{preroll}, prefix...)
			}
		}
		if len(prefix) > 0 {
			merged := append([]core.RatingKey(nil), keys[:featureIdx]...)
			merged = append(append(merged, prefix...), keys[featureIdx:]...)
			keys = merged
			prefixAt = featureIdx
			s.prewarmFeature(ctx, pr, feature, s.clientCaps(pr))
		}
	}
	snap, err := s.queues.createWith(pr.User.ID, pr.ClientID, keys, s.now(), func(p *playQueue) {
		p.sourceURI = uri
		if len(keys) > 0 && !strings.HasPrefix(uri, "library://") {
			if path, e := uriPath(uri); e == nil {
				authority := s.identity().MachineIdentifier
				if strings.HasPrefix(uri, "server://") {
					authority, _, _ = strings.Cut(strings.TrimPrefix(uri, "server://"), "/")
				}
				kind := "item"
				sourceKey := keyFromPath(path)
				if strings.Contains(path, ",") || strings.Contains(path, "/sections/") || strings.HasSuffix(path, "/children") || strings.HasSuffix(path, "/allLeaves") || strings.HasSuffix(path, "/grandchildren") || sourceKey.Kind() == core.KindShow || sourceKey.Kind() == core.KindSeason || sourceKey.Kind() == core.KindCollection {
					kind = "directory"
				}
				p.sourceURI = "library://" + authority + "/" + kind + "/" + url.QueryEscape(path)
			}
		}
		p.playlistID, p.playlistTitle = plKey, plTitle
		if start := keyFromPath(q.Get("key")); start != 0 {
			for i, it := range p.items {
				if it.key == start {
					p.selected = i
					break
				}
			}
		}
		if prefixAt >= 0 {
			p.selected = prefixAt // playback starts with the first trailer
		}
		if shuffle {
			shuffleQueue(p)
		}
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writePlayQueue(w, r, snap)
}

// continuousKeys resolves a play queue URI; with continuous=1 a single
// episode expands to the remaining episodes in its presented season.
func (s *Server) continuousKeys(ctx context.Context, pr *Request, uri string, continuous bool) ([]core.RatingKey, error) {
	if strings.HasPrefix(uri, "server://") {
		authority, _, _ := strings.Cut(strings.TrimPrefix(uri, "server://"), "/")
		if authority != s.identity().MachineIdentifier {
			return nil, nil
		}
	}
	keys, err := s.resolveURI(ctx, pr, uri)
	if err != nil {
		return nil, err
	}
	if !continuous || len(keys) != 1 || keys[0].Kind() != core.KindEpisode {
		return keys, nil
	}
	ep := keys[0]
	season := ep.Season()
	if s.d.EpisodeOrder != nil {
		if n, _, _, ok := s.d.EpisodeOrder.Position(ctx, pr.User, ep); ok {
			season = n
		}
	}
	eps, err := s.metadataChildren(ctx, pr, core.SeasonKey(ep.TMDB(), season))
	if err != nil {
		return keys, nil
	}
	for i, e := range eps {
		if e.Key == ep {
			candidates := make([]core.Title, 0, len(eps)-i)
			for _, candidate := range eps[i+1:] {
				// Presented seasons: an episode order may place a TMDB special
				// inside a regular season, where it belongs in the queue.
				if (ep.Season() > 0 && s.queueSpecial(ctx, pr, candidate.Key)) || (!candidate.Released.IsZero() && candidate.Released.After(s.now())) {
					continue
				}
				candidates = append(candidates, candidate)
			}
			candidates = s.availableForViewer(ctx, pr.User, s.visibleTitles(pr.User, candidates))
			return capKeys(append([]core.RatingKey{ep}, titleKeys(candidates)...)), nil
		}
	}
	return keys, nil
}

// queueSpecial identifies a special outside an explicitly interleaved order.
func (s *Server) queueSpecial(ctx context.Context, pr *Request, key core.RatingKey) bool {
	if key.Kind() != core.KindEpisode || key.Season() != 0 {
		return false
	}
	if s.d.EpisodeOrder != nil {
		if season, _, _, ok := s.d.EpisodeOrder.Position(ctx, pr.User, key); ok && season > 0 {
			return false
		}
	}
	return true
}

// keyFromPath extracts a rating key from "/library/metadata/123" or "123".
func keyFromPath(p string) core.RatingKey {
	if p == "" {
		return 0
	}
	if i := strings.Index(p, "/library/metadata/"); i >= 0 {
		p = p[i+len("/library/metadata/"):]
	}
	p, _, _ = strings.Cut(p, "/")
	p, _, _ = strings.Cut(p, "?")
	k, err := core.ParseRatingKey(p)
	if err != nil {
		return 0
	}
	return k
}

func shuffleQueue(p *playQueue) {
	if p.shuffled || len(p.items) < 2 || p.selected < 0 || p.selected >= len(p.items) {
		return
	}
	p.original = append([]pqItem(nil), p.items...)
	sel := p.items[p.selected]
	rest := append(append([]pqItem(nil), p.items[:p.selected]...), p.items[p.selected+1:]...)
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	p.items = append([]pqItem{sel}, rest...)
	p.selected = 0
	p.shuffled = true
	p.version++
}

func unshuffleQueue(p *playQueue) {
	if !p.shuffled {
		return
	}
	var sel int64
	if p.selected >= 0 && p.selected < len(p.items) {
		// Removing every item leaves no selection to restore.
		sel = p.items[p.selected].id
	}
	present := map[int64]bool{}
	for _, it := range p.items {
		present[it.id] = true
	}
	var items []pqItem
	for _, it := range p.original {
		if present[it.id] {
			items = append(items, it)
		}
	}
	p.items, p.original, p.shuffled = items, nil, false
	for i, it := range p.items {
		if it.id == sel {
			p.selected = i
		}
	}
	p.version++
}

func (s *Server) pqParam(w http.ResponseWriter, r *http.Request) (int64, *core.User, bool) {
	u := FromContext(r.Context()).User
	id, err := strconv.ParseInt(chi.URLParam(r, "pq"), 10, 32)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return 0, nil, false
	}
	if u == nil {
		writeHTTPError(w, http.StatusNotFound)
		return 0, nil, false
	}
	return id, u, true
}

func (s *Server) handlePlayQueue(w http.ResponseWriter, r *http.Request) {
	id, u, ok := s.pqParam(w, r)
	if !ok {
		return
	}
	var snap playQueue
	if r.URL.Query().Get("own") == "1" {
		snap, ok = s.queues.take(id, u.ID, FromContext(r.Context()).ClientID, s.now())
	} else {
		snap, ok = s.queues.get(id, u.ID)
	}
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.writePlayQueue(w, r, snap)
}

// handlePlayQueueMutate covers PUT /playQueues/{id} (add) and the
// shuffle/unshuffle/reset/clear/remove/move sub-resources.
func (s *Server) handlePlayQueueMutate(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, u, ok := s.pqParam(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		pr := FromContext(ctx)
		q := r.URL.Query()
		if !validQueueWindow(q) {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if op == "move" && q.Has("after") && q.Get("after") == "" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if existing, found := s.queues.get(id, u.ID); found {
			if existing.liveSession != "" {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			// Once the viewer has explicitly queued content, PMS refuses
			// shuffle controls rather than replacing that Up Next order.
			if (op == "shuffle" || op == "unshuffle") && existing.lastAdded != 0 {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
		}
		var addKeys []core.RatingKey
		if op == "add" {
			var err error
			switch {
			case q.Get("uri") != "":
				addKeys, err = s.continuousKeys(ctx, pr, q.Get("uri"), q.Get("continuous") == "1")
			case q.Get("playlistID") != "":
				var k core.RatingKey
				if k, err = core.ParseRatingKey(q.Get("playlistID")); err == nil {
					addKeys, err = s.expandKey(ctx, pr, k)
				}
			}
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
		pre, err := s.queues.preallocate(addKeys)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		itemID, _ := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64)
		var bad bool
		var before playQueue
		var removedSelected bool
		snap, ok := s.queues.with(id, u.ID, s.now(), func(p *playQueue) {
			before = p.snapshot()
			removedSelected = false
			bad = false // a clustered edit may run again after a conflict
			switch op {
			case "add":
				s.queues.addItems(p, addKeys, pre, q.Get("next") == "1")
			case "shuffle":
				s.queues.reshuffle(p, true)
			case "unshuffle":
				s.queues.reshuffle(p, false)
			case "reset":
				p.selected = 0
				p.version++
			case "clear":
				// Keep the playing item, drop the rest.
				if p.selected >= 0 && p.selected < len(p.items) {
					p.items = []pqItem{p.items[p.selected]}
				} else {
					p.items = nil
				}
				p.selected = 0
				p.original = nil
				p.version++
			case "remove":
				idx := indexOfItem(p.items, itemID)
				if idx < 0 {
					bad = true
					return
				}
				removedSelected = idx == p.selected
				p.items = append(p.items[:idx], p.items[idx+1:]...)
				if idx < p.selected {
					p.selected--
				}
				if removedSelected && p.selected >= len(p.items) {
					p.selected = -1
				}
				p.reanchorAdded()
				p.version++
			case "move":
				idx := indexOfItem(p.items, itemID)
				if idx < 0 {
					bad = true
					return
				}
				if q.Get("after") == strconv.FormatInt(itemID, 10) {
					bad = true
					return
				}
				var selID int64
				if p.selected >= 0 && p.selected < len(p.items) {
					selID = p.items[p.selected].id
				}
				it := p.items[idx]
				rest := append(append([]pqItem(nil), p.items[:idx]...), p.items[idx+1:]...)
				to := 0
				if after, err := strconv.ParseInt(q.Get("after"), 10, 64); err == nil && after > 0 {
					if a := indexOfItem(rest, after); a >= 0 {
						to = a + 1
					}
				}
				p.items = append(rest[:to], append([]pqItem{it}, rest[to:]...)...)
				p.selected = indexOfItem(p.items, selID)
				p.reanchorAdded()
				p.version++
			}
		})
		if !ok || bad {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if op == "remove" {
			before.suppressItem = itemID
			if removedSelected {
				before.suppressRows = true
				before.suppressMetadata = true
				before.forceOffsetZero = true
			}
			s.writePlayQueue(w, r, before)
			return
		}
		s.writePlayQueue(w, r, snap)
	}
}

func indexOfItem(items []pqItem, id int64) int {
	for i, it := range items {
		if it.id == id {
			return i
		}
	}
	return -1
}

// writePlayQueue renders a queue window around the selected item.
func (s *Server) writePlayQueue(w http.ResponseWriter, r *http.Request, pq playQueue) {
	if pq.liveSession != "" {
		s.writeLivePlayQueue(w, r, pq)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if !validQueueWindow(q) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	center := pq.selected
	if q.Has("center") {
		id, err := strconv.ParseInt(q.Get("center"), 10, 64)
		center = -1
		if err == nil {
			center = indexOfItem(pq.items, id)
		}
	}
	lo, hi := 0, 0
	if center >= 0 && center < len(pq.items) {
		lo, hi = 0, len(pq.items)
		if q.Has("window") {
			n, _ := strconv.Atoi(q.Get("window"))
			if n == 0 {
				lo, hi = center, center
			} else {
				lo, hi = max(0, center-n), min(len(pq.items), center+n+1)
			}
		}
		before, _ := queueFlag(q, "includeBefore")
		after, _ := queueFlag(q, "includeAfter")
		if q.Has("includeBefore") && !before {
			lo = min(hi, center+1)
		}
		if q.Has("includeAfter") && !after {
			hi = max(lo, center)
		}
	}
	if pq.suppressRows {
		lo, hi = 0, 0
	}
	slice := pq.items[lo:hi]
	keys := make([]core.RatingKey, len(slice))
	for i, it := range slice {
		keys[i] = it.key
	}
	// Selection identity is independent of the requested centre/window.
	if pq.selected >= 0 && pq.selected < len(pq.items) && !pq.suppressMetadata {
		keys = append(keys, pq.items[pq.selected].key)
	}
	titles := s.visibleTitles(pr.User, s.getTitles(ctx, dedupeKeys(keys)))
	byKey := make(map[core.RatingKey]*core.Title, len(titles))
	for i := range titles {
		byKey[titles[i].Key] = &titles[i]
	}
	state := s.stateFor(ctx, pr, titles)

	var selItem pqItem
	if pq.selected >= 0 && pq.selected < len(pq.items) {
		selItem = pq.items[pq.selected]
	}
	if selTitle, ok := byKey[selItem.key]; ok {
		s.warmDetail(ctx, pr, selTitle)
	}
	mc := libraryContainer()
	mc.AllowSync = nil
	mc.Extra = []model.Attr{model.A("playQueueID", pq.id), model.A("playQueueShuffled", pq.shuffled), model.A("playQueueVersion", pq.version)}
	if len(pq.items) > 0 || pq.selected < 0 {
		offset := max(0, pq.selected)
		if pq.forceOffsetZero {
			offset = 0
		}
		mc.Extra = append(mc.Extra, model.A("playQueueSelectedItemOffset", offset), model.A("playQueueTotalCount", len(pq.items)))
	}
	if selItem.id != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueSelectedItemID", selItem.id))
		selectedVisible := byKey[selItem.key] != nil
		if selItem.key.Kind() == core.KindExtra {
			_, selectedVisible = s.extraMetadataByKey(ctx, pr, selItem.key)
		}
		if selectedVisible && !pq.suppressMetadata {
			mc.Extra = append(mc.Extra, model.A("playQueueSelectedMetadataItemID", selItem.key.String()))
		}
	}
	if pq.playlistID == 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueSourceURI", pq.sourceURI))
	}
	if pq.lastAdded != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueLastAddedItemID", pq.lastAdded))
	}
	if pq.playlistID != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueuePlaylistID", pq.playlistID.String()), model.A("playQueuePlaylistTitle", pq.playlistTitle))
	}
	pr.versionFlowKey = selItem.key
	pr.versionItemKey = selItem.key
	for _, it := range slice {
		if it.id == pq.suppressItem {
			continue
		}
		if it.key.Kind() == core.KindExtra {
			if md, ok := s.extraMetadataByKey(ctx, pr, it.key); ok {
				md.PlayQueueItemID = it.id
				mc.Metadata = append(mc.Metadata, md)
			}
			continue
		}
		t, ok := byKey[it.key]
		if !ok {
			continue
		}
		md := s.metadata(ctx, pr, t, renderOpts{detail: true, media: true, state: state})
		md.PlayQueueItemID = it.id
		mc.Metadata = append(mc.Metadata, md)
	}
	applyExcludedElements(mc.Metadata, excludedElements(q))
	if pr.HasPage {
		lo, hi := pr.window(len(mc.Metadata))
		mc.Metadata = mc.Metadata[lo:hi]
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func dedupeKeys(ks []core.RatingKey) []core.RatingKey {
	seen := make(map[core.RatingKey]bool, len(ks))
	out := ks[:0:0]
	for _, k := range ks {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func validQueueWindow(q url.Values) bool {
	if q.Has("includeBefore") && q.Has("includeAfter") {
		before, _ := queueFlag(q, "includeBefore")
		after, _ := queueFlag(q, "includeAfter")
		if !before && !after {
			return false
		}
	}
	for _, name := range []string{"includeBefore", "includeAfter"} {
		if _, ok := queueFlag(q, name); !ok {
			return false
		}
	}
	if q.Has("window") {
		n, err := strconv.Atoi(q.Get("window"))
		if err != nil || n < 0 {
			return false
		}
	}
	return true
}

func (q *playQueue) reanchorAdded() {
	if q.lastAdded == 0 {
		return
	}
	for i := max(0, q.selected+1); i < len(q.items) && q.items[i].added; i++ {
		q.lastAdded = q.items[i].id
	}
}

// Controls preserve the playing identity while every other slot receives a
// newly allocated identity. This also applies when the requested state repeats.
func (s *playQueueStore) reshuffle(q *playQueue, shuffled bool) {
	oldVersion := q.version
	if shuffled {
		original := append([]pqItem(nil), q.original...)
		if q.shuffled {
			q.shuffled = false
		}
		shuffleQueue(q)
		if len(original) > 0 {
			q.original = original
		}
	} else {
		unshuffleQueue(q)
	}
	q.version = oldVersion + 1
	keys := make([]core.RatingKey, len(q.items))
	for i, it := range q.items {
		keys[i] = it.key
	}
	var minted []pqItem
	if s.cl == nil {
		minted = s.newItemsLocked(keys)
	} else {
		var err error
		minted, err = s.preallocate(keys)
		if err != nil {
			return
		}
	}
	ids := map[int64]int64{}
	for i := range q.items {
		if i != q.selected {
			ids[q.items[i].id] = minted[i].id
			q.items[i].id = minted[i].id
		}
	}
	for i := range q.original {
		if id, ok := ids[q.original[i].id]; ok {
			q.original[i].id = id
		}
	}
}

func (s *playQueueStore) take(id, userID int64, clientID string, now time.Time) (playQueue, bool) {
	if s.cl == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		q, ok := s.queues[id]
		if !ok || q.userID != userID {
			return playQueue{}, false
		}
		if q.clientID != clientID {
			s.retireClientLocked(userID, clientID, id)
			q.clientID = clientID
			q.version++
			q.updatedAt = now
		}
		return q.snapshot(), true
	}
	current, found := s.get(id, userID)
	if !found {
		return playQueue{}, false
	}
	if current.clientID == clientID {
		return current, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	tx, err := s.cl.db.BeginTx(ctx, nil)
	if err != nil {
		return playQueue{}, false
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE plex_counters SET value=value WHERE name='playqueue_item'`); err != nil {
		return playQueue{}, false
	}
	var owner, rev, updated int64
	var data string
	if err = tx.QueryRowContext(ctx, `SELECT user_id,rev,data,updated_at FROM plex_playqueues WHERE id=?`, id).Scan(&owner, &rev, &data, &updated); err != nil || owner != userID {
		return playQueue{}, false
	}
	q, err := decodeQueue(id, owner, rev, updated, data)
	if err != nil {
		return playQueue{}, false
	}
	if q.clientID != clientID {
		q.clientID = clientID
		q.version++
		q.rev++
		q.updatedAt = now
	}
	data, err = encodeQueue(q)
	if err != nil {
		return playQueue{}, false
	}
	if _, err = tx.ExecContext(ctx, `UPDATE plex_playqueues SET data=?,rev=?,updated_at=? WHERE id=?`, data, q.rev, now.UnixMilli(), id); err != nil {
		return playQueue{}, false
	}
	retired, err := retireQueueClient(ctx, tx, userID, clientID, id)
	if err != nil {
		return playQueue{}, false
	}
	if err = tx.Commit(); err != nil {
		return playQueue{}, false
	}
	for _, old := range retired {
		s.forget(old)
		s.cl.publish("invalidate:playqueue", queueEvent{ID: old})
	}
	s.cache(q)
	s.cl.publish("invalidate:playqueue", queueEvent{ID: id})
	return q.snapshot(), true
}

// Bound work by active queues, independently of catalogue size.
func retireQueueClient(ctx context.Context, tx *sql.Tx, userID int64, clientID string, keep int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,data FROM plex_playqueues WHERE user_id=? AND id<>? LIMIT 2000`, userID, keep)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var data string
		if err = rows.Scan(&id, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var sq storedQueue
		if err = json.Unmarshal([]byte(data), &sq); err != nil {
			rows.Close()
			return nil, err
		}
		if sq.ClientID == clientID {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM plex_playqueues WHERE id=? AND user_id=?`, id, userID); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
