package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

type imageBytes struct{}

func (imageBytes) Get(context.Context, string, int) ([]byte, string, error) {
	return []byte("image"), "image/jpeg", nil
}

type imageAdapterMembers struct{ db *sql.DB }

func (m imageAdapterMembers) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	var active int
	err := m.db.QueryRowContext(ctx, `SELECT active FROM auth_users WHERE id=?`, id).Scan(&active)
	if err != nil || active != 1 {
		return nil, core.ErrUnauthorized
	}
	return &core.User{ID: id}, nil
}

func imageAdapterService(t *testing.T, db *sql.DB, surface frontend.Protocol, now func() time.Time) (*ImageGrantService, string) {
	t.Helper()
	ctx := context.Background()
	if now == nil {
		now = time.Now
	}
	grants, err := NewGrants(db, imageAdapterMembers{db}, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	credential, err := grants.Issue(ctx, 1, nil, string(surface), device)
	if err != nil {
		t.Fatal(err)
	}
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{image}}, nil
	}}
	service, err := NewImageGrantService(db, surface, grants, func() frontend.Experience { return experience }, imageBytes{}, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	return service, credential
}

func TestImageGrantMirrorCanonicalScopeAndRevoke(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100000, 0)
	clock := func() time.Time { return now }
	service, credential := imageAdapterService(t, db, "jellyfin", clock)
	other, _ := imageAdapterService(t, db, "emby", clock)
	device := core.Device{ID: "tv"}
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	secret, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: device, Item: core.MovieKey(7), Image: image})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(secret))
	want := hex.EncodeToString(sum[:])
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != secret {
		t.Fatal("canonical image secret diverged")
	}
	var storedHash, storedDevice, namespace string
	var legacyExpires, headerExpires, shard, item int64
	canonical, err := json.Marshal(device)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT hash,device,expires,item FROM frontend_image_grants`).Scan(&storedHash, &storedDevice, &legacyExpires, &item); err != nil {
		t.Fatal(err)
	}
	if storedHash != want || storedDevice != string(canonical) || item != int64(core.MovieKey(7)) || legacyExpires != now.Add(time.Minute).UnixMilli() {
		t.Fatal("canonical image grant row diverged")
	}
	if err = db.QueryRowContext(ctx, `SELECT namespace,expires,shard FROM capability_headers WHERE digest=?`, want).Scan(&namespace, &headerExpires, &shard); err != nil {
		t.Fatal(err)
	}
	if namespace != imageCapabilityNamespace || headerExpires != legacyExpires || shard != 1%capability.HeaderShardCount {
		t.Fatal("image header mirror diverged")
	}
	var ticketHeaders int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket'`).Scan(&ticketHeaders); err != nil || ticketHeaders != 0 {
		t.Fatal("image mirror used the ticket namespace", ticketHeaders, err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 200 {
		t.Fatal("legacy authority rejected a live grant", w.Code)
	}
	if err = other.Revoke(ctx, credential, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface revoke", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE digest=?`, want).Scan(&ticketHeaders); err != nil || ticketHeaders != 1 {
		t.Fatal("foreign revoke dropped the header", ticketHeaders, err)
	}
	if err = service.Revoke(ctx, credential, secret); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants WHERE hash=?`, want).Scan(&revoked); err != nil || revoked != 0 {
		t.Fatal("legacy revoke diverged", revoked, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE digest=?`, want).Scan(&ticketHeaders); err != nil || ticketHeaders != 0 {
		t.Fatal("header remained after revoke", ticketHeaders, err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 {
		t.Fatal("revoked legacy row still served", w.Code)
	}
}

func TestImageGrantConcurrentAdmission(t *testing.T) {
	ctx := context.Background()
	var db *sql.DB
	var err error
	if os.Getenv(store.TestPostgresEnv) != "" {
		db = store.OpenTest(t)
	} else {
		db, err = store.OpenPath(ctx, filepath.Join(t.TempDir(), "image-grants.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	if _, err = db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	service.admission.perScope = 4
	service.admission.perShard = 4
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(1), Image: image}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = service.Issue(ctx, request)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		var capacity *ImageCapacityError
		if !errors.As(err, &capacity) || capacity.Scope != "viewer" {
			t.Fatal(err)
		}
	}
	if ok != 4 {
		t.Fatal("concurrent admissions", ok)
	}
	var headers int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&headers); err != nil || headers != 4 {
		t.Fatal("header admissions", headers, err)
	}
}

func TestImageGrantBatchBindsPastHeaderStatementLimit(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	items := make([]core.RatingKey, 41)
	for i := range items {
		items[i] = core.MovieKey(int64(i + 1))
	}
	batched, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Device: core.Device{ID: "batch"}, Items: items})
	if err != nil || len(batched) != len(items) {
		t.Fatal("batch across header statement limit", len(batched), err)
	}
	var headers int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&headers); err != nil || headers != len(items) {
		t.Fatal("batched headers", headers, err)
	}
}

// Issuing a page of images prunes this viewer's expired rows once, not in
// every issue, and prunes again when admission runs out of room.
func TestImageIssuePrunesOncePerWindow(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	prunes := 0
	service.beforeImageCleanup = func(*sql.Tx) { prunes++ }
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	for range 5 {
		if _, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(1), Image: image}); err != nil {
			t.Fatal(err)
		}
	}
	if prunes != 1 {
		t.Fatalf("pruned %d times for five issues, want once", prunes)
	}
	// Full admission prunes before giving up.
	service.admission.perScope = 5
	if _, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(1), Image: image}); err == nil {
		t.Fatal("a full scope admitted a sixth image")
	}
	if prunes != 2 {
		t.Fatalf("a full scope did not prune before refusing: %d", prunes)
	}
}
