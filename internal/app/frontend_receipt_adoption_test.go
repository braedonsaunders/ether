package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// This exercises the production constructor and real SQLite transaction. A
// successful baseline must mint a handle before the failure cases can pass.
func TestProductionReceiptMirrorFailureRollsBack(t *testing.T) {
	for _, mode := range []string{"bind", "cleanup"} {
		t.Run(mode, func(t *testing.T) {
			a := setupTestApp(t, true)
			ctx := context.Background()
			if err := a.users.SetOwner(ctx, plextv.Account{ID: 321, Username: "owner"}, "token"); err != nil {
				t.Fatal(err)
			}
			viewer, err := a.users.Owner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			title := core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{TMDB: 42}, Title: "Visible", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
			if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
				t.Fatal(err)
			}
			if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
				t.Fatal(err)
			}
			if err = a.state.Progress(ctx, viewer.ID, title.Key, 10*time.Minute, 90*time.Minute, "paused"); err != nil {
				t.Fatal(err)
			}
			a.availability = nil
			exp, err := a.newSurfaceExperience("jellyfin", nil)
			if err != nil {
				t.Fatal(err)
			}
			req := frontend.RowRequest{Viewer: viewer, Device: core.Device{ID: "tv"}, Placement: frontend.PlacementHome}
			rows, err := exp.Rows(ctx, req)
			if err != nil {
				t.Fatalf("baseline rows: %v", err)
			}
			hasHandle := false
			for _, row := range rows {
				if row.Handle != "" {
					hasHandle = true
				}
			}
			if !hasHandle {
				t.Fatal("baseline did not reach receipt issue")
			}
			if _, err = a.db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace='experience.receipt'`); err != nil {
				t.Fatal(err)
			}
			if _, err = a.db.ExecContext(ctx, `DELETE FROM experience_receipts`); err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("a", 64)
			if mode == "cleanup" {
				if _, err = a.db.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank) VALUES(?,?,?,?,?,?,?,?,?)`, digest, viewer.ID, "tv", "jellyfin", 1, "row", "old", "continue", 0); err != nil {
					t.Fatal(err)
				}
				if _, err = a.db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES(?,?,?,?,?,?,?)`, "experience.receipt", digest, viewer.ID%64, viewer.ID, "jellyfin", "tv", 1000); err != nil {
					t.Fatal(err)
				}
			}
			trigger := `CREATE TRIGGER receipt_bind_fail BEFORE INSERT ON capability_headers WHEN NEW.namespace='experience.receipt' BEGIN SELECT RAISE(ABORT,'receipt bind fault'); END`
			if mode == "cleanup" {
				trigger = `CREATE TRIGGER receipt_drop_fail BEFORE DELETE ON capability_headers WHEN OLD.namespace='experience.receipt' AND OLD.device_id='tv' BEGIN SELECT RAISE(ABORT,'receipt drop fault'); END`
			}
			if _, err = a.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			rows, err = exp.Rows(ctx, req)
			if err == nil {
				t.Fatalf("%s fault returned rows: %+v", mode, rows)
			}
			for _, row := range rows {
				if row.Handle != "" {
					t.Fatal("returned handle after fault")
				}
			}
			expected := 0
			if mode == "cleanup" {
				expected = 1
			}
			var receipts, headers int
			if err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='experience.receipt'`).Scan(&headers); err != nil {
				t.Fatal(err)
			}
			if receipts != expected || headers != expected {
				t.Fatalf("rollback receipts=%d headers=%d expected=%d", receipts, headers, expected)
			}
		})
	}
}
