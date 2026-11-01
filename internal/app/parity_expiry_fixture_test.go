//go:build plexparity && !scorecard

package app

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestParityExpiryReadAtPacesSingleLargeRead(t *testing.T) {
	t.Setenv("ETHER_PARITY_EXPIRY_ONLY", "1")
	f, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	want := bytes.Repeat([]byte("fixture"), 40000)
	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	p := &parityMedia{File: f}
	got := make([]byte, len(want))
	start := time.Now()
	n, err := p.ReadAt(got, 0)
	if err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("large offset-zero read was not paced")
	}
}
