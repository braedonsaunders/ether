package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type lightweightArtworkExperience struct {
	frontendtest.Experience
	calls int
}

func (e *lightweightArtworkExperience) ItemArtwork(_ context.Context, _ *core.Viewer, _ core.Device, _ core.RatingKey) ([]frontend.Image, bool, error) {
	e.calls++
	return []frontend.Image{{Kind: frontend.ImagePoster, Path: "primary"}}, true, nil
}

func TestImageGrantBatchUsesLightweightArtworkPolicy(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
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
	experience := &lightweightArtworkExperience{Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		t.Fatal("batch ran full playback enrichment")
		return frontend.ItemDetail{}, nil
	}}}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return experience }, &imageFixture{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Items: []core.RatingKey{core.MovieKey(1), core.MovieKey(1)}})
	if err != nil || len(result) != 1 || len(result[0].Images) != 1 || experience.calls != 1 {
		t.Fatalf("batch: %+v calls=%d err=%v", result, experience.calls, err)
	}
}

func TestImageGrantBatchPreservesPolicyAndOrdering(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", device)
	if err != nil {
		t.Fatal(err)
	}
	first, hidden, empty, unavailable := core.MovieKey(1), core.MovieKey(2), core.MovieKey(3), core.MovieKey(4)
	calls := map[core.RatingKey]int{}
	allowed := true
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		calls[req.Key]++
		switch req.Key {
		case first:
			if !allowed {
				return frontend.ItemDetail{}, core.ErrNotFound
			}
			return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{{Kind: frontend.ImageBackdrop, Path: "back"}, {Kind: frontend.ImagePoster, Path: "primary"}, {Kind: frontend.ImagePoster, Path: "alternate"}, {Kind: frontend.ImagePerson, Path: "person", Subject: "tmdb:1"}}}, nil
		case hidden:
			return frontend.ItemDetail{}, core.ErrNotFound
		case empty:
			return frontend.ItemDetail{ImagesAuthoritative: true}, nil
		case unavailable:
			return frontend.ItemDetail{ImagesAuthoritative: false}, nil
		default:
			return frontend.ItemDetail{}, core.ErrNotFound
		}
	}}
	source := &imageFixture{}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return experience }, source, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Device: device, Items: []core.RatingKey{first, hidden, first, empty, unavailable}, Kinds: []string{frontend.ImagePoster, frontend.ImageBackdrop}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 || batch[0].Item != first || batch[1].Item != empty || batch[2].Item != unavailable || !batch[0].Authoritative || !batch[1].Authoritative || batch[2].Authoritative {
		t.Fatalf("batch state: %+v", batch)
	}
	if calls[first] != 1 || len(batch[0].Images) != 2 || batch[0].Images[0].Image.Path != "back" || batch[0].Images[1].Image.Path != "primary" || batch[0].Images[0].Secret == batch[0].Images[1].Secret || len(batch[1].Images) != 0 || len(batch[2].Images) != 0 {
		t.Fatalf("batch order: %+v calls=%v", batch, calls)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&count); err != nil || count != 2 {
		t.Fatal("persisted grants", count, err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), batch[0].Images[1].Secret)
	if w.Code != 200 {
		t.Fatal("batch secret not usable", w.Code)
	}
	allowed = false
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), batch[0].Images[1].Secret)
	if w.Code != 404 {
		t.Fatal("batch secret ignored policy change", w.Code)
	}
	tooMany := make([]core.RatingKey, 51)
	for i := range tooMany {
		tooMany[i] = core.MovieKey(int64(i + 1))
	}
	if _, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Items: tooMany}); !errors.Is(err, frontend.ErrImageBatchLimit) {
		t.Fatal("batch limit", err)
	}
	if _, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Items: []core.RatingKey{first}, Kinds: []string{frontend.ImagePerson}}); !errors.Is(err, frontend.ErrImageBatchLimit) {
		t.Fatal("person batch allowed", err)
	}
}

func TestImageGrantBatchRejectsRevokedSourceBeforeInsert(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
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
	sum := sha256.Sum256([]byte(credential))
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		if err := grants.RevokeGrant(ctx, 1, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
		return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{{Kind: frontend.ImagePoster, Path: "ref"}}}, nil
	}}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return experience }, &imageFixture{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Items: []core.RatingKey{core.MovieKey(1)}}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("revoked source", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial grant", count, err)
	}
}

func TestImageGrantBatchInsertsFullPage(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
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
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{{Kind: frontend.ImagePoster, Path: fmt.Sprint(req.Key)}}}, nil
	}}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return experience }, &imageFixture{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]core.RatingKey, 50)
	for i := range items {
		items[i] = core.MovieKey(int64(i + 1))
	}
	result, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Items: items})
	if err != nil || len(result) != len(items) {
		t.Fatalf("batch size %d: %v", len(result), err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&count); err != nil || count != len(items) {
		t.Fatalf("stored %d: %v", count, err)
	}
}
