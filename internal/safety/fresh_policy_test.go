package safety

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestFreshPolicyDoesNotTrustLaggingCache(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	writer, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Put(ctx, 1, Policy{RemoteDisabled: true, AllowedHours: []int{9}}); err != nil {
		t.Fatal(err)
	}
	if reader.Get(1).RemoteDisabled {
		t.Fatal("fixture cache unexpectedly refreshed")
	}
	fresh, err := reader.FreshPolicy(ctx, 1)
	if err != nil || !fresh.RemoteDisabled || len(fresh.AllowedHours) != 1 || fresh.AllowedHours[0] != 9 {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	fresh.AllowedHours[0] = 12
	again, err := reader.FreshPolicy(ctx, 1)
	if err != nil || again.AllowedHours[0] != 9 {
		t.Fatalf("fresh alias=%+v err=%v", again, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE safety_policies SET policy='not-json' WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.FreshPolicy(ctx, 1); err == nil {
		t.Fatal("malformed durable policy accepted")
	}
}
