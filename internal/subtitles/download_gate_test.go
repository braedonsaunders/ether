package subtitles

import (
	"context"
	"testing"
	"time"
)

func TestProviderDownloadHostGate(t *testing.T) {
	release, err := acquireDownloadHost(context.Background(), "provider.example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if unlock, err := acquireDownloadHost(ctx, "provider.example"); err == nil {
		unlock()
		t.Fatal("parallel download entered same host")
	}
	other, err := acquireDownloadHost(context.Background(), "other.example")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	again, err := acquireDownloadHost(context.Background(), "provider.example")
	if err != nil {
		t.Fatal(err)
	}
	again()
	providerDownloadGates.Lock()
	defer providerDownloadGates.Unlock()
	if len(providerDownloadGates.hosts) != 0 {
		t.Fatalf("leaked host gates: %v", providerDownloadGates.hosts)
	}
}
