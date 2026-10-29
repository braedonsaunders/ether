package mediabrowser

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
	stop := context.AfterFunc(i.life, cancel)
	defer stop()
	defer cancel()
	write := func(v map[string]any) error {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		id[6] = (id[6] & 0x0f) | 0x40
		id[8] = (id[8] & 0x3f) | 0x80
		v["MessageId"] = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		wc, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		return conn.Write(wc, websocket.MessageText, b)
	}
	var events <-chan frontend.Event
	if i.host.Events != nil {
		events, _ = i.host.Events.Subscribe(ctx, frontend.EventScope{Viewer: viewer, Protocol: i.protocol, Device: device(r)})
	}
	var keepAlive any = 60
	if i.protocol == "emby" {
		keepAlive = "0,30"
	}
	if write(map[string]any{"MessageType": "ForceKeepAlive", "Data": keepAlive}) != nil {
		return
	}
	var sessionEvents <-chan frontend.Event
	var cancelSessions context.CancelFunc
	defer func() {
		if cancelSessions != nil {
			cancelSessions()
		}
	}()
	writeSessions := func() error {
		if i.host.Sessions == nil {
			return nil
		}
		rows, err := i.sessionRows(ctx, viewer, device(r))
		if err != nil {
			return err
		}
		return write(map[string]any{"MessageType": "Sessions", "Data": rows})
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
			} else if msg == "SessionsStart" {
				if cancelSessions != nil {
					cancelSessions()
					cancelSessions = nil
					sessionEvents = nil
				}
				if i.host.Events != nil {
					sessionCtx, stopSessions := context.WithCancel(ctx)
					ch, err := i.host.Events.Subscribe(sessionCtx, frontend.EventScope{Viewer: viewer, Owner: viewer.IsOwner && !viewer.Delegated, Protocol: i.protocol})
					if err != nil {
						stopSessions()
					} else {
						sessionEvents, cancelSessions = ch, stopSessions
					}
				}
				if writeSessions() != nil {
					return
				}
			} else if msg == "SessionsStop" {
				if cancelSessions != nil {
					cancelSessions()
					cancelSessions = nil
				}
				sessionEvents = nil
			}
		case event, ok := <-sessionEvents:
			if !ok {
				sessionEvents = nil
				continue
			}
			if event.Type == "SessionStarted" || event.Type == "SessionUpdated" || event.Type == "SessionStopped" {
				if writeSessions() != nil {
					return
				}
			}
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Type == "SessionCommand" {
				dev := device(r)
				if event.Command == nil || dev.ID == "" || event.TargetProtocol != i.protocol || event.TargetDeviceID != dev.ID {
					continue
				}
				command := map[frontend.CommandKind]string{frontend.CommandStop: "Stop", frontend.CommandPause: "Pause", frontend.CommandUnpause: "Unpause", frontend.CommandSeek: "Seek"}[event.Command.Kind]
				if command != "" {
					data := map[string]any{"Command": command}
					if event.Command.Kind == frontend.CommandSeek {
						data["SeekPositionTicks"] = event.Command.Position.Nanoseconds() / 100
					}
					if event.ActorViewerID > 0 {
						if actor, err := ids.GUID(ids.User, event.ActorViewerID); err == nil {
							data["ControllingUserId"] = actor
						}
					}
					if write(map[string]any{"MessageType": "Playstate", "Data": data}) != nil {
						return
					}
					continue
				}
				if event.ActorViewerID <= 0 {
					continue
				}
				actor, err := ids.GUID(ids.User, event.ActorViewerID)
				if err != nil {
					continue
				}
				name := ""
				arguments := map[string]string{}
				switch event.Command.Kind {
				case frontend.CommandSetAudio:
					name = "SetAudioStreamIndex"
					arguments["Index"] = strconv.Itoa(event.Command.StreamIndex)
				case frontend.CommandSetSubtitle:
					name = "SetSubtitleStreamIndex"
					arguments["Index"] = strconv.Itoa(event.Command.StreamIndex)
				case frontend.CommandMessage:
					name = "DisplayMessage"
					arguments["Header"] = "Ether"
					arguments["Text"] = event.Command.Message
				}
				if name != "" && write(map[string]any{"MessageType": "GeneralCommand", "Data": map[string]any{"Name": name, "ControllingUserId": actor, "Arguments": arguments}}) != nil {
					return
				}
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
			dataDTO := i.userDataDTO(id, st)
			dataDTO["Key"] = id
			dataDTO["ItemId"] = id
			payload := map[string]any{"UserId": userID, "UserDataList": []any{dataDTO}}
			if write(map[string]any{"MessageType": "UserDataChanged", "Data": payload}) != nil {
				return
			}
		}
	}
}
