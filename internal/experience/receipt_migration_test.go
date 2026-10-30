package experience

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptExistingStateMigratesAuthorityAndClaims(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	calls := 0
	svc, err := NewReceipts(relayOptions(db, &now, 10, true, &calls))
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7}
	device := core.Device{ID: "tv"}
	token, err := svc.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "preserved"})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := svc.codec.Hash(token)
	// Reconstruct the exact pre-cutover storage boundary without new guards.
	for _, name := range []string{"experience_receipt_admit", "experience_receipt_immutable", "experience_receipt_retire", "experience_receipt_header_retire", "experience_receipt_header_immutable"} {
		query := fmt.Sprintf("DROP TRIGGER %s", name)
		if store.DialectOf(db) == store.Postgres {
			table := "experience_receipts"
			if name == "experience_receipt_header_retire" || name == "experience_receipt_header_immutable" {
				table = "capability_headers"
			}
			query += " ON " + table
		}
		if _, err = db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if store.DialectOf(db) == store.Postgres {
		for _, name := range []string{"experience_receipt_admit_fn", "experience_receipt_immutable_fn", "experience_receipt_retire_fn", "experience_receipt_header_retire_fn"} {
			if _, err = db.ExecContext(ctx, "DROP FUNCTION "+name+"()"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace='experience.receipt'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE experience_receipt_actions(hash TEXT NOT NULL REFERENCES experience_receipts(hash) ON DELETE CASCADE,action TEXT NOT NULL,PRIMARY KEY(hash,action))`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO experience_receipt_actions(hash,action) VALUES(?,?)`, digest, string(frontend.RowImpression)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE component='experience_receipts' AND version=5`); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	report := frontend.RowInteraction{Viewer: viewer, Device: device, Protocol: "jellyfin", Handle: token, Kind: frontend.RowImpression}
	if err = svc.Report(ctx, report); err != nil || calls != 0 {
		t.Fatalf("previous claim repeated: calls=%d %v", calls, err)
	}
	report.Kind = frontend.RowClick
	if err = svc.Report(ctx, report); err != nil || calls != 1 {
		t.Fatalf("new claim unavailable: calls=%d %v", calls, err)
	}
	var subject string
	if err = db.QueryRowContext(ctx, `SELECT subject FROM experience_receipts WHERE hash=?`, digest).Scan(&subject); err != nil || subject != "preserved" {
		t.Fatalf("payload %q %v", subject, err)
	}
}
