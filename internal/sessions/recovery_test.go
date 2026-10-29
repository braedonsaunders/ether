package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakeFailover struct {
	id, url string
	at      time.Duration
}

func (f *fakeFailover) FailoverInput(_ context.Context, id, url string, at time.Duration) error {
	f.id, f.url, f.at = id, url, at
	return nil
}
func TestRecoverUsesBoundSource(t *testing.T) {
	table := NewBindingTable[string]()
	table.Put(&Binding[string]{ID: "encoder", PlayID: "source", UserID: 1, ClientID: "phone"})
	p := &fakeFailover{}
	if err := Recover(context.Background(), table, p, "encoder", "loopback", time.Second); err != nil {
		t.Fatal(err)
	}
	if p.id != "source" || p.url != "loopback" || p.at != time.Second {
		t.Fatalf("failover %+v", p)
	}
	if err := Recover(context.Background(), table, struct{}{}, "encoder", "loopback", 0); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("unavailable: %v", err)
	}
	if err := Recover(context.Background(), table, struct{}{}, "unknown", "loopback", 0); err != nil {
		t.Fatal(err)
	}
}
