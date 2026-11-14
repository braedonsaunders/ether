package vela

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type episodeArtworkExperience struct {
	frontendtest.Experience
	images  map[core.RatingKey][]frontend.Image
	batches int
}

func (e *episodeArtworkExperience) ItemArtworkPage(_ context.Context, v *core.Viewer, d core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Image, error) {
	e.batches++
	if v.ID != 1 || d.ID != "tv" {
		return nil, core.ErrUnauthorized
	}
	out := map[core.RatingKey][]frontend.Image{}
	for _, key := range keys {
		out[key] = e.images[key]
	}
	return out, nil
}

type episodeArtworkReader struct{ frontendtest.Artwork }

func (episodeArtworkReader) Read(_ context.Context, req frontend.ArtworkRequest, image frontend.Image, _ frontend.ArtworkTransform) ([]byte, string, error) {
	if req.Viewer.ID != 1 || req.Device.ID != "tv" {
		return nil, "", core.ErrUnauthorized
	}
	return []byte(image.Path), "image/jpeg", nil
}

// Exercise selection through the real batch signer and image server: distinct
// episode stills, truthful fallback kinds, and policy rechecks on the capability.
func TestVelaEpisodeArtwork(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, castMembers{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "vela", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	exp := &episodeArtworkExperience{images: map[core.RatingKey][]frontend.Image{}}
	titles := []frontend.Title{}
	for n := 1; n <= 5; n++ {
		key := core.EpisodeKey(42, 1, n)
		titles = append(titles, frontend.Title{Key: key})
		if n < 5 {
			exp.images[key] = []frontend.Image{{Kind: frontend.ImagePoster, Path: "poster"}}
		}
		if n < 4 {
			exp.images[key] = append(exp.images[key], frontend.Image{Kind: frontend.ImageBackdrop, Path: "show-art"})
		}
		if n < 3 {
			exp.images[key] = append(exp.images[key], frontend.Image{Kind: frontend.ImageStill, Path: []string{"still-one", "still-two"}[n-1]})
		}
	}
	exp.ItemArtworkFunc = func(_ context.Context, v *core.Viewer, d core.Device, k core.RatingKey) ([]frontend.Image, bool, error) {
		if v.ID != 1 || d.ID != "tv" {
			return nil, false, core.ErrUnauthorized
		}
		return exp.images[k], true, nil
	}
	service, err := identity.NewArtworkImageGrantService(db, "vela", grants, func() frontend.Experience { return exp }, episodeArtworkReader{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{host: frontend.Host{Experience: exp, ImageGrants: service}}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/titles/season/children", nil)
	r.Header.Set("X-Vela-Device-Id", "tv")
	out, err := i.titles(r, &frontend.Viewer{ID: 1}, credential, titles)
	if err != nil {
		t.Fatal(err)
	}
	if exp.batches != 1 {
		t.Fatalf("artwork batches=%d", exp.batches)
	}
	for n, item := range out {
		art := item["artwork"].([]object)
		if n == 4 {
			if len(art) != 0 {
				t.Fatal("invented missing artwork")
			}
			continue
		}
		want := []string{"still", "still", "backdrop", "poster"}[n]
		if len(art) != 1 || art[0]["kind"] != want {
			t.Fatalf("episode %d artwork=%v", n+1, art)
		}
		u, err := url.Parse(art[0]["sizes"].([]object)[0]["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		secret := strings.TrimPrefix(u.Path, "/vela/v0/images/")
		serve := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, u.String(), nil)
			req.SetPathValue("secret", secret)
			w := httptest.NewRecorder()
			i.image(w, req)
			return w
		}
		w := serve()
		expected := []string{"still-one", "still-two", "show-art", "poster"}[n]
		if w.Code != 200 || w.Body.String() != expected {
			t.Fatalf("episode %d image=%d %q", n+1, w.Code, w.Body.String())
		}
		delete(exp.images, titles[n].Key)
		if w := serve(); w.Code != 404 {
			t.Fatalf("lost artwork still served: %d", w.Code)
		}
	}
}
