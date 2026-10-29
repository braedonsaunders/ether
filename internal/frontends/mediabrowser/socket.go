package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/coder/websocket"
)

func (i *instance) socket(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	write := func(v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		wc, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		return conn.Write(wc, websocket.MessageText, b)
	}
	if write(map[string]any{"MessageType": "ForceKeepAlive", "Data": "0,30"}) != nil {
		return
	}
	incoming := make(chan string, 1)
	go func() {
		defer close(incoming)
		for {
			rc, done := context.WithTimeout(ctx, 60*time.Second)
			_, b, e := conn.Read(rc)
			done()
			if e != nil {
				return
			}
			var msg struct{ MessageType string }
			if json.Unmarshal(b, &msg) == nil {
				select {
				case incoming <- msg.MessageType:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	var events <-chan frontend.Event
	if i.host.Events != nil {
		events, _ = i.host.Events.Subscribe(ctx, frontend.EventScope{Viewer: viewer})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-incoming:
			if !ok {
				return
			}
			if msg == "KeepAlive" {
				if write(map[string]any{"MessageType": "KeepAlive"}) != nil {
					return
				}
			}
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Type != "UserDataChanged" || i.host.Experience == nil {
				continue
			}
			data, e := i.host.Experience.UserData(ctx, viewer, []frontend.RatingKey{event.Item})
			if e != nil {
				continue
			}
			st, ok := data[event.Item]
			if !ok {
				continue
			}
			id := ids.RatingKeyGUID(event.Item)
			userID, _ := ids.GUID(ids.User, viewer.ID)
			payload := map[string]any{"UserId": userID, "UserDataList": []any{map[string]any{"ItemId": id, "PlaybackPositionTicks": st.ViewOffset.Nanoseconds() / 100, "PlayCount": st.ViewCount, "Played": st.ViewCount > 0, "Rating": st.UserRating}}}
			if write(map[string]any{"MessageType": "UserDataChanged", "Data": payload}) != nil {
				return
			}
		}
	}
}
