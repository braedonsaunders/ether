package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
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

func TestSignedPageGrantWritesNothingAndKeepsRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	calls := 0
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return &pageArtwork{allow: &allowed, calls: &calls} }, &imageFixture{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.SetImageKey([]byte("test-image-page-key"))
	service.SetImageGeneration(func() uint64 { return 1 })
	title := core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Bench"}
	page, err := service.SignPage(ctx, frontend.ImagePageRequest{Credential: credential, Device: core.Device{ID: "tv"}, Titles: []core.Title{title}, Kinds: []string{frontend.ImagePoster}})
	if err != nil || len(page) != 1 || len(page[0].Images) != 1 || !shortImageTag(page[0].Images[0].Secret) {
		t.Fatalf("page %+v err=%v", page, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&count); err != nil || count != 0 || calls != 1 {
		t.Fatalf("writes=%d calls=%d err=%v", count, calls, err)
	}
	again, err := service.SignPage(ctx, frontend.ImagePageRequest{Credential: credential, Device: core.Device{ID: "tv"}, Titles: []core.Title{title}})
	if err != nil || calls != 1 || len(again) != 1 {
		t.Fatalf("cache calls=%d err=%v", calls, err)
	}
	secret := page[0].Images[0].Secret
	rec := httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/image", nil), secret)
	if rec.Code != 200 || rec.Body.String() != "image" {
		t.Fatalf("serve %d %s", rec.Code, rec.Body.String())
	}
	allowed = false
	rec = httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/image", nil), secret)
	if rec.Code != 404 {
		t.Fatalf("policy flip %d", rec.Code)
	}
	allowed = true
	if err := service.Revoke(ctx, credential, secret); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/image", nil), secret)
	if rec.Code != 404 {
		t.Fatalf("revoke %d", rec.Code)
	}
	fresh, err := service.SignPage(ctx, frontend.ImagePageRequest{Credential: credential, Titles: []core.Title{title}})
	if err != nil || len(fresh) != 1 || len(fresh[0].Images) != 1 {
		t.Fatalf("fresh %+v %v", fresh, err)
	}
	if err := grants.RevokeViewer(ctx, 1); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/image", nil), fresh[0].Images[0].Secret)
	if rec.Code != 404 {
		t.Fatalf("credential revoke %d", rec.Code)
	}
}

type pageArtwork struct {
	frontendtest.Experience
	allow  *bool
	calls  *int
	images []frontend.Image
}

func (p *pageArtwork) ItemArtwork(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]frontend.Image, bool, error) {
	*p.calls++
	if p.allow == nil || !*p.allow {
		return nil, false, core.ErrNotFound
	}
	if len(p.images) > 0 {
		return p.images, true, nil
	}
	return []frontend.Image{{Kind: frontend.ImagePoster, Path: "primary"}}, true, nil
}

type pathImageFixture struct{ path string }

func (p *pathImageFixture) Get(_ context.Context, path string, _ int) ([]byte, string, error) {
	p.path = path
	return []byte(path), "image/jpeg", nil
}

func TestSignedPageGrantSealsOncePerTitle(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	calls := 0
	art := &pageArtwork{allow: &allowed, calls: &calls, images: []frontend.Image{
		{Kind: frontend.ImagePoster, Path: "primary"},
		{Kind: frontend.ImageBackdrop, Path: "back"},
	}}
	source := &pathImageFixture{}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return art }, source, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.SetImageKey([]byte("test-image-page-key"))
	title := core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Bench"}
	page, err := service.SignPage(ctx, frontend.ImagePageRequest{Credential: credential, Device: core.Device{ID: "tv"}, Titles: []core.Title{title}, Kinds: []string{frontend.ImagePoster, frontend.ImageBackdrop}})
	if err != nil || len(page) != 1 || len(page[0].Images) != 2 || !shortImageTag(page[0].Images[0].Secret) || page[0].Images[0].Secret == page[0].Images[1].Secret {
		t.Fatalf("page %+v err=%v", page, err)
	}
	rec := httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/Items/9/Images/Primary", nil), page[0].Images[0].Secret)
	if rec.Code != 200 || source.path != "primary" || rec.Body.String() != "primary" {
		t.Fatalf("primary %d %s path=%s", rec.Code, rec.Body.String(), source.path)
	}
	rec = httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/Items/9/Images/Backdrop", nil), page[0].Images[1].Secret)
	if rec.Code != 200 || source.path != "back" || rec.Body.String() != "back" {
		t.Fatalf("backdrop %d %s path=%s", rec.Code, rec.Body.String(), source.path)
	}
	rec = httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/Items/9/Images/Backdrop", nil), page[0].Images[0].Secret)
	if rec.Code != 404 {
		t.Fatalf("poster tag served a backdrop %d", rec.Code)
	}
}

func shortImageTag(secret string) bool {
	if len(secret) != 32 {
		return false
	}
	_, err := hex.DecodeString(secret)
	return err == nil
}

func TestPageGrantTagStaysStableAndImageRevalidates(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	grants, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	calls := 0
	images := &imageFixture{}
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience {
		return &pageArtwork{allow: &allowed, calls: &calls}
	}, images, 10*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	service.SetImageKey([]byte("test-image-page-key"))
	title := core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Bench"}
	req := frontend.ImagePageRequest{Credential: credential, Device: core.Device{ID: "tv"}, Titles: []core.Title{title}, Kinds: []string{frontend.ImagePoster}}
	first, err := service.SignPage(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SignPage(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Images[0].Secret == "" || first[0].Images[0].Secret != second[0].Images[0].Secret {
		t.Fatalf("tag changed %q %q", first[0].Images[0].Secret, second[0].Images[0].Secret)
	}
	secret := first[0].Images[0].Secret
	rec := httptest.NewRecorder()
	service.Serve(rec, httptest.NewRequest("GET", "/Items/9/Images/Primary", nil), secret)
	etag := rec.Header().Get("ETag")
	if rec.Code != 200 || etag == "" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=86400") || images.calls != 1 {
		t.Fatalf("serve %d etag=%q cache=%q calls=%d", rec.Code, etag, rec.Header().Get("Cache-Control"), images.calls)
	}
	revalidating := httptest.NewRequest("GET", "/Items/9/Images/Primary", nil)
	revalidating.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	service.Serve(rec, revalidating, secret)
	if rec.Code != 304 || images.calls != 1 {
		t.Fatalf("revalidate %d calls=%d", rec.Code, images.calls)
	}
	if err := service.Revoke(ctx, credential, secret); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.SignPage(ctx, req)
	if err != nil || fresh[0].Images[0].Secret == secret {
		t.Fatalf("revoke did not rotate the tag: %v", fresh)
	}
}

type pageArtworkBatch struct{ *pageArtwork }

func (p pageArtworkBatch) ItemArtworkPage(ctx context.Context, v *core.Viewer, d core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Image, error) {
	out := map[core.RatingKey][]frontend.Image{}
	for _, k := range keys {
		images, _, err := p.ItemArtwork(ctx, v, d, k)
		if err == nil {
			out[k] = images
		}
	}
	return out, nil
}

// A page listed in one batch reads its revoked tags in one query and still
// rotates a revoked tag.
func TestBatchListedPageRotatesARevokedTag(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	grants, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, calls := true, 0
	service, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience {
		return pageArtworkBatch{&pageArtwork{allow: &allowed, calls: &calls}}
	}, &imageFixture{}, 10*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	service.SetImageKey([]byte("test-image-page-key"))
	titles := []core.Title{{Key: core.MovieKey(9), Type: core.TypeMovie}, {Key: core.MovieKey(10), Type: core.TypeMovie}}
	req := frontend.ImagePageRequest{Credential: credential, Device: core.Device{ID: "tv"}, Titles: titles, Kinds: []string{frontend.ImagePoster}}
	first, err := service.SignPage(ctx, req)
	if err != nil || len(first) != 2 {
		t.Fatalf("sign %v %v", first, err)
	}
	secret := first[0].Images[0].Secret
	if err := service.Revoke(ctx, credential, secret); err != nil {
		t.Fatal(err)
	}
	// Another node knows the revocation only from the database.
	other, err := identity.NewImageGrantService(db, "jellyfin", grants, func() frontend.Experience {
		return pageArtworkBatch{&pageArtwork{allow: &allowed, calls: &calls}}
	}, &imageFixture{}, 10*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	other.SetImageKey([]byte("test-image-page-key"))
	for _, s := range []*identity.ImageGrantService{service, other} {
		fresh, err := s.SignPage(ctx, req)
		if err != nil || fresh[0].Images[0].Secret == secret || fresh[1].Images[0].Secret != first[1].Images[0].Secret {
			t.Fatalf("revoked tag not rotated, or a live one changed: %v", fresh)
		}
	}
}
