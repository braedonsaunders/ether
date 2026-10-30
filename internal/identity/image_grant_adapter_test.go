package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
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
	if err = db.QueryRowContext(ctx, `SELECT revoked FROM frontend_image_grants WHERE hash=?`, want).Scan(&revoked); err != nil || revoked != 1 {
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

func TestImageGrantEmptyDeviceAndHeaderlessLegacyStayAuthoritative(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	service.images = imageBytes{}
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	if _, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{}, Item: core.MovieKey(1), Image: image}); err != nil {
		t.Fatal(err)
	}
	var headers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&headers); err != nil || headers != 0 {
		t.Fatal("empty device was bound", headers, err)
	}
	raw := bytes.Repeat([]byte{9}, 32)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	hash, ok := validImageSecret(secret)
	if !ok {
		t.Fatal("baseline secret rejected")
	}
	sourceHash := sha256.Sum256([]byte(credential))
	deviceJSON, _ := json.Marshal(core.Device{ID: "tv"})
	expires := time.Now().Add(time.Hour).UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, hash, hex.EncodeToString(sourceHash[:]), 1, "jellyfin", 3, string(deviceJSON), image.Kind, image.Path, image.Tag, expires, "", image.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES(?,?,?,?,?,?,?)`, imageCapabilityNamespace, hash, 1, 1, "jellyfin", "other-device", expires); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 200 {
		t.Fatal("historical legacy row was not authoritative", w.Code)
	}
	if _, err := db.ExecContext(ctx, `UPDATE frontend_image_grants SET revoked=1 WHERE hash=?`, hash); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 {
		t.Fatal("header authorized a revoked legacy row", w.Code)
	}
}

func TestImageGrantQuotaIgnoresLegacyOnlyRows(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	service.admission.perScope = 1
	service.admission.perShard = 2
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	deviceJSON, _ := json.Marshal(core.Device{ID: "tv"})
	source := sha256.Sum256([]byte(credential))
	for i := 0; i < 5; i++ {
		raw := bytes.Repeat([]byte{byte(i + 1)}, 32)
		secret := base64.RawURLEncoding.EncodeToString(raw)
		hash, ok := validImageSecret(secret)
		if !ok {
			t.Fatal("baseline secret rejected")
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, hash, hex.EncodeToString(source[:]), 1, "jellyfin", i+1, string(deviceJSON), image.Kind, image.Path, image.Tag, time.Now().Add(time.Hour).UnixMilli(), "", image.Subject); err != nil {
			t.Fatal(err)
		}
	}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(9), Image: image}
	if _, err := service.Issue(ctx, request); err != nil {
		t.Fatal("legacy-only rows consumed admission", err)
	}
	var capacity *ImageCapacityError
	if _, err := service.Issue(ctx, request); !errors.As(err, &capacity) || capacity.Scope != "viewer" {
		t.Fatal("viewer admission", err)
	}
	var headers, legacy int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&headers); err != nil || headers != 1 {
		t.Fatal("header count", headers, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants WHERE revoked=0`).Scan(&legacy); err != nil || legacy != 6 {
		t.Fatal("legacy count", legacy, err)
	}
}

func TestImageGrantCleanupRechecksExpiryAndMaintenance(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100000, 0)
	clock := func() time.Time { return now }
	service, credential := imageAdapterService(t, db, "jellyfin", clock)
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(4), Image: image}
	if _, err := service.Issue(ctx, request); err != nil {
		t.Fatal(err)
	}
	var kept string
	if err := db.QueryRowContext(ctx, `SELECT hash FROM frontend_image_grants`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	service.beforeImageCleanup = func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=? WHERE hash=?`, now.Add(time.Hour).UnixMilli(), kept); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capability_headers SET expires=? WHERE namespace=? AND digest=?`, now.Add(time.Hour).UnixMilli(), imageCapabilityNamespace, kept); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Issue(ctx, request); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants WHERE hash=?`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatal("renewed legacy row was cleaned", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE digest=?`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatal("renewed header was cleaned", n, err)
	}
	ticketDigest := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	if _, err := db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES('ticket',?,?,1,'jellyfin','tv',1)`, ticketDigest, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE capability_headers SET expires=1 WHERE namespace=?`, imageCapabilityNamespace); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneImageGrants(ctx, db, now)
	if err != nil || removed < 2 {
		t.Fatal("maintenance", removed, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, ticketDigest).Scan(&n); err != nil || n != 1 {
		t.Fatal("maintenance touched a ticket header", n, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&n); err != nil || n != 0 {
		t.Fatal("expired image headers remained", n, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&n); err != nil || n != 0 {
		t.Fatal("expired legacy rows remained", n, err)
	}
}

func TestImageGrantConcurrentAdmission(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "image-grants.db"))
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

// Default issue must not reject a viewer because stale headers belonging to
// other viewers filled a shard before cross-scope maintenance is available.
func TestImageGrantDefaultAdmissionDisabled(t *testing.T) {
	a := defaultImageAdmission()
	if a.perScope != 0 || a.perShard != 0 {
		t.Fatalf("unvalidated admission enabled: %+v", a)
	}
	if a.legacyCleanup <= 0 || a.headerCleanup <= 0 {
		t.Fatalf("cleanup disabled: %+v", a)
	}
}

func TestImageGrantDefaultIssuesAboveFormerViewerCap(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: 1, Surface: "jellyfin", DeviceID: "tv"}
	for start := 0; start < imagePerScope; start += capability.HeaderBatchMax {
		end := min(start+capability.HeaderBatchMax, imagePerScope)
		batch := make([]capability.Header, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, capability.Header{HeaderScope: scope, Digest: fmt.Sprintf("%064x", i+1), ExpiresMillis: time.Now().Add(time.Hour).UnixMilli()})
		}
		if err = capability.BindHeadersInTx(ctx, tx, batch); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(9), Image: image}
	if _, err = service.Issue(ctx, request); err != nil {
		t.Fatalf("default issuance at former cap: %v", err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND viewer_id=? AND surface=?`, imageCapabilityNamespace, 1, "jellyfin").Scan(&count); err != nil || count != imagePerScope+1 {
		t.Fatalf("headers=%d err=%v", count, err)
	}
}

func TestImageGrantShardMaintenanceIsolation(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0),(2,124,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	source := sha256.Sum256([]byte(credential))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, row := range []struct {
		viewer  int64
		digest  string
		expires int64
	}{{1, fmt.Sprintf("%064x", 1), now.Add(-time.Minute).UnixMilli()}, {2, fmt.Sprintf("%064x", 2), now.Add(-time.Minute).UnixMilli()}, {1, fmt.Sprintf("%064x", 3), now.Add(time.Minute).UnixMilli()}} {
		_, err = tx.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.digest, hex.EncodeToString(source[:]), row.viewer, "jellyfin", 1, `{"ID":"tv"}`, "poster", "opaque", "v1", row.expires, "", "")
		if err != nil {
			t.Fatal(err)
		}
		h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: row.viewer, Surface: "jellyfin", DeviceID: "tv"}, Digest: row.digest, ExpiresMillis: row.expires}
		if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{h}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	n, err := PruneImageGrantShard(ctx, db, now, 1)
	if err != nil || n != 2 {
		t.Fatalf("removed=%d err=%v", n, err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("legacy=%d err=%v", count, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, imageCapabilityNamespace).Scan(&count); err != nil || count != 2 {
		t.Fatalf("headers=%d err=%v", count, err)
	}
	_ = service
}
