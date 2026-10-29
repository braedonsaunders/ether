package compactids_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend/compactids"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestStableScopedConcurrentAllocation(t *testing.T) {
	db := store.OpenTest(t)
	a, err := compactids.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const n = 30
	results := make([]int32, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for j := range results {
		wg.Add(1)
		go func(j int) { defer wg.Done(); results[j], errs[j] = a.Compact(ctx, "silo", "file", "same") }(j)
	}
	wg.Wait()
	for j := range results {
		if errs[j] != nil || results[j] != results[0] || results[j] <= 0 {
			t.Fatalf("allocation %d: %d %v", j, results[j], errs[j])
		}
	}
	for j := 0; j < 20; j++ {
		id, e := a.Compact(ctx, "silo", "file", fmt.Sprint(j))
		if e != nil || id == results[0] {
			t.Fatalf("unique %d: %d %v", j, id, e)
		}
		ref, e := a.Resolve(ctx, "silo", "file", id)
		if e != nil || ref != fmt.Sprint(j) {
			t.Fatalf("resolve %s %v", ref, e)
		}
	}
	b, err := compactids.New(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.Compact(ctx, "silo", "file", "same")
	if err != nil || id != results[0] {
		t.Fatalf("restart %d %v", id, err)
	}
	if _, err = b.Resolve(ctx, "other", "file", id); err == nil {
		t.Fatal("cross surface resolved")
	}
	if _, err = b.Resolve(ctx, "silo", "other", id); err == nil {
		t.Fatal("cross kind resolved")
	}
}
