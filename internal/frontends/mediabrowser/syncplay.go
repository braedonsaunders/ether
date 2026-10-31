package mediabrowser

import (
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) syncPlayRoutes() error {
	if i.protocol != "jellyfin" || i.host.WatchParty == nil {
		return nil
	}
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{"GET", "/GetUtcTime", i.utcTime},
		{"POST", "/SyncPlay/New", i.syncNew},
		{"POST", "/SyncPlay/Join", i.syncJoin},
		{"POST", "/SyncPlay/Leave", i.syncLeave},
		{"GET", "/SyncPlay/List", i.syncList},
		{"POST", "/SyncPlay/Pause", i.syncPause},
		{"POST", "/SyncPlay/Unpause", i.syncUnpause},
		{"POST", "/SyncPlay/Seek", i.syncSeek},
		{"POST", "/SyncPlay/Stop", i.syncStop},
		{"POST", "/SyncPlay/Buffering", i.syncBuffer},
		{"POST", "/SyncPlay/Ready", i.syncReady},
		{"POST", "/SyncPlay/Ping", i.syncPing},
		{"POST", "/SyncPlay/SetIgnoreWait", i.syncIgnore},
		{"POST", "/SyncPlay/SetNewQueue", i.syncQueue},
		{"POST", "/SyncPlay/SetPlaylistItem", i.syncPlaylistItem},
		{"POST", "/SyncPlay/RemoveFromPlaylist", i.syncRemove},
		{"POST", "/SyncPlay/MovePlaylistItem", i.syncMove},
		{"POST", "/SyncPlay/Queue", i.syncAppend},
		{"POST", "/SyncPlay/NextItem", i.syncNext},
		{"POST", "/SyncPlay/PreviousItem", i.syncPrev},
		{"POST", "/SyncPlay/SetRepeatMode", i.syncRepeat},
		{"POST", "/SyncPlay/SetShuffleMode", i.syncShuffle},
	}
	for _, route := range routes {
		if err := i.router.Handle(route.method, route.path, route.h); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) utcTime(w http.ResponseWriter, _ *http.Request) {
	received := time.Now().UTC()
	sent := time.Now().UTC()
	jsonReply(w, map[string]any{
		"RequestReceptionTime":     received.Format(time.RFC3339Nano),
		"ResponseTransmissionTime": sent.Format(time.RFC3339Nano),
	})
}

func (i *instance) syncActor(r *http.Request) (frontend.PartyActor, int) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		return frontend.PartyActor{}, http.StatusUnauthorized
	}
	if i.host.WatchParty == nil {
		return frontend.PartyActor{}, http.StatusNotFound
	}
	return frontend.PartyActor{Viewer: v, Device: device(r), Protocol: i.protocol}, 0
}

func (i *instance) syncRoom(r *http.Request, actor frontend.PartyActor) (frontend.PartyView, error) {
	for _, view := range i.host.WatchParty.List(r.Context(), actor) {
		for _, member := range view.Members {
			if member.Self {
				return view, nil
			}
		}
	}
	return frontend.PartyView{}, frontend.ErrPartyNotFound
}

func (i *instance) syncNew(w http.ResponseWriter, r *http.Request) {
	actor, status := i.syncActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	var body struct {
		GroupName string
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if _, err := i.host.WatchParty.Create(r.Context(), actor, frontend.PartyCreate{Name: body.GroupName}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncJoin(w http.ResponseWriter, r *http.Request) {
	actor, status := i.syncActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	var body struct {
		GroupId string
	}
	if decode(r, &body) != nil || body.GroupId == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if _, err := i.host.WatchParty.Join(r.Context(), actor, frontend.PartyJoin{GroupID: body.GroupId}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncLeave(w http.ResponseWriter, r *http.Request) {
	actor, status := i.syncActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if err := i.host.WatchParty.Leave(r.Context(), actor); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncList(w http.ResponseWriter, r *http.Request) {
	actor, status := i.syncActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	rows := []any{}
	for _, view := range i.host.WatchParty.List(r.Context(), actor) {
		parts := []any{}
		for _, member := range view.Members {
			id, err := ids.GUID(ids.User, member.ViewerID)
			if err != nil {
				continue
			}
			parts = append(parts, id)
		}
		rows = append(rows, map[string]any{
			"GroupId": view.RoomID, "GroupName": view.Name, "State": jellyfinState(view.Playback),
			"Participants": parts, "LastUpdatedAt": view.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	jsonReply(w, rows)
}

func jellyfinState(play string) string {
	switch play {
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

func (i *instance) syncPause(w http.ResponseWriter, r *http.Request) {
	i.syncTransport(w, r, "pause", 0)
}

func (i *instance) syncUnpause(w http.ResponseWriter, r *http.Request) {
	i.syncTransport(w, r, "play", 0)
}

func (i *instance) syncSeek(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PositionTicks int64
	}
	if decode(r, &body) != nil || body.PositionTicks < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	i.syncTransport(w, r, "seek", time.Duration(body.PositionTicks)*100)
}

func (i *instance) syncTransport(w http.ResponseWriter, r *http.Request, action string, pos time.Duration) {
	actor, view, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	if err := i.host.WatchParty.Transport(r.Context(), actor, view.RoomID, "", frontend.PartyInput{Action: action, Position: pos}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncStop(w http.ResponseWriter, r *http.Request) {
	actor, view, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	if _, err := i.host.WatchParty.Stop(r.Context(), actor, view.RoomID, ""); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncBuffer(w http.ResponseWriter, r *http.Request) {
	i.syncReport(w, r, "buffering")
}

func (i *instance) syncReady(w http.ResponseWriter, r *http.Request) {
	i.syncReport(w, r, "ready")
}

func (i *instance) syncReport(w http.ResponseWriter, r *http.Request, kind string) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		PositionTicks  int64
		IsPlaying      bool
		PlaylistItemId string
		When           time.Time
	}
	if r.ContentLength != 0 && decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := i.host.WatchParty.Report(r.Context(), actor, frontend.PartyReport{
		Kind: kind, Position: time.Duration(body.PositionTicks) * 100, Playing: body.IsPlaying,
		Playlist: body.PlaylistItemId, When: body.When,
	}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncPing(w http.ResponseWriter, r *http.Request) {
	if _, status := i.syncActor(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncIgnore(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		IgnoreWait bool
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	ignore := body.IgnoreWait
	if err := i.host.WatchParty.Report(r.Context(), actor, frontend.PartyReport{Kind: "ignore", Ignore: &ignore}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncQueue(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		PlayingQueue        []string
		PlayingItemPosition int
		StartPositionTicks  int64
	}
	if decode(r, &body) != nil || len(body.PlayingQueue) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	items := make([]frontend.RatingKey, 0, len(body.PlayingQueue))
	for _, id := range body.PlayingQueue {
		key, err := ids.ParseRatingKeyGUID(id)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		items = append(items, key)
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{
		Kind: "replace", Items: items, Index: body.PlayingItemPosition, Position: time.Duration(body.StartPositionTicks) * 100,
	}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncPlaylistItem(w http.ResponseWriter, r *http.Request) {
	i.syncOne(w, r, "item")
}

func (i *instance) syncRemove(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		PlaylistItemIds []string
		ClearPlaylist   bool
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if body.ClearPlaylist {
		view, err := i.syncRoom(r, actor)
		if err != nil {
			i.syncError(w, err)
			return
		}
		for _, item := range view.Queue {
			if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: "remove", PlaylistItemID: item.PlaylistItemID}); err != nil {
				i.syncError(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, id := range body.PlaylistItemIds {
		if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: "remove", PlaylistItemID: id}); err != nil {
			i.syncError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncMove(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		PlaylistItemId string
		NewIndex       int
	}
	if decode(r, &body) != nil || body.PlaylistItemId == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: "move", PlaylistItemID: body.PlaylistItemId, Index: body.NewIndex}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncAppend(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		ItemIds []string
	}
	if decode(r, &body) != nil || len(body.ItemIds) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	items := make([]frontend.RatingKey, 0, len(body.ItemIds))
	for _, id := range body.ItemIds {
		key, err := ids.ParseRatingKeyGUID(id)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		items = append(items, key)
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: "append", Items: items}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncNext(w http.ResponseWriter, r *http.Request) {
	i.syncStep(w, r, "next")
}

func (i *instance) syncPrev(w http.ResponseWriter, r *http.Request) {
	i.syncStep(w, r, "prev")
}

func (i *instance) syncStep(w http.ResponseWriter, r *http.Request, kind string) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: kind}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncRepeat(w http.ResponseWriter, r *http.Request) {
	i.syncMode(w, r, true)
}

func (i *instance) syncShuffle(w http.ResponseWriter, r *http.Request) {
	i.syncMode(w, r, false)
}

func (i *instance) syncMode(w http.ResponseWriter, r *http.Request, repeat bool) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		Mode string
	}
	if decode(r, &body) != nil || body.Mode == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	op := frontend.PartyQueue{Kind: "shuffle", Shuffle: body.Mode}
	if repeat {
		op = frontend.PartyQueue{Kind: "repeat", Repeat: body.Mode}
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, op); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) syncOne(w http.ResponseWriter, r *http.Request, kind string) {
	actor, _, ok := i.requireParty(w, r)
	if !ok {
		return
	}
	var body struct {
		PlaylistItemId string
	}
	if decode(r, &body) != nil || body.PlaylistItemId == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := i.host.WatchParty.Queue(r.Context(), actor, frontend.PartyQueue{Kind: kind, PlaylistItemID: body.PlaylistItemId}); err != nil {
		i.syncError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) requireParty(w http.ResponseWriter, r *http.Request) (frontend.PartyActor, frontend.PartyView, bool) {
	actor, status := i.syncActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return frontend.PartyActor{}, frontend.PartyView{}, false
	}
	view, err := i.syncRoom(r, actor)
	if err != nil {
		i.syncError(w, err)
		return frontend.PartyActor{}, frontend.PartyView{}, false
	}
	return actor, view, true
}

func (i *instance) syncError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrPartyNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, frontend.ErrPartyConflict):
		http.Error(w, "Conflict", http.StatusConflict)
	case errors.Is(err, frontend.ErrPartyDenied), errors.Is(err, frontend.ErrPartyForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	default:
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
	}
}

func syncPlayFrame(ev frontend.PartyEvent) (map[string]any, bool) {
	switch ev.Type {
	case "group":
		return map[string]any{"MessageType": "SyncPlayGroupUpdate", "Data": map[string]any{
			"GroupId": ev.View.RoomID, "Type": ev.GroupKind, "Data": ev.GroupData,
		}}, true
	case "command":
		return map[string]any{"MessageType": "SyncPlayCommand", "Data": map[string]any{
			"GroupId": ev.View.RoomID, "PlaylistItemId": ev.Command.PlaylistItemID,
			"When": ev.Command.ExecuteAt.UTC().Format(time.RFC3339Nano), "PositionTicks": ev.Command.Position.Nanoseconds() / 100,
			"Command": ev.Command.Jellyfin, "EmittedAt": ev.Command.IssuedAt.UTC().Format(time.RFC3339Nano),
		}}, true
	default:
		return nil, false
	}
}
