package sessions

import (
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestBindingOwnershipAndDecisionFailure(t *testing.T) {
	table := NewBindingTable[string]()
	now := time.Now()
	if !table.Claim("encoder", 1, "phone", now) || table.Claim("encoder", 2, "phone", now) {
		t.Fatal("claim did not enforce ownership")
	}
	first, ok := table.Get("encoder")
	if !ok || first.StreamToken == "" {
		t.Fatal("claim did not create a stream grant")
	}
	table.Put(&Binding[string]{ID: "encoder", UserID: 2, ClientID: "phone", PlayID: "wrong"})
	if got, _ := table.Get("encoder"); got.UserID != 1 || got.PlayID != "" {
		t.Fatal("another viewer replaced binding")
	}
	table.Put(&Binding[string]{ID: "encoder", UserID: 1, ClientID: "phone", PlayID: "play", Started: true})
	got, _ := table.Get("encoder")
	if got.StreamToken != first.StreamToken || got.PlayID != "play" {
		t.Fatal("binding did not preserve its grant")
	}
	want := errors.New("decision failed")
	key := core.MovieKey(1)
	table.Fail("encoder", 1, "phone", key, want, now)
	if err, ok := table.TakeFailure("encoder", 2, key, now); ok || err != nil {
		t.Fatal("failure leaked across viewers")
	}
	if err, ok := table.TakeFailure("encoder", 1, key, now); !ok || !errors.Is(err, want) {
		t.Fatalf("failure %v, %v", err, ok)
	}
	if _, ok := table.TakeFailure("encoder", 1, key, now); ok {
		t.Fatal("failure was not one-shot")
	}
	if ids := table.ByPlayID("play"); len(ids) != 1 || ids[0] != "encoder" {
		t.Fatalf("play bindings: %v", ids)
	}
}
