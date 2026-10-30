package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestImageAuthorityEmptyDeviceAndImmutableExpiry(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	service, credential := imageAdapterService(t, db, "jellyfin", func() time.Time { return now })
	token, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Item: core.MovieKey(1), Image: frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	var device string
	if err = db.QueryRow(`SELECT device_id FROM capability_headers WHERE namespace='image' AND digest=?`, digest).Scan(&device); err != nil || device != "" {
		t.Fatal(device, err)
	}
	for _, query := range []string{`UPDATE frontend_image_grants SET expires=expires+60000 WHERE hash=?`, `UPDATE capability_headers SET expires=expires+60000 WHERE digest=?`, `INSERT INTO frontend_image_grants SELECT 'unadmitted',source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,revoked,channel,image_subject FROM frontend_image_grants WHERE hash=?`} {
		if _, err = db.Exec(query, digest); err == nil {
			t.Fatalf("writer bypass: %s", query)
		}
	}
	wrongShard := fmt.Sprintf("%064x", 77)
	if _, err = db.Exec(`INSERT INTO capability_headers SELECT namespace,?,-1,viewer_id,surface,device_id,expires FROM capability_headers WHERE namespace='image' AND digest=?`, wrongShard, digest); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO frontend_image_grants SELECT ?,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,revoked,channel,image_subject FROM frontend_image_grants WHERE hash=?`, wrongShard, digest); err == nil {
		t.Fatal("wrong-shard reservation accepted")
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), token)
	if w.Code != 200 || w.Body.String() != "image" {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = db.Exec(`DELETE FROM capability_headers WHERE namespace='image' AND digest=?`, digest); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), token)
	if w.Code != 404 {
		t.Fatal("headerless fallback", w.Code)
	}
}

// Reconstruct the pre-cutover tables, with actual issued payloads, then migrate.
// Every field and token remains usable; no second authority survives.
func TestCapabilityExistingStateCutover(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	token, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Item: core.MovieKey(1), Image: frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	// DDL below is the old frontend ticket migration, not an emulated new issuer.
	for _, name := range []string{"image_admitted_insert", "image_immutable", "image_header_immutable", "image_header_delete", "ticket_admitted_insert", "ticket_immutable", "ticket_header_immutable"} {
		table := "frontend_image_grants"
		if name == "ticket_admitted_insert" || name == "ticket_immutable" {
			table = "capability_tickets"
		}
		if name == "image_header_immutable" || name == "ticket_header_immutable" || name == "image_header_delete" {
			table = "capability_headers"
		}
		query := "DROP TRIGGER " + name
		if store.DialectOf(db) == store.Postgres {
			query += " ON " + table
		}
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
		if store.DialectOf(db) == store.Postgres {
			if _, err = db.Exec("DROP FUNCTION " + name + "_fn()"); err != nil {
				t.Fatal(err)
			}
		}
	}
	statements := []string{`ALTER TABLE capability_tickets ADD COLUMN legacy_mirror INTEGER NOT NULL DEFAULT 0`, `CREATE TABLE frontend_tickets(hash TEXT PRIMARY KEY,surface TEXT NOT NULL,purpose TEXT NOT NULL,viewer_id BIGINT NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE,device TEXT NOT NULL,expires BIGINT NOT NULL)`, `DELETE FROM capability_headers WHERE namespace IN ('image','ticket')`, `DELETE FROM schema_migrations WHERE (component='frontend_image_grants' AND version=7) OR (component='frontend_tickets' AND version=3)`}
	for _, query := range statements {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	ticketToken := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	sum := sha256.Sum256([]byte(ticketToken))
	digest := hex.EncodeToString(sum[:])
	source, _ := grantHash(credential)
	if _, err = db.Exec(`INSERT INTO frontend_tickets VALUES(?,'jellyfin','events-ws',1,?,'{"ID":"tv"}',?)`, digest, source, time.Now().Add(30*time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// Old consumers could remove a frontend ticket and leave its mirror charged.
	// Such a stale mirror must not become a newly usable ticket at cutover.
	stale := fmt.Sprintf("%064x", 99)
	if _, err = db.Exec(`INSERT INTO capability_headers VALUES('ticket',?,1,1,'jellyfin','tv',?)`, stale, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO capability_tickets(namespace,digest,purpose,source_hash,device,legacy_mirror) VALUES('ticket',?,'events-ws',?,'{"ID":"tv"}',1)`, stale, source); err != nil {
		t.Fatal(err)
	}
	// A malformed historical payload must fail transactionally, never partially cut over.
	if _, err = db.Exec(`INSERT INTO frontend_image_grants SELECT 'bad',source_hash,viewer_id,surface,item,'{',image_kind,image_path,image_tag,expires,revoked,channel,image_subject FROM frontend_image_grants LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err == nil {
		t.Fatal("malformed historical payload migrated")
	}
	var preserved int
	if err = db.QueryRow(`SELECT COUNT(*) FROM frontend_tickets WHERE hash=?`, digest).Scan(&preserved); err != nil || preserved != 1 {
		t.Fatal("failed migration lost ticket", preserved, err)
	}
	if _, err = db.Exec(`DELETE FROM frontend_image_grants WHERE hash='bad'`); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var staleCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, stale).Scan(&staleCount); err != nil || staleCount != 0 {
		t.Fatal("consumed mirror revived", staleCount, err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), token)
	if w.Code != 200 || w.Body.String() != "image" {
		t.Fatal(w.Code, w.Body.String())
	}
	tickets, err := NewTicketService(db, "jellyfin", service.grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tickets.Consume(ctx, ticketToken, "events-ws")
	if err != nil || got.Device.ID != "tv" {
		t.Fatal(fmt.Sprint(got), err)
	}
	if _, err = db.Exec(`INSERT INTO frontend_tickets VALUES('old','jellyfin','events-ws',1,?,'{}',1)`, source); err == nil {
		t.Fatal("old writer survived")
	}
}

func TestImageBatchAdmissionRollback(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", nil)
	service.admission.perScope = 40
	service.admission.perShard = 40
	keys := make([]core.RatingKey, 41)
	for i := range keys {
		keys[i] = core.MovieKey(int64(i + 1))
	}
	if _, err := service.IssueBatch(context.Background(), frontend.ImageBatchRequest{Credential: credential, Items: keys}); err == nil {
		t.Fatal("over-capacity batch accepted")
	}
	for _, table := range []string{"frontend_image_grants", "capability_headers"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestImageShardCleanupBoundedAndLive(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	service, credential := imageAdapterService(t, db, "jellyfin", func() time.Time { return now })
	req := frontend.ImageGrantRequest{Credential: credential, Item: core.MovieKey(1), Image: frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-poster", Tag: "v1", Subject: "tmdb:1"}}
	for i := 0; i < 45; i++ {
		if _, err := service.Issue(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(30 * time.Second)
	token, err := service.Issue(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	for _, want := range []int{6, 1} {
		if _, err = PruneImageGrantShard(ctx, db, now, 1); err != nil {
			t.Fatal(err)
		}
		var n int
		if err = db.QueryRow(`SELECT COUNT(*) FROM frontend_image_grants`).Scan(&n); err != nil || n != want {
			t.Fatal(n, want, err)
		}
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), token)
	if w.Code != 200 {
		t.Fatal("live image evicted", w.Code)
	}
}
