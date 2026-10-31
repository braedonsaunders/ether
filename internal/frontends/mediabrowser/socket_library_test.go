package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/coder/websocket"
)

func TestSocketLibraryChangedNamesTheWatchlistFolder(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	events := make(socketEvents, 1)
	exp := frontendtest.Experience{UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	item := core.MovieKey(3)
	events <- frontend.Event{Type: "LibraryChanged", ViewerID: 1, Item: item}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := ids.GUID(ids.View, personalViewNumber(1, "watchlist.available"))
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		MessageType string
		Data        struct {
			ItemsAdded        []string
			CollectionFolders []string
		}
	}
	if json.Unmarshal(b, &msg) != nil || msg.MessageType != "LibraryChanged" || len(msg.Data.ItemsAdded) != 1 || msg.Data.ItemsAdded[0] != ids.RatingKeyGUID(item) || len(msg.Data.CollectionFolders) != 1 || msg.Data.CollectionFolders[0] != folder {
		t.Fatalf("%s", b)
	}
}
