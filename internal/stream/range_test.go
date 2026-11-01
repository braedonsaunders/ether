package stream

import (
	"bytes"
	"context"
	"testing"
)

func TestMetadataRange(t *testing.T) {
	src := bytes.NewReader([]byte("abcdefghijklmnopqrstuvwxyz"))
	got, err := MetadataRange(context.Background(), src, 4, 6)
	if err != nil || string(got) != "efghij" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := MetadataRange(context.Background(), src, -1, 2); err == nil {
		t.Fatal("negative offset accepted")
	}
	empty, err := MetadataRange(context.Background(), src, 10, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty %q %v", empty, err)
	}
}
