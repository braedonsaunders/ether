package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/watchparty"
	"github.com/coder/websocket"
)

func TestSyncPlayGroupCommandAndEmbyHasNoRoute(t *testing.T) {
	ada := &frontend.Viewer{ID: 3, Username: "ada"}
	bea := &frontend.Viewer{ID: 4, Username: "bea"}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		switch token {
		case "ada":
			return ada, nil
		case "bea":
			return bea, nil
		default:
			return nil, errors.New("bad")
		}
	}}
	engine := watchparty.New(watchparty.Options{
		Allow: func(_ context.Context, _ frontend.Protocol, v *core.Viewer, _ core.RatingKey) error {
			if v.ID == bea.ID {
				return core.ErrNotFound
			}
			return nil
		},
		ItemID:       ids.RatingKeyGUID,
		JellyfinUser: func(id int64) string { value, _ := ids.GUID(ids.User, id); return value },
		UseLead:      true,
		Lead:         time.Second,
	})
	jf, err := (Jellyfin{MachineID: "party"}).Mount(context.Background(), frontend.Host{Identity: ident, WatchParty: engine})
	if err != nil {
		t.Fatal(err)
	}
	emby, err := (Emby{MachineID: "party"}).Mount(context.Background(), frontend.Host{Identity: ident, WatchParty: engine})
	if err != nil {
		t.Fatal(err)
	}
	missing := syncCall(emby, "POST", "/SyncPlay/New", "ada", `{"GroupName":"nope"}`)
	if missing.Code != 404 {
		t.Fatal(missing.Code, missing.Body.String())
	}
	user := (&instance{protocol: "emby", host: frontend.Host{WatchParty: engine}}).userDTO(httptest.NewRequest("GET", "/", nil), ada)
	if _, ok := user["Policy"].(map[string]any)["SyncPlayAccess"]; ok {
		t.Fatal(user["Policy"])
	}
	jellyUser := (&instance{protocol: "jellyfin", host: frontend.Host{WatchParty: engine}}).userDTO(httptest.NewRequest("GET", "/", nil), ada)
	if jellyUser["Policy"].(map[string]any)["SyncPlayAccess"] != "CreateAndJoinGroups" {
		t.Fatal(jellyUser["Policy"])
	}
	clock := syncCall(jf, "GET", "/GetUtcTime", "", "")
	if clock.Code != 200 || !strings.Contains(clock.Body.String(), "RequestReceptionTime") {
		t.Fatal(clock.Code, clock.Body.String())
	}
	srv := httptest.NewServer(jf.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=ada", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	created := syncCall(jf, "POST", "/SyncPlay/New", "ada", `{"GroupName":"Friday"}`)
	if created.Code != 204 {
		t.Fatal(created.Code, created.Body.String())
	}
	joined := readSync(t, ctx, conn, "GroupJoined")
	var msg struct {
		Data struct {
			Type string
			Data struct {
				GroupId, GroupName, State string
			}
		}
	}
	if json.Unmarshal(joined, &msg) != nil || msg.Data.Type != "GroupJoined" || msg.Data.Data.GroupName != "Friday" || msg.Data.Data.State != "Idle" {
		t.Fatal(string(joined))
	}
	item := ids.RatingKeyGUID(9)
	queued := syncCall(jf, "POST", "/SyncPlay/SetNewQueue", "ada", `{"PlayingQueue":["`+item+`"],"PlayingItemPosition":0,"StartPositionTicks":0}`)
	if queued.Code != 204 {
		t.Fatal(queued.Code, queued.Body.String())
	}
	if !strings.Contains(string(readSync(t, ctx, conn, "PlayQueue")), item) {
		t.Fatal("queue")
	}
	if syncCall(jf, "POST", "/SyncPlay/Ready", "ada", `{"PositionTicks":0,"IsPlaying":false,"PlaylistItemId":""}`).Code != 204 {
		t.Fatal("ready")
	}
	command := readSync(t, ctx, conn, "SyncPlayCommand")
	if !strings.Contains(string(command), `"Command":"Unpause"`) || !strings.Contains(string(command), `"PositionTicks":0`) {
		t.Fatal(string(command))
	}
	denied := syncCall(jf, "POST", "/SyncPlay/Join", "bea", `{"GroupId":"`+msg.Data.Data.GroupId+`"}`)
	if denied.Code != 403 || strings.Contains(denied.Body.String(), item) {
		t.Fatal(denied.Code, denied.Body.String())
	}
	if syncCall(jf, "POST", "/SyncPlay/Leave", "ada", "").Code != 204 {
		t.Fatal("leave")
	}
	if syncCall(jf, "POST", "/SyncPlay/Pause", "ada", "").Code != 404 {
		t.Fatal("pause after leave")
	}
}

func readSync(t *testing.T, ctx context.Context, conn *websocket.Conn, kind string) []byte {
	t.Helper()
	for {
		_, body, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), kind) {
			return body
		}
	}
}

func syncCall(inst frontend.Instance, method, path, token, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	if token != "" {
		r.Header.Set("Authorization", `MediaBrowser Token="`+token+`"`)
	}
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	return w
}
