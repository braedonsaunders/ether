package core

import (
	"context"
	"testing"
)

func TestStreamKey(t *testing.T) {
	base := context.Background()
	if WithStreamKey(base, nil) != base || StreamKey(base) != nil {
		t.Fatal("a nil key must leave the context untagged")
	}
	k := new(int)
	if got := StreamKey(WithStreamKey(base, k)); got != any(k) {
		t.Fatalf("StreamKey = %v, want %v", got, k)
	}
}
