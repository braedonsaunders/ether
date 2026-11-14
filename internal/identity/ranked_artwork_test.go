package identity_test

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRankedImageGrantPolicyAndRestart(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	base := frontend.Image{Kind: frontend.ImagePoster, Path: "primary", Tag: "v1"}
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		if !allowed {
			return frontend.ItemDetail{}, core.ErrUnauthorized
		}
		return frontend.ItemDetail{Images: []frontend.Image{base}, ImagesAuthoritative: true}, nil
	}}
	art := &scopedArtworkFixture{}
	fresh := func() *identity.ImageGrantService {
		s, e := identity.NewArtworkImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return exp }, art, time.Hour, nil, identity.ImageGrantOptions{Token: authhdr.MediaBrowserToken})
		if e != nil {
			t.Fatal(e)
		}
		s.SetImageKey([]byte("ranked-test-node-key"))
		return s
	}
	key := core.MovieKey(9)
	rows, err := fresh().SignPage(ctx, frontend.ImagePageRequest{Credential: credential, Titles: []core.Title{{Key: key, Type: core.TypeMovie}}, Ranked: map[core.RatingKey]int{key: 10}, Kinds: []string{frontend.ImagePoster, frontend.ImageThumb}})
	if err != nil || len(rows) != 1 || len(rows[0].Images) != 2 {
		t.Fatal(rows, err)
	}
	tag := rows[0].Images[0].Secret
	req := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(key)+"/Images/Primary?api_key="+credential, nil)
	out := httptest.NewRecorder()
	fresh().Serve(out, req, tag)
	if out.Code != 200 {
		t.Fatalf("restart image status=%d: %s", out.Code, out.Body.String())
	}
	_, rank, wide, err := frontend.UnrankArtwork(art.image)
	if err != nil || rank != 10 || wide {
		t.Fatalf("wrong presentation: %+v %v", art.image, err)
	}

	thumbReq := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(key)+"/Images/Thumb?api_key="+credential, nil)
	thumbOut := httptest.NewRecorder()
	fresh().Serve(thumbOut, thumbReq, rows[0].Images[1].Secret)
	if thumbOut.Code != 200 {
		t.Fatalf("wide thumb status=%d", thumbOut.Code)
	}
	_, rank, wide, err = frontend.UnrankArtwork(art.image)
	if err != nil || rank != 10 || !wide {
		t.Fatal("wide thumb lost source membership", err)
	}
	allowed = false
	out = httptest.NewRecorder()
	fresh().Serve(out, req, tag)
	if out.Code == 200 {
		t.Fatal("warm rank bypassed current policy")
	}
}
