package experience

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptOldWriterAndRenewalFailClosed(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s, err := NewReceipts(relayOptions(db, &now, 10, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Issue(ctx, &core.User{ID: 7}, core.Device{ID: "tv"}, "jellyfin", ReceiptSubject{Kind: "title", ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank) VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',7,'tv','jellyfin',9999999999,'title','old','',0)`); err == nil {
		t.Fatal("old writer bypassed mandatory admission")
	}
	if _, err = db.ExecContext(ctx, `UPDATE experience_receipts SET expires=expires+60`); err == nil {
		t.Fatal("old writer renewed typed receipt")
	}
	if _, err = db.ExecContext(ctx, `UPDATE capability_headers SET expires=expires+60000 WHERE namespace='experience.receipt'`); err == nil {
		t.Fatal("old writer renewed header")
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d %v", n, err)
	}
}
