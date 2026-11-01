package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestAuthorizedCollectionAndPlaylistDetails(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	collection := core.CollectionKey(8)
	playlist := core.PlaylistKey(9)
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != viewer {
			t.Fatal("wrong viewer")
		}
		switch req.Key {
		case collection:
			return frontend.ItemDetail{Collection: &core.Collection{Key: collection, Title: "Collection", ChildCount: 3}}, nil
		case playlist:
			return frontend.ItemDetail{Playlist: &core.Playlist{Key: playlist, Title: "Playlist", Items: []core.RatingKey{5, 6}}}, nil
		default:
			return frontend.ItemDetail{Collection: &core.Collection{Key: collection}}, nil
		}
	}}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, dialect := range []struct {
		frontend frontend.Frontend
		protocol string
	}{{Jellyfin{MachineID: "test"}, "jellyfin"}, {Emby{MachineID: "test"}, "emby"}} {
		inst, err := dialect.frontend.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			key        frontend.RatingKey
			kind, name string
			count      float64
		}{{collection, "BoxSet", "Collection", 3}, {playlist, "Playlist", "Playlist", 2}} {
			path := "/Items/" + ids.RatingKeyGUID(tc.key)
			if dialect.protocol == "emby" {
				uid, _ := ids.GUID(ids.User, viewer.ID)
				path = "/Users/" + uid + path
			}
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("X-Emby-Token", "grant")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			var dto map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil || dto["Id"] != ids.RatingKeyGUID(tc.key) || dto["Type"] != tc.kind || dto["Name"] != tc.name || dto["ChildCount"] != tc.count {
				t.Fatal(w.Code, w.Body.String())
			}
			spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", dialect.protocol, "")
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			doc, err := openapi.Parse(spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatalf("%s %s: %v", dialect.protocol, tc.kind, err)
			}
		}
		r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(123), nil)
		r.Header.Set("X-Emby-Token", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("mismatched collection allowed", w.Code)
		}
	}
}
