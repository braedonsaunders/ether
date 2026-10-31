package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// ---------------------------------------------------------------------------
// Play session tracking (timeline-driven; feeds /status/sessions and
// "playing" notifications).

type playEntry struct {
	id              string // userID/clientID/sessionIdentifier
	sessionKey      int
	user            core.User
	client          Request // product/platform/device snapshot (no User)
	key             core.RatingKey
	state           string
	offset          time.Duration
	duration        time.Duration
	playQueueItemID int64
	transcodeID     string
	startedAt       time.Time
	updatedAt       time.Time

	// Set on entries another cluster node serves (read from plex_sessions).
	node     string
	remoteTS *model.TranscodeSess
}

// sessionActivities reads and writes the mounted broker. A server that is
// not mounted keeps the rows itself so an in-flight request still sees them
// after unmount drops the broker binding.
type sessionActivities struct {
	mu     sync.Mutex
	local  map[string]playEntry
	next   int
	broker *sessions.Broker
}

func newSessionActivities() *sessionActivities {
	return &sessionActivities{local: map[string]playEntry{}}
}

func trackID(userID int64, clientID, sessionID string) string {
	return fmt.Sprintf("%d/%s/%s", userID, clientID, sessionID)
}
func (t *sessionActivities) bind(b *sessions.Broker) {
	if t != nil {
		t.broker = b
	}
}
func (t *sessionActivities) has(id string) bool {
	if t == nil {
		return false
	}
	if t.broker != nil {
		return t.broker.HasPlexActivity(id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.local[id]
	return ok
}
func (t *sessionActivities) ensure(id string, key int) {
	if t == nil {
		return
	}
	if t.broker != nil {
		t.broker.EnsurePlexActivity(id, key)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.local[id]; !ok && key > 0 {
		t.local[id] = playEntry{id: id, sessionKey: key}
	}
}
func (t *sessionActivities) update(id string, fn func(*playEntry)) playEntry {
	if t == nil {
		var zero playEntry
		if fn != nil {
			fn(&zero)
		}
		return zero
	}
	if t.broker != nil {
		act := t.broker.UpsertPlexActivity(id, func(a *sessions.PlexActivity) {
			e := activityEntry(*a)
			fn(&e)
			*a = entryActivity(e)
		})
		return activityEntry(act)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.local[id]
	if !ok {
		t.next++
		e = playEntry{id: id, sessionKey: t.next}
	}
	fn(&e)
	t.local[id] = e
	return e
}
func (t *sessionActivities) remove(id string) (playEntry, bool) {
	if t == nil {
		return playEntry{}, false
	}
	if t.broker != nil {
		act, ok := t.broker.RemovePlexActivity(id)
		if !ok {
			return playEntry{}, false
		}
		return activityEntry(act), true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.local[id]
	if !ok {
		return playEntry{}, false
	}
	delete(t.local, id)
	return e, true
}
func (t *sessionActivities) list() []playEntry {
	if t == nil {
		return nil
	}
	if t.broker != nil {
		acts := t.broker.ListPlexActivities()
		out := make([]playEntry, 0, len(acts))
		for _, a := range acts {
			out = append(out, activityEntry(a))
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]playEntry, 0, len(t.local))
	for _, e := range t.local {
		out = append(out, e)
	}
	return out
}
func (t *sessionActivities) expire(now time.Time, ttl time.Duration) []playEntry {
	if t == nil {
		return nil
	}
	if t.broker != nil {
		acts := t.broker.ExpirePlexActivities(now, ttl)
		out := make([]playEntry, 0, len(acts))
		for _, a := range acts {
			out = append(out, activityEntry(a))
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []playEntry
	for id, e := range t.local {
		if now.Sub(e.updatedAt) > ttl {
			out = append(out, e)
			delete(t.local, id)
		}
	}
	return out
}

func activityEntry(a sessions.PlexActivity) playEntry {
	e := playEntry{id: a.ID, sessionKey: a.SessionKey, user: a.User, key: a.Key, state: a.State, offset: a.Offset, duration: a.Duration, playQueueItemID: a.PlayQueueItemID, transcodeID: a.TranscodeID, startedAt: a.StartedAt, updatedAt: a.UpdatedAt}
	e.client = Request{ClientID: a.ClientID, Product: a.Product, Version: a.Version, Platform: a.Platform, PlatformVersion: a.PlatformVersion, Device: a.Device, DeviceName: a.DeviceName, Model: a.Model, Vendor: a.Vendor, SessionID: a.SessionID, Remote: a.Remote}
	if a.RemoteIP != "" {
		e.client.RemoteIP = parseAddr(a.RemoteIP)
	}
	return e
}

func entryActivity(e playEntry) sessions.PlexActivity {
	a := sessions.PlexActivity{ID: e.id, SessionKey: e.sessionKey, User: e.user, ClientID: e.client.ClientID, Product: e.client.Product, Version: e.client.Version, Platform: e.client.Platform, PlatformVersion: e.client.PlatformVersion, Device: e.client.Device, DeviceName: e.client.DeviceName, Model: e.client.Model, Vendor: e.client.Vendor, SessionID: e.client.SessionID, Remote: e.client.Remote, Key: e.key, State: e.state, Offset: e.offset, Duration: e.duration, PlayQueueItemID: e.playQueueItemID, TranscodeID: e.transcodeID, StartedAt: e.startedAt, UpdatedAt: e.updatedAt}
	if e.client.RemoteIP.IsValid() {
		a.RemoteIP = e.client.RemoteIP.String()
	}
	return a
}

// ---------------------------------------------------------------------------
// Notification fan-out

type subscriber struct {
	userID  int64
	owner   bool
	ch      chan notification
	filters map[string]bool // empty = all
}

type notification struct {
	typ  string
	data []byte   // JSON NotificationContainer (websocket)
	sse  [][]byte // one bare entry object per event (eventsource)
}

func newNotification(typ string, data []byte) notification {
	return notification{typ: typ, data: data, sse: sseEntries(data)}
}

// sseEntries splits a JSON NotificationContainer into the payloads PMS sends
// on /:/eventsource/notifications: one {"<Entry>":{...}} object per entry,
// with no container (recorded from PMS 1.43). Clients that read the
// eventsource stream ignore the websocket container, so library changes
// never reached their cached hubs.
func sseEntries(data []byte) [][]byte {
	var root struct {
		NotificationContainer map[string]json.RawMessage
	}
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	keys := make([]string, 0, len(root.NotificationContainer))
	for k, v := range root.NotificationContainer {
		if len(v) > 0 && v[0] == '[' {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out [][]byte
	for _, k := range keys {
		var items []json.RawMessage
		if json.Unmarshal(root.NotificationContainer[k], &items) != nil {
			continue
		}
		for _, item := range items {
			if b, err := json.Marshal(map[string]json.RawMessage{k: item}); err == nil {
				out = append(out, b)
			}
		}
	}
	return out
}

type notifier struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
	done   chan struct{}
}

func newNotifier() *notifier {
	return &notifier{subs: map[*subscriber]struct{}{}, done: make(chan struct{})}
}

func (n *notifier) subscribe(filters []string, user *core.User) (*subscriber, bool) {
	sub := &subscriber{userID: user.ID, owner: user.IsOwner, ch: make(chan notification, 64), filters: map[string]bool{}}
	for _, f := range filters {
		if f = strings.TrimSpace(f); f != "" {
			sub.filters[f] = true
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, false
	}
	n.subs[sub] = struct{}{}
	return sub, true
}

func (n *notifier) unsubscribe(sub *subscriber) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs, sub)
}

// encode serialises a notification for subscribers.
func encodeNotification(nc model.NotificationContainer) (notification, bool) {
	data, err := model.Marshal(nc, model.JSON)
	if err != nil {
		return notification{}, false
	}
	data = []byte(strings.TrimSuffix(string(data), "\n"))
	return newNotification(nc.Type, data), true
}

// publish delivers to every interested subscriber, dropping for slow ones.
func (n *notifier) publish(nc model.NotificationContainer, userID int64) {
	if msg, ok := encodeNotification(nc); ok {
		n.deliver(msg, userID)
	}
}

// deliver hands an encoded notification to every interested subscriber.
func (n *notifier) deliver(msg notification, userID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for sub := range n.subs {
		if userID != 0 && sub.userID != userID && !sub.owner {
			continue
		}
		if len(sub.filters) > 0 && !sub.filters[msg.typ] {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
		}
	}
}

func (n *notifier) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.closed {
		n.closed = true
		close(n.done)
	}
}

func notificationFilters(r *http.Request) []string {
	q := r.URL.Query()
	v := q.Get("filters")
	if v == "" {
		v = q.Get("filter")
	}
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// handleWebsocket serves /:/websockets/notifications.
func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Plex Web connects from app.plex.tv; tokens, not origins, gate access.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Debug("websocket accept failed", "err", err)
		return
	}
	defer c.CloseNow()
	sub, ok := s.notifier.subscribe(notificationFilters(r), FromContext(r.Context()).User)
	if !ok {
		c.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer s.notifier.unsubscribe(sub)

	ctx := c.CloseRead(r.Context())
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifier.done:
			c.Close(websocket.StatusGoingAway, "server shutting down")
			return
		case <-ping.C:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case msg := <-sub.ch:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageText, msg.data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// sseWriteTimeout bounds one Server-Sent Events write. Pings go out every
// 10s, so a healthy client never comes close.
const sseWriteTimeout = 2 * time.Minute

// handleEventSource serves /:/eventsource/notifications (Server-Sent Events).
func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	sub, ok := s.notifier.subscribe(notificationFilters(r), FromContext(r.Context()).User)
	if !ok {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	defer s.notifier.unsubscribe(sub)
	// No server WriteTimeout covers this long-lived response; arm a deadline
	// before each write so a client that stops reading errors out instead
	// of parking this goroutine and its subscription forever.
	rc := http.NewResponseController(w)
	arm := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	arm()
	fmt.Fprint(w, "retry: 5000\n\n")
	fl.Flush()
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.notifier.done:
			return
		case <-ping.C:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			arm()
			if _, err := fmt.Fprint(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		case msg := <-sub.ch:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			arm()
			payloads := msg.sse
			if len(payloads) == 0 {
				payloads = [][]byte{msg.data}
			}
			for _, data := range payloads {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.typ, data); err != nil {
					return
				}
			}
			fl.Flush()
		}
	}
}

// notifyPlaying publishes a PlaySessionStateNotification.
func (s *Server) notifyPlaying(e playEntry, state string) {
	if state == "" {
		state = e.state
	}
	s.publishNotification(model.NotificationContainer{
		Type: "playing",
		Size: 1,
		PlaySessionStateNotification: []model.PlaySessionStateNotification{{
			SessionKey:       fmt.Sprint(e.sessionKey),
			ClientIdentifier: e.client.ClientID,
			GUID:             "",
			RatingKey:        e.key.String(),
			URL:              "",
			Key:              "/library/metadata/" + e.key.String(),
			ViewOffset:       ms(e.offset),
			PlayQueueItemID:  e.playQueueItemID,
			State:            state,
			TranscodeSession: e.transcodeID,
		}},
	}, e.user.ID)
}

// WatchStateChanged invalidates client library metadata after a committed watch
// update. A playing notification only updates session status; clients can keep
// their cached Continue Watching shelf until a library timeline event arrives.
func (s *Server) WatchStateChanged(userID int64, key core.RatingKey) {
	var kind core.MediaType
	switch key.Kind() {
	case core.KindMovie:
		kind = core.TypeMovie
	case core.KindEpisode:
		kind = core.TypeEpisode
	default:
		return
	}
	s.notifyTimeline(key, kind, "", userID)
}

// notifyTimeline publishes a library timeline entry (watched-state change).
func (s *Server) notifyTimeline(key core.RatingKey, t core.MediaType, title string, userID int64) {
	sec, _ := s.sectionFor(t)
	typeNum := map[core.MediaType]int{core.TypeMovie: 1, core.TypeShow: 2, core.TypeSeason: 3, core.TypeEpisode: 4}[t]
	// PMS names an episode's season and show, and a season's show, so a
	// client can refresh the show's rows (Continue Watching, On Deck).
	var parent, root string
	switch key.Kind() {
	case core.KindEpisode:
		parent, root = key.Parent().String(), core.ShowKey(key.TMDB()).String()
	case core.KindSeason:
		parent = key.Parent().String()
	}
	s.publishNotification(model.NotificationContainer{
		Type: "timeline",
		Size: 1,
		TimelineEntry: []model.TimelineEntry{{
			Identifier:   model.LibraryIdentifier,
			SectionID:    fmt.Sprint(sec.ID),
			ItemID:       key.String(),
			ParentItemID: parent,
			RootItemID:   root,
			Type:         typeNum,
			Title:        title,
			State:        5,
			UpdatedAt:    s.now().Unix(),
		}},
	}, userID)
}

// publishNotification delivers a notification to this node's subscribers
// and, on a clustered node, to every other node's.
func (s *Server) publishNotification(nc model.NotificationContainer, userID int64) {
	msg, ok := encodeNotification(nc)
	if !ok {
		return
	}
	s.notifier.deliver(msg, userID)
	if s.cl != nil {
		s.cl.shareNotification(msg, userID)
	}
}
