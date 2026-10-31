// Package watchparty is the shared room both Silo Watch Party and Jellyfin
// SyncPlay speak. One title, one timeline. Each member still opens their own
// playback session, so the broker keeps their transcode and stream limit.
package watchparty

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/google/uuid"
)

const (
	defaultLead = 500 * time.Millisecond
	maxMembers  = 16
	ticketTTL   = 30 * time.Second
	socketMax   = 300
	driftSeek   = 5 * time.Second
)

// Options configures access checks and the clock. Tests pass a fake clock.
type Options struct {
	Now     func() time.Time
	Lead    time.Duration
	UseLead bool
	// Allow reports whether this viewer may know the title exists.
	// Any error denies the party operation and the title is not returned.
	Allow func(context.Context, frontend.Protocol, *core.Viewer, core.RatingKey) error
	// Streams reports whether this member may open another play of the title.
	// core.ErrStreamLimit blocks that member without stopping the others,
	// unless the member who asked for playback is the one over the limit.
	// Callers receive frontend.ErrPartyForbidden for that refusal.
	Streams func(context.Context, frontend.Protocol, *core.Viewer, core.Device, core.RatingKey) error
	// ItemID and JellyfinUser render protocol ids. Nil uses a decimal form.
	ItemID       func(core.RatingKey) string
	JellyfinUser func(int64) string
}

// Engine is safe for concurrent clients.
type Engine struct {
	mu      sync.Mutex
	opts    Options
	rooms   map[string]*room
	codes   map[string]string
	invites map[string]string
	tickets map[string]*ticket
	subs    map[string]*sub
	nextSub int64
}

type room struct {
	id, code, invite, name string
	mode, policy           string
	phase, play            string
	revision, generation   int64
	item                   core.RatingKey
	fileID, libraryID      string
	queue                  []qitem
	index                  int
	anchor                 time.Duration
	anchorAt               time.Time
	paused                 bool
	rate                   float64
	repeat, shuffle        string
	updated                time.Time
	members                []*member
	last                   *frontend.PartyCommand
	lastIssued             time.Time
}

type qitem struct {
	item core.RatingKey
	id   string
}

type member struct {
	viewer                             *core.Viewer
	device                             core.Device
	protocol                           frontend.Protocol
	token                              string
	host                               bool
	connected, ready, buffering, lobby bool
	ignore, blocked                    bool
	session                            string
}

type sub struct {
	id int64
	ch chan frontend.PartyEvent
}

type ticket struct {
	room string
	key  string
	exp  time.Time
}

// New returns an empty engine.
func New(opts Options) *Engine {
	return &Engine{
		opts:    opts,
		rooms:   map[string]*room{},
		codes:   map[string]string{},
		invites: map[string]string{},
		tickets: map[string]*ticket{},
		subs:    map[string]*sub{},
	}
}

func (e *Engine) now() time.Time {
	if e.opts.Now != nil {
		return e.opts.Now()
	}
	return time.Now()
}

func (e *Engine) lead() time.Duration {
	if e.opts.UseLead {
		if e.opts.Lead < 0 {
			return 0
		}
		return e.opts.Lead
	}
	return defaultLead
}

func (e *Engine) itemID(k core.RatingKey) string {
	if k == 0 {
		return ""
	}
	if e.opts.ItemID != nil {
		return e.opts.ItemID(k)
	}
	return strconv.FormatInt(int64(k), 10)
}

func (e *Engine) jfUser(id int64) string {
	if e.opts.JellyfinUser != nil {
		return e.opts.JellyfinUser(id)
	}
	return strconv.FormatInt(id, 10)
}

func actorKey(a frontend.PartyActor) string {
	if a.Viewer == nil {
		return ""
	}
	id := a.Device.ID
	if id == "" {
		id = "device"
	}
	return string(a.Protocol) + "\x00" + strconv.FormatInt(a.Viewer.ID, 10) + "\x00" + id
}

func (e *Engine) Create(ctx context.Context, actor frontend.PartyActor, req frontend.PartyCreate) (frontend.PartyView, error) {
	if actor.Viewer == nil {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	mode := req.Mode
	if mode == "" {
		mode = "host_pick"
	}
	if mode != "host_pick" {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	id := req.RoomID
	if id == "" {
		id = uuid.NewString()
	}
	canon, err := canonicalID(id)
	if err != nil {
		return frontend.PartyView{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Watch Party"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.rooms[canon]; ok {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	if e.membershipLocked(actor) != nil {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	code := e.freshCodeLocked()
	invite := secret()
	r := &room{
		id: canon, code: code, invite: invite, name: name,
		mode: mode, policy: "guest_play_pause",
		phase: "lobby", play: "idle", paused: true, rate: 1,
		repeat: "RepeatNone", shuffle: "Sorted", updated: e.now(),
	}
	m := e.newMember(actor, true)
	r.members = []*member{m}
	m.connected = e.subs[actorKey(actor)] != nil
	e.rooms[canon] = r
	e.codes[code] = canon
	e.invites[invite] = canon
	e.announceLocked(r, m, true)
	return e.viewLocked(r, m), nil
}

func (e *Engine) Join(ctx context.Context, actor frontend.PartyActor, req frontend.PartyJoin) (frontend.PartyView, error) {
	if actor.Viewer == nil {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	e.mu.Lock()
	var r *room
	switch {
	case req.Code != "":
		r = e.rooms[e.codes[strings.ToUpper(strings.TrimSpace(req.Code))]]
	case req.Token != "":
		r = e.rooms[e.invites[req.Token]]
	case req.GroupID != "":
		id, err := canonicalID(req.GroupID)
		if err != nil {
			e.mu.Unlock()
			return frontend.PartyView{}, frontend.ErrPartyNotFound
		}
		r = e.rooms[id]
	default:
		e.mu.Unlock()
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	if r == nil || r.phase == "ended" {
		e.mu.Unlock()
		e.deny(actor, "GroupDoesNotExist")
		return frontend.PartyView{}, frontend.ErrPartyNotFound
	}
	if existing := memberOf(r, actor); existing != nil {
		existing.connected = e.subs[actorKey(actor)] != nil || existing.connected
		view := e.viewLocked(r, existing)
		e.mu.Unlock()
		return view, nil
	}
	item := r.item
	e.mu.Unlock()
	if item != 0 && e.opts.Allow != nil {
		if err := e.opts.Allow(ctx, actor.Protocol, actor.Viewer, item); err != nil {
			e.deny(actor, "LibraryAccessDenied")
			return frontend.PartyView{}, frontend.ErrPartyDenied
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r = e.rooms[r.id]
	if r == nil || r.item != item {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	if memberOf(r, actor) != nil {
		return e.viewLocked(r, memberOf(r, actor)), nil
	}
	if len(r.members) >= maxMembers {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	if e.membershipLocked(actor) != nil {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	m := e.newMember(actor, false)
	m.connected = e.subs[actorKey(actor)] != nil
	r.members = append(r.members, m)
	r.generation++
	r.updated = e.now()
	e.announceLocked(r, m, true)
	return e.viewLocked(r, m), nil
}

func (e *Engine) Get(_ context.Context, actor frontend.PartyActor, roomID, token string) (frontend.PartyView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyView{}, err
	}
	return e.viewLocked(r, m), nil
}

func (e *Engine) Select(ctx context.Context, actor frontend.PartyActor, roomID, token string, pick frontend.PartyPick) (frontend.PartyView, error) {
	if pick.Item == 0 {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	if err := e.visible(ctx, roomID, pick.Item); err != nil {
		return frontend.PartyView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyView{}, err
	}
	if !r.canQueue(m) {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	if r.item != pick.Item {
		// Re-check under the lock in case membership changed after visible().
		if err := e.visibleLocked(ctx, r, pick.Item); err != nil {
			return frontend.PartyView{}, err
		}
	}
	r.item = pick.Item
	r.fileID, r.libraryID = pick.FileID, pick.LibraryID
	r.queue = []qitem{{item: pick.Item, id: uuid.NewString()}}
	r.index = 0
	r.revision++
	r.generation++
	r.updated = e.now()
	if r.phase == "playing" {
		e.beginWaitLocked(r, 0)
	} else {
		e.snapshotsLocked(r)
	}
	return e.viewLocked(r, m), nil
}

func (e *Engine) Start(ctx context.Context, actor frontend.PartyActor, roomID, token string) (frontend.PartyView, error) {
	e.mu.Lock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		e.mu.Unlock()
		return frontend.PartyView{}, err
	}
	if !m.host {
		e.mu.Unlock()
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	if r.item == 0 {
		e.mu.Unlock()
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	item := r.item
	e.mu.Unlock()
	if err := e.streamsFor(ctx, actor, item); err != nil {
		return frontend.PartyView{}, err
	}
	if err := e.visible(ctx, roomID, item); err != nil {
		return frontend.PartyView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err = e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyView{}, err
	}
	if r.item != item {
		return frontend.PartyView{}, frontend.ErrPartyConflict
	}
	e.markBlockedLocked(ctx, r, m, item)
	e.beginWaitLocked(r, 0)
	return e.viewLocked(r, m), nil
}

func (e *Engine) Stop(_ context.Context, actor frontend.PartyActor, roomID, token string) (frontend.PartyView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyView{}, err
	}
	if !r.canQueue(m) && !m.host {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	e.issueLocked(r, "stop", 0)
	return e.viewLocked(r, m), nil
}

func (e *Engine) Policy(_ context.Context, actor frontend.PartyActor, roomID, token, policy string) (frontend.PartyView, error) {
	if policy != "host_only" && policy != "guest_play_pause" {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyView{}, err
	}
	if !m.host {
		return frontend.PartyView{}, frontend.ErrPartyForbidden
	}
	r.policy = policy
	r.generation++
	r.updated = e.now()
	e.snapshotsLocked(r)
	return e.viewLocked(r, m), nil
}

func (e *Engine) End(_ context.Context, actor frontend.PartyActor, roomID, token string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return err
	}
	if !m.host {
		return frontend.ErrPartyForbidden
	}
	e.closeLocked(r, "ended")
	return nil
}

func (e *Engine) Leave(_ context.Context, actor frontend.PartyActor) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m := e.roomOfLocked(actor)
	if r == nil || m == nil {
		e.sendSubLocked(actor, frontend.PartyEvent{Type: "group", GroupKind: "NotInGroup", GroupData: map[string]any{}})
		return frontend.ErrPartyNotFound
	}
	e.removeLocked(r, m, "left")
	return nil
}

func (e *Engine) List(_ context.Context, actor frontend.PartyActor) []frontend.PartyView {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []frontend.PartyView{}
	for _, r := range e.rooms {
		if r.phase == "ended" {
			continue
		}
		m := memberOf(r, actor)
		if m == nil {
			m = &member{viewer: actor.Viewer, protocol: actor.Protocol, device: actor.Device}
		}
		out = append(out, e.viewLocked(r, m))
	}
	return out
}

func (e *Engine) Transport(ctx context.Context, actor frontend.PartyActor, roomID, token string, in frontend.PartyInput) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return err
	}
	if !r.canTransport(m) {
		return frontend.ErrPartyForbidden
	}
	m.connected = true
	if in.Rate > 0 && in.Rate != r.rate {
		if !r.allowRate(in.Rate) {
			e.emit(m, frontend.PartyEvent{Type: "error", ErrorCode: "rate_unsupported", ErrorMessage: "This party includes a player that cannot change speed."})
			return frontend.ErrPartyForbidden
		}
		r.rate = in.Rate
	}
	switch in.Action {
	case "play":
		if r.item == 0 {
			return frontend.ErrPartyForbidden
		}
		r.phase = "playing"
		e.issueLocked(r, "play", r.anchor)
	case "pause":
		e.issueLocked(r, "pause", in.Position)
	case "seek":
		if in.Position < 0 {
			return frontend.ErrPartyForbidden
		}
		e.issueLocked(r, "seek", in.Position)
	default:
		return frontend.ErrPartyForbidden
	}
	return nil
}

func (e *Engine) Report(ctx context.Context, actor frontend.PartyActor, msg frontend.PartyReport) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, m := e.roomOfLocked(actor)
	if r == nil || m == nil {
		return frontend.ErrPartyNotFound
	}
	if msg.Kind != "ping" {
		m.connected = true
	}
	switch msg.Kind {
	case "ping":
		received := e.now()
		sent := e.now()
		e.emit(m, frontend.PartyEvent{Type: "pong", ClientSent: msg.ClientSent, Received: received, Sent: sent})
	case "attach":
		if r.item != 0 && e.opts.Streams != nil {
			if err := e.opts.Streams(ctx, m.protocol, m.viewer, m.device, r.item); err != nil {
				if errors.Is(err, core.ErrStreamLimit) {
					m.blocked = true
					e.emit(m, frontend.PartyEvent{Type: "error", ErrorCode: "stream_limit", ErrorMessage: "This profile is already using its available streams."})
					e.snapshotsLocked(r)
					return frontend.ErrPartyForbidden
				}
				return err
			}
		}
		m.session = msg.SessionID
		m.blocked = false
		e.snapshotsLocked(r)
	case "buffering":
		m.buffering = true
		m.ready = false
		if r.play == "playing" {
			r.play = "waiting"
			e.issueLocked(r, "pause", msg.Position)
			e.stateLocked(r, "Buffer")
		} else {
			e.snapshotsLocked(r)
		}
	case "ready":
		m.buffering = false
		m.ready = true
		m.session = firstNonEmpty(msg.SessionID, m.session)
		if r.play == "waiting" && r.allReady() {
			r.phase = "playing"
			e.issueLocked(r, "play", r.anchor)
		} else {
			e.snapshotsLocked(r)
		}
	case "state":
		if msg.SessionID != "" {
			m.session = msg.SessionID
		}
		if msg.Ready != nil {
			m.ready = *msg.Ready
		}
		if m.host && r.play == "playing" && e.now().Sub(r.lastIssued) > 2*time.Second {
			drift := msg.Position - r.positionAt(e.now())
			if drift < 0 {
				drift = -drift
			}
			if drift > driftSeek {
				e.issueLocked(r, "seek", msg.Position)
			}
		}
	case "lobby":
		if msg.Lobby != nil {
			m.lobby = *msg.Lobby
		}
		e.snapshotsLocked(r)
	case "ignore":
		if msg.Ignore != nil {
			m.ignore = *msg.Ignore
			r.generation++
			e.snapshotsLocked(r)
			if r.play == "waiting" && r.allReady() {
				e.issueLocked(r, "play", r.anchor)
			}
		}
	default:
		return frontend.ErrPartyForbidden
	}
	return nil
}

func (e *Engine) Queue(ctx context.Context, actor frontend.PartyActor, op frontend.PartyQueue) error {
	e.mu.Lock()
	r, m := e.roomOfLocked(actor)
	if r == nil || m == nil {
		e.mu.Unlock()
		return frontend.ErrPartyNotFound
	}
	if !r.canQueue(m) {
		e.mu.Unlock()
		e.deny(actor, "LibraryAccessDenied")
		return frontend.ErrPartyForbidden
	}
	id := r.id
	e.mu.Unlock()
	switch op.Kind {
	case "replace":
		if len(op.Items) == 0 {
			return frontend.ErrPartyForbidden
		}
		if _, err := e.Select(ctx, actor, id, "", frontend.PartyPick{Item: op.Items[0]}); err != nil {
			return err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		r, m = e.roomOfLocked(actor)
		if r == nil {
			return frontend.ErrPartyNotFound
		}
		r.queue = nil
		for _, item := range op.Items {
			r.queue = append(r.queue, qitem{item: item, id: uuid.NewString()})
		}
		if op.Index >= 0 && op.Index < len(r.queue) {
			r.index = op.Index
			r.item = r.queue[r.index].item
		}
		if err := e.streamsForLocked(ctx, m, r.item); err != nil {
			return err
		}
		e.markBlockedLocked(ctx, r, m, r.item)
		e.beginWaitLocked(r, op.Position)
		return nil
	case "append", "item", "remove", "move", "next", "prev", "repeat", "shuffle":
		e.mu.Lock()
		defer e.mu.Unlock()
		r, m = e.roomOfLocked(actor)
		if r == nil || m == nil || !r.canQueue(m) {
			return frontend.ErrPartyForbidden
		}
		return e.queueLocked(ctx, r, op)
	default:
		return frontend.ErrPartyForbidden
	}
}

func (e *Engine) IssueTicket(_ context.Context, actor frontend.PartyActor, roomID, token string) (frontend.PartyTicket, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, _, err := e.authLocked(actor, roomID, token)
	if err != nil {
		return frontend.PartyTicket{}, err
	}
	value := secret()
	e.tickets[value] = &ticket{room: r.id, key: actorKey(actor), exp: e.now().Add(ticketTTL)}
	return frontend.PartyTicket{Ticket: value, ExpiresIn: int(ticketTTL / time.Second), MaxSeconds: socketMax, Protocol: "silo.room.v2"}, nil
}

func (e *Engine) OpenSocket(_ context.Context, roomID, raw string) (frontend.PartySocket, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tk := e.tickets[raw]
	delete(e.tickets, raw)
	id, err := canonicalID(roomID)
	if err != nil || tk == nil || tk.room != id || e.now().After(tk.exp) {
		return frontend.PartySocket{}, frontend.ErrPartyForbidden
	}
	r := e.rooms[id]
	if r == nil {
		return frontend.PartySocket{}, frontend.ErrPartyNotFound
	}
	var m *member
	for _, candidate := range r.members {
		if actorKey(frontend.PartyActor{Viewer: candidate.viewer, Device: candidate.device, Protocol: candidate.protocol}) == tk.key {
			m = candidate
			break
		}
	}
	if m == nil {
		return frontend.PartySocket{}, frontend.ErrPartyForbidden
	}
	ch := e.bindLocked(tk.key, m.protocol == "silo")
	m.connected = true
	e.catchupLocked(r, m)
	subID := e.subs[tk.key].id
	key := tk.key
	return frontend.PartySocket{Actor: frontend.PartyActor{Viewer: m.viewer, Device: m.device, Protocol: m.protocol}, Events: ch, Close: func() { e.unbind(key, subID) }}, nil
}

func (e *Engine) Subscribe(actor frontend.PartyActor) (<-chan frontend.PartyEvent, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := actorKey(actor)
	if key == "" {
		ch := make(chan frontend.PartyEvent)
		close(ch)
		return ch, func() {}
	}
	ch := e.bindLocked(key, false)
	if r, m := e.roomOfLocked(actor); r != nil && m != nil {
		m.connected = true
		e.catchupLocked(r, m)
	}
	subID := e.subs[key].id
	return ch, func() { e.unbind(key, subID) }
}

func (e *Engine) bindLocked(key string, replaced bool) chan frontend.PartyEvent {
	if old := e.subs[key]; old != nil && replaced {
		send(old.ch, frontend.PartyEvent{Type: "replaced", Reason: "replaced"})
	}
	e.nextSub++
	ch := make(chan frontend.PartyEvent, 32)
	e.subs[key] = &sub{id: e.nextSub, ch: ch}
	return ch
}

func (e *Engine) unbind(key string, id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.subs[key]
	if s == nil || s.id != id {
		return
	}
	delete(e.subs, key)
	for _, r := range e.rooms {
		for _, m := range r.members {
			if actorKey(frontend.PartyActor{Viewer: m.viewer, Device: m.device, Protocol: m.protocol}) == key {
				m.connected = false
				m.ready = false
				e.snapshotsLocked(r)
			}
		}
	}
}

func (e *Engine) newMember(actor frontend.PartyActor, host bool) *member {
	return &member{viewer: actor.Viewer, device: actor.Device, protocol: actor.Protocol, token: secret(), host: host}
}

func (e *Engine) membershipLocked(actor frontend.PartyActor) *room {
	r, _ := e.roomOfLocked(actor)
	return r
}

func (e *Engine) roomOfLocked(actor frontend.PartyActor) (*room, *member) {
	for _, r := range e.rooms {
		if m := memberOf(r, actor); m != nil {
			return r, m
		}
	}
	return nil, nil
}

func memberOf(r *room, actor frontend.PartyActor) *member {
	if actor.Viewer == nil {
		return nil
	}
	for _, m := range r.members {
		if m.viewer != nil && m.viewer.ID == actor.Viewer.ID && m.protocol == actor.Protocol && deviceID(m.device) == deviceID(actor.Device) {
			return m
		}
	}
	return nil
}

func deviceID(d core.Device) string {
	if d.ID == "" {
		return "device"
	}
	return d.ID
}

func (e *Engine) authLocked(actor frontend.PartyActor, roomID, token string) (*room, *member, error) {
	id, err := canonicalID(roomID)
	if roomID != "" && err != nil {
		return nil, nil, frontend.ErrPartyNotFound
	}
	r, m := e.roomOfLocked(actor)
	if roomID != "" {
		r = e.rooms[id]
		m = nil
		if r != nil {
			m = memberOf(r, actor)
		}
	}
	if r == nil || m == nil {
		return nil, nil, frontend.ErrPartyNotFound
	}
	if token != "" && token != m.token {
		return nil, nil, frontend.ErrPartyForbidden
	}
	return r, m, nil
}

func (e *Engine) visible(ctx context.Context, roomID string, item core.RatingKey) error {
	e.mu.Lock()
	id, err := canonicalID(roomID)
	r := e.rooms[id]
	if err != nil || r == nil {
		e.mu.Unlock()
		return frontend.ErrPartyNotFound
	}
	members := append([]*member(nil), r.members...)
	e.mu.Unlock()
	if e.opts.Allow == nil {
		return nil
	}
	for _, m := range members {
		if m.viewer == nil {
			continue
		}
		if err := e.opts.Allow(ctx, m.protocol, m.viewer, item); err != nil {
			return frontend.ErrPartyDenied
		}
	}
	return nil
}

func (e *Engine) visibleLocked(ctx context.Context, r *room, item core.RatingKey) error {
	if e.opts.Allow == nil {
		return nil
	}
	for _, m := range r.members {
		if m.viewer == nil {
			continue
		}
		if err := e.opts.Allow(ctx, m.protocol, m.viewer, item); err != nil {
			return frontend.ErrPartyDenied
		}
	}
	return nil
}

func (e *Engine) streamsFor(ctx context.Context, actor frontend.PartyActor, item core.RatingKey) error {
	if e.opts.Streams == nil {
		return nil
	}
	err := e.opts.Streams(ctx, actor.Protocol, actor.Viewer, actor.Device, item)
	if errors.Is(err, core.ErrStreamLimit) {
		return frontend.ErrPartyForbidden
	}
	return err
}

func (e *Engine) streamsForLocked(ctx context.Context, m *member, item core.RatingKey) error {
	if e.opts.Streams == nil || m == nil {
		return nil
	}
	err := e.opts.Streams(ctx, m.protocol, m.viewer, m.device, item)
	if errors.Is(err, core.ErrStreamLimit) {
		return frontend.ErrPartyForbidden
	}
	return err
}

func (e *Engine) markBlockedLocked(ctx context.Context, r *room, actor *member, item core.RatingKey) {
	if e.opts.Streams == nil {
		return
	}
	for _, m := range r.members {
		if m == actor {
			m.blocked = false
			continue
		}
		err := e.opts.Streams(ctx, m.protocol, m.viewer, m.device, item)
		m.blocked = errors.Is(err, core.ErrStreamLimit)
		if m.blocked {
			e.emit(m, frontend.PartyEvent{Type: "error", ErrorCode: "stream_limit", ErrorMessage: "This profile is already using its available streams."})
		}
	}
}

func (e *Engine) beginWaitLocked(r *room, pos time.Duration) {
	r.phase = "playing"
	r.play = "waiting"
	r.paused = true
	r.anchor = pos
	r.anchorAt = e.now()
	r.generation++
	r.updated = e.now()
	for _, m := range r.members {
		if m.blocked {
			continue
		}
		m.ready = false
		m.buffering = false
	}
	for _, m := range r.members {
		if m.protocol == "jellyfin" {
			e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "PlayQueue", GroupData: e.queuePayload(r, "NewPlaylist")})
			e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "StateUpdate", GroupData: map[string]any{"State": "Waiting", "Reason": "Play"}})
		} else {
			e.emit(m, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, m)})
		}
	}
}

func (e *Engine) issueLocked(r *room, action string, pos time.Duration) {
	now := e.now()
	exec := now.Add(e.lead())
	jf := "Unpause"
	switch action {
	case "pause":
		if r.play == "playing" {
			pos = r.positionAt(exec)
			r.play = "paused"
		}
		r.anchor = pos
		r.anchorAt = exec
		r.paused = true
		jf = "Pause"
	case "play":
		r.anchorAt = exec
		r.paused = false
		r.play = "playing"
		r.phase = "playing"
		pos = r.anchor
		jf = "Unpause"
	case "seek":
		r.anchor = pos
		r.anchorAt = exec
		if r.paused {
			if r.play != "waiting" {
				r.play = "paused"
			}
		} else {
			r.play = "playing"
		}
		jf = "Seek"
	case "stop":
		r.phase = "lobby"
		r.play = "idle"
		r.paused = true
		r.anchor = 0
		r.anchorAt = exec
		pos = 0
		jf = "Stop"
		for _, m := range r.members {
			m.ready = false
			m.buffering = false
		}
	}
	r.generation++
	r.updated = now
	r.lastIssued = now
	cmd := frontend.PartyCommand{
		ID: uuid.NewString(), Action: action, Jellyfin: jf, Position: pos,
		ExecuteAt: exec, IssuedAt: now, Playback: r.play, SelectionRevision: r.revision,
		Rate: r.rate,
	}
	if r.index >= 0 && r.index < len(r.queue) {
		cmd.PlaylistItemID = r.queue[r.index].id
	}
	stored := cmd
	r.last = &stored
	for _, m := range r.members {
		outgoing := cmd
		outgoing.SessionID = m.session
		view := e.viewLocked(r, m)
		if m.protocol == "jellyfin" {
			e.emit(m, frontend.PartyEvent{Type: "command", View: view, Command: outgoing})
			e.emit(m, frontend.PartyEvent{Type: "group", View: view, GroupKind: "StateUpdate", GroupData: map[string]any{"State": jfState(r), "Reason": reasonFor(action)}})
		} else {
			e.emit(m, frontend.PartyEvent{Type: "transport", View: view, Command: outgoing})
			e.emit(m, frontend.PartyEvent{Type: "snapshot", View: view})
		}
	}
}

func reasonFor(action string) string {
	switch action {
	case "pause":
		return "Pause"
	case "seek":
		return "Seek"
	case "stop":
		return "Stop"
	default:
		return "Unpause"
	}
}

func (r *room) positionAt(now time.Time) time.Duration {
	if r.paused || r.rate <= 0 || !now.After(r.anchorAt) {
		if r.anchor < 0 {
			return 0
		}
		return r.anchor
	}
	delta := time.Duration(float64(now.Sub(r.anchorAt)) * r.rate)
	return r.anchor + delta
}

func (r *room) allReady() bool {
	saw := false
	for _, m := range r.members {
		if !m.connected || m.ignore || m.blocked {
			continue
		}
		saw = true
		if !m.ready {
			return false
		}
	}
	return saw
}

func (r *room) canQueue(m *member) bool {
	return m != nil && (m.host || r.jellyfinOnly())
}

func (r *room) canTransport(m *member) bool {
	if m == nil || m.blocked {
		return false
	}
	return m.host || r.policy == "guest_play_pause" || r.jellyfinOnly()
}

func (r *room) allowRate(rate float64) bool {
	if rate == 1 {
		return true
	}
	for _, m := range r.members {
		if m.protocol != "silo" {
			return false
		}
	}
	return true
}

func (r *room) jellyfinOnly() bool {
	if len(r.members) == 0 {
		return false
	}
	for _, m := range r.members {
		if m.protocol != "jellyfin" {
			return false
		}
	}
	return true
}

func (e *Engine) queueLocked(ctx context.Context, r *room, op frontend.PartyQueue) error {
	reason := "Queue"
	switch op.Kind {
	case "append":
		for _, item := range op.Items {
			if err := e.visibleLocked(ctx, r, item); err != nil {
				return err
			}
			r.queue = append(r.queue, qitem{item: item, id: uuid.NewString()})
		}
		reason = "Queue"
	case "item":
		for i, q := range r.queue {
			if q.id == op.PlaylistItemID {
				r.index = i
				r.item = q.item
				e.beginWaitLocked(r, 0)
				return nil
			}
		}
		return frontend.ErrPartyNotFound
	case "remove":
		next := r.queue[:0]
		for _, q := range r.queue {
			if q.id == op.PlaylistItemID {
				continue
			}
			next = append(next, q)
		}
		r.queue = next
		if r.index >= len(r.queue) {
			r.index = 0
		}
		if len(r.queue) == 0 {
			r.item = 0
			e.issueLocked(r, "stop", 0)
			return nil
		}
		r.item = r.queue[r.index].item
		reason = "RemoveItems"
	case "move":
		from := -1
		for i, q := range r.queue {
			if q.id == op.PlaylistItemID {
				from = i
			}
		}
		if from < 0 || op.Index < 0 || op.Index >= len(r.queue) {
			return frontend.ErrPartyForbidden
		}
		item := r.queue[from]
		r.queue = append(r.queue[:from], r.queue[from+1:]...)
		r.queue = append(r.queue[:op.Index], append([]qitem{item}, r.queue[op.Index:]...)...)
		reason = "MoveItem"
	case "next", "prev":
		if len(r.queue) == 0 {
			return nil
		}
		step := 1
		if op.Kind == "prev" {
			step = -1
		}
		next := r.index + step
		if r.repeat == "RepeatOne" {
			next = r.index
		} else if next < 0 || next >= len(r.queue) {
			if r.repeat == "RepeatAll" {
				next = (next + len(r.queue)) % len(r.queue)
			} else {
				e.issueLocked(r, "stop", 0)
				return nil
			}
		}
		r.index = next
		r.item = r.queue[next].item
		if err := e.visibleLocked(ctx, r, r.item); err != nil {
			return err
		}
		e.beginWaitLocked(r, 0)
		return nil
	case "repeat":
		if op.Repeat != "RepeatNone" && op.Repeat != "RepeatOne" && op.Repeat != "RepeatAll" {
			return frontend.ErrPartyForbidden
		}
		r.repeat = op.Repeat
		reason = "RepeatMode"
	case "shuffle":
		if op.Shuffle != "Sorted" && op.Shuffle != "Shuffle" {
			return frontend.ErrPartyForbidden
		}
		r.shuffle = op.Shuffle
		if op.Shuffle == "Shuffle" && len(r.queue) > 1 {
			for i := len(r.queue) - 1; i > 0; i-- {
				j := int(e.now().UnixNano() % int64(i+1))
				if j < 0 {
					j = -j
				}
				r.queue[i], r.queue[j] = r.queue[j], r.queue[i]
			}
		}
		reason = "ShuffleMode"
	}
	r.generation++
	r.updated = e.now()
	for _, m := range r.members {
		if m.protocol == "jellyfin" {
			e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "PlayQueue", GroupData: e.queuePayload(r, reason)})
		} else {
			e.emit(m, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, m)})
		}
	}
	return nil
}

func (e *Engine) queuePayload(r *room, reason string) map[string]any {
	items := make([]any, 0, len(r.queue))
	for _, q := range r.queue {
		items = append(items, map[string]any{"ItemId": e.itemID(q.item), "PlaylistItemId": q.id})
	}
	playing := r.play == "playing"
	return map[string]any{
		"Reason": reason, "LastUpdate": r.updated.UTC().Format(time.RFC3339Nano),
		"Playlist": items, "PlayingItemIndex": r.index, "StartPositionTicks": r.anchor.Nanoseconds() / 100,
		"IsPlaying": playing, "ShuffleMode": r.shuffle, "RepeatMode": r.repeat,
	}
}

func (e *Engine) closeLocked(r *room, reason string) {
	for _, m := range r.members {
		if m.protocol == "jellyfin" {
			e.emit(m, frontend.PartyEvent{Type: "group", GroupKind: "GroupLeft", GroupData: map[string]any{}, View: e.viewLocked(r, m)})
		} else {
			e.emit(m, frontend.PartyEvent{Type: "closed", Reason: reason, View: e.viewLocked(r, m)})
		}
	}
	delete(e.rooms, r.id)
	delete(e.codes, r.code)
	delete(e.invites, r.invite)
}

func (e *Engine) removeLocked(r *room, m *member, reason string) {
	if m.protocol == "jellyfin" {
		e.emit(m, frontend.PartyEvent{Type: "group", GroupKind: "GroupLeft", GroupData: map[string]any{}})
	} else {
		e.emit(m, frontend.PartyEvent{Type: "closed", Reason: reason})
	}
	next := r.members[:0]
	for _, o := range r.members {
		if o != m {
			next = append(next, o)
		}
	}
	r.members = next
	if len(r.members) == 0 {
		e.closeLocked(r, reason)
		return
	}
	if m.host {
		r.members[0].host = true
	}
	r.generation++
	r.updated = e.now()
	for _, o := range r.members {
		if o.protocol == "jellyfin" {
			e.emit(o, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, o), GroupKind: "UserLeft", GroupData: e.jfUser(m.viewer.ID)})
		} else {
			e.emit(o, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, o)})
		}
	}
}

func (e *Engine) announceLocked(r *room, m *member, joined bool) {
	for _, o := range r.members {
		if o.protocol == "jellyfin" {
			if o == m && joined {
				e.emit(o, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, o), GroupKind: "GroupJoined", GroupData: e.groupInfo(r)})
				if len(r.queue) > 0 {
					e.emit(o, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, o), GroupKind: "PlayQueue", GroupData: e.queuePayload(r, "NewPlaylist")})
				}
			} else if joined {
				e.emit(o, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, o), GroupKind: "UserJoined", GroupData: e.jfUser(m.viewer.ID)})
			}
		} else {
			e.emit(o, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, o)})
		}
	}
}

func (e *Engine) catchupLocked(r *room, m *member) {
	if m.protocol == "jellyfin" {
		e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "GroupJoined", GroupData: e.groupInfo(r)})
		if len(r.queue) > 0 {
			e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "PlayQueue", GroupData: e.queuePayload(r, "NewPlaylist")})
		}
		if r.last != nil && r.last.ExecuteAt.After(e.now()) {
			cmd := *r.last
			cmd.SessionID = m.session
			e.emit(m, frontend.PartyEvent{Type: "command", View: e.viewLocked(r, m), Command: cmd})
		}
		return
	}
	e.emit(m, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, m)})
	if r.last != nil && (r.phase == "playing" || r.last.ExecuteAt.After(e.now())) {
		cmd := *r.last
		cmd.SessionID = m.session
		e.emit(m, frontend.PartyEvent{Type: "transport", View: e.viewLocked(r, m), Command: cmd})
	}
}

func (e *Engine) snapshotsLocked(r *room) {
	for _, m := range r.members {
		if m.protocol == "silo" || m.protocol == "" {
			e.emit(m, frontend.PartyEvent{Type: "snapshot", View: e.viewLocked(r, m)})
		}
	}
}

func (e *Engine) stateLocked(r *room, reason string) {
	for _, m := range r.members {
		if m.protocol == "jellyfin" {
			e.emit(m, frontend.PartyEvent{Type: "group", View: e.viewLocked(r, m), GroupKind: "StateUpdate", GroupData: map[string]any{"State": jfState(r), "Reason": reason}})
		}
	}
}

func (e *Engine) groupInfo(r *room) map[string]any {
	parts := make([]any, 0, len(r.members))
	for _, m := range r.members {
		if m.viewer != nil {
			parts = append(parts, e.jfUser(m.viewer.ID))
		}
	}
	return map[string]any{
		"GroupId": r.id, "GroupName": r.name, "State": jfState(r),
		"Participants": parts, "LastUpdatedAt": r.updated.UTC().Format(time.RFC3339Nano),
	}
}

func jfState(r *room) string {
	switch r.play {
	case "waiting":
		return "Waiting"
	case "paused":
		return "Paused"
	case "playing":
		return "Playing"
	default:
		return "Idle"
	}
}

func (e *Engine) emit(m *member, ev frontend.PartyEvent) {
	if m == nil || m.viewer == nil {
		return
	}
	e.sendSubLocked(frontend.PartyActor{Viewer: m.viewer, Device: m.device, Protocol: m.protocol}, ev)
}

func (e *Engine) sendSubLocked(actor frontend.PartyActor, ev frontend.PartyEvent) {
	s := e.subs[actorKey(actor)]
	if s == nil {
		return
	}
	send(s.ch, ev)
}

func (e *Engine) deny(actor frontend.PartyActor, kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sendSubLocked(actor, frontend.PartyEvent{Type: "group", GroupKind: kind, GroupData: map[string]any{}})
}

func send(ch chan frontend.PartyEvent, ev frontend.PartyEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

func (e *Engine) viewLocked(r *room, self *member) frontend.PartyView {
	now := e.now()
	hostUp := false
	members := make([]frontend.PartyMember, 0, len(r.members))
	for _, m := range r.members {
		if m.host && m.connected {
			hostUp = true
		}
		name := ""
		var id int64
		if m.viewer != nil {
			id = m.viewer.ID
			name = m.viewer.Title
			if name == "" {
				name = m.viewer.Username
			}
		}
		members = append(members, frontend.PartyMember{
			ViewerID: id, Name: name, Protocol: m.protocol, DeviceID: deviceID(m.device),
			Host: m.host, Self: m == self, Connected: m.connected, Ready: m.ready,
			Buffering: m.buffering, Lobby: m.lobby, IgnoreWait: m.ignore, Blocked: m.blocked,
		})
	}
	queue := make([]frontend.PartyQueueItem, 0, len(r.queue))
	for _, q := range r.queue {
		queue = append(queue, frontend.PartyQueueItem{Item: q.item, ContentID: e.itemID(q.item), PlaylistItemID: q.id})
	}
	role := "guest"
	canT, canM, ignore := false, false, false
	session := ""
	token := ""
	if self != nil {
		if self.host {
			role = "host"
		}
		canT = r.canTransport(self)
		canM = self.host
		ignore = self.ignore
		session = self.session
		token = self.token
	}
	return frontend.PartyView{
		RoomID: r.id, Code: r.code, JoinToken: r.invite, RoomToken: token,
		InvitePath: "/rooms/join?token=" + url.QueryEscape(r.invite), Name: r.name,
		Phase: r.phase, Playback: r.play, SelectionMode: r.mode, GuestPolicy: r.policy, Role: role,
		SelectionRevision: r.revision, Generation: r.generation, Item: r.item, ContentID: e.itemID(r.item),
		FileID: r.fileID, LibraryID: r.libraryID, Paused: r.paused, Position: r.positionAt(now),
		AnchorAt: r.anchorAt, Rate: r.rate, HostConnected: hostUp, CanTransport: canT, CanManage: canM,
		IgnoreWait: ignore, AttachedSessionID: session, Members: members, Repeat: r.repeat, Shuffle: r.shuffle,
		Queue: queue, QueueIndex: r.index, UpdatedAt: r.updated,
	}
}

func (e *Engine) freshCodeLocked() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "ETHER" + strconv.FormatInt(e.now().Unix()%1000, 10)
		}
		buf := make([]byte, 8)
		for i, b := range raw {
			buf[i] = alphabet[int(b)%len(alphabet)]
		}
		code := string(buf)
		if _, used := e.codes[code]; !used {
			return code
		}
	}
}

func secret() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func canonicalID(id string) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return "", frontend.ErrPartyNotFound
	}
	return parsed.String(), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var _ frontend.WatchParty = (*Engine)(nil)
