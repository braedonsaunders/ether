package reqmemo

import (
	"context"
	"errors"
	"testing"
)

func TestDo(t *testing.T) {
	loads := 0
	load := func() (int, error) { loads++; return 7, nil }
	if v, _ := Do(context.Background(), "k", load); v != 7 || loads != 1 {
		t.Fatal("no memo must load")
	}
	ctx := With(context.Background())
	for range 3 {
		if v, err := Do(ctx, "k", load); v != 7 || err != nil {
			t.Fatal(v, err)
		}
	}
	if loads != 2 {
		t.Fatalf("loads %d", loads)
	}
	if _, err := Do(ctx, "bad", func() (int, error) { return 0, errors.New("x") }); err == nil {
		t.Fatal("error lost")
	}
	if v, _ := Do(ctx, "bad", func() (int, error) { return 3, nil }); v != 3 {
		t.Fatal("a failed load was remembered")
	}
	Forget(ctx, "k")
	_, _ = Do(ctx, "k", load)
	if loads != 3 {
		t.Fatal("Forget kept the value")
	}
}
