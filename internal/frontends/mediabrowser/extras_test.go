package mediabrowser

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestExtrasRoutesAuthorizeParentAndReturnEmptyArrays(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("denied")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != viewer || req.Key != 5 {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: 5}}, nil
	}}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range []string{"LocalTrailers", "SpecialFeatures"} {
			path := "/Items/" + ids.RatingKeyGUID(5) + "/" + route
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("X-Emby-Token", "valid")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
				t.Fatal(tc.name, path, w.Code, w.Body.String())
			}
			spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", tc.name, "")
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
				t.Fatalf("%s %s: %v", tc.name, path, err)
			}
		}
		for _, tc := range []struct {
			key   frontend.RatingKey
			token string
			want  int
		}{{5, "bad", 401}, {6, "valid", 404}} {
			r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(tc.key)+"/LocalTrailers", nil)
			r.Header.Set("X-Emby-Token", tc.token)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code)
			}
		}
	}
}
