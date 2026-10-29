package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestCreditsUseParentAuthorizedFacetAndPortrait(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	personID, _ := ids.GUID(ids.User, 12)
	grants := &imageGrantsFake{}
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie", Cast: []core.Person{{TMDB: 12, Name: "Actor", Role: "Hero"}, {TMDB: 13, Name: "Hidden"}}, Directors: []core.Person{{TMDB: 12, Name: "Actor"}}}, ImagesAuthoritative: true, Images: []frontend.Image{{Kind: frontend.ImagePerson, Subject: "tmdb:12", Path: "/poster"}, {Kind: frontend.ImagePerson, Subject: "tmdb:13", Path: "/hidden"}}}, nil
	}, PersonFacetIDsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, requested []int64) (map[int64]string, error) {
		if v != viewer || key != 5 || len(requested) != 2 {
			t.Fatal(v, key, requested)
		}
		return map[int64]string{12: personID}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, ImageGrants: grants})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
	r.Header.Set("X-Emby-Token", "grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	var got struct {
		People []struct{ Id, Name, Role, Type, PrimaryImageTag string }
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.People) != 2 || got.People[0].Id != personID || got.People[0].PrimaryImageTag != "scoped-image/poster" || got.People[1].Type != "Director" || grants.issued != 1 {
		t.Fatal(w.Code, w.Body.String(), grants.issued)
	}
}
