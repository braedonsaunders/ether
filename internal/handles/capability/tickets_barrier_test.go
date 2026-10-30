package capability

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// SQLite BEGIN IMMEDIATE blocks the replacement at transaction start; PG
// exercises the pre-lock read/reissue interleaving explicitly.
func TestTicketConsumeRechecksAfterReissue(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL transaction interleaving")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	sourceA, sourceB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, source := range []string{sourceA, sourceB} {
		if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, source); err != nil {
			t.Fatal(err)
		}
	}
	issuer, _ := NewTickets(db)
	scope := TicketScope{ViewerID: 1, Surface: "silo", DeviceID: "tv", SourceHash: sourceA, DeviceJSON: `{"ID":"tv","Client":"old"}`}
	e := TicketEntry{Digest: strings.Repeat("c", 64), Purpose: "events-ws"}
	if _, err := issuer.IssueBatch(ctx, scope, []TicketEntry{e}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{})
	resume := make(chan struct{})
	consumer, _ := NewTickets(db)
	consumer.beforeConsumeLock = func() { close(seen); <-resume }
	type answer struct {
		ticket Ticket
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		ticket, err := consumer.Consume(ctx, "silo", "events-ws", e.Digest)
		done <- answer{ticket, err}
	}()
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("initial read timed out")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace='ticket' AND digest=?`, e.Digest); err != nil {
		close(resume)
		t.Fatal(err)
	}
	scope.SourceHash = sourceB
	scope.DeviceJSON = `{"ID":"tv","Client":"new"}`
	if _, err := issuer.IssueBatch(ctx, scope, []TicketEntry{e}); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	select {
	case got := <-done:
		if got.err != nil || got.ticket.SourceHash != sourceB || got.ticket.DeviceJSON != scope.DeviceJSON {
			t.Fatalf("stale result: %+v err=%v", got.ticket, got.err)
		}
	case <-ctx.Done():
		t.Fatal("consume timed out")
	}
}
