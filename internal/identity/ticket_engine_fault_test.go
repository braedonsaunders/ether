package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

type ticketFaultViewer struct{}

func (ticketFaultViewer) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	return &core.User{ID: id}, nil
}

type ticketCommitFault struct {
	delegate       ticketEngine
	issue, consume bool
}

var errAmbiguousTicketCommit = errors.New("injected ambiguous commit")

func (f ticketCommitFault) IssueMirrored(ctx context.Context, s capability.TicketScope, e []capability.TicketEntry) (int64, error) {
	expiry, err := f.delegate.IssueMirrored(ctx, s, e)
	if err == nil && f.issue {
		return 0, errAmbiguousTicketCommit
	}
	return expiry, err
}
func (f ticketCommitFault) ConsumeMirrored(ctx context.Context, s, p, d string) (capability.Ticket, error) {
	ticket, err := f.delegate.ConsumeMirrored(ctx, s, p, d)
	if err == nil && f.consume {
		return capability.Ticket{}, errAmbiguousTicketCommit
	}
	return ticket, err
}

// Simulates a committed operation whose response was lost: the service must
// never return a usable token or consumed ticket on an ambiguous error.
func TestTicketServiceAmbiguousCommitFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := NewGrants(db, ticketFaultViewer{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	credential, err := grants.Issue(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewTicketService(db, "silo", grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	real := service.engine
	service.engine = ticketCommitFault{delegate: real, issue: true}
	token, err := service.Issue(ctx, credential, device, "events-ws")
	if !errors.Is(err, errAmbiguousTicketCommit) || token != "" {
		t.Fatalf("ambiguous issue token=%q err=%v", token, err)
	}
	service.engine = real
	token, err = service.Issue(ctx, credential, device, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	service.engine = ticketCommitFault{delegate: real, consume: true}
	result, err := service.Consume(ctx, token, "events-ws")
	if !errors.Is(err, errAmbiguousTicketCommit) || result.Viewer != nil {
		t.Fatalf("ambiguous consume viewer=%v err=%v", result.Viewer, err)
	}
	service.engine = real
	if _, err = service.Consume(ctx, token, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("committed consume replay: %v", err)
	}
}
