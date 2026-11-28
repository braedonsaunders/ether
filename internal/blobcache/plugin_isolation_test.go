package blobcache

import (
	"context"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPluginNamespaceIsolationNoPrefixLeak(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	put, open, del := c.HostBackends(nil)
	victimLib := core.PluginLibraryKey("com.acme.music-pro", "album")
	ref, err := put(context.Background(), "com.acme.music-pro", victimLib, "secret", strings.NewReader("victim data"))
	if err != nil {
		t.Fatal(err)
	}
	if src, err := open(context.Background(), "com.acme.music", ref); err == nil {
		src.Close()
		t.Fatal("plugin com.acme.music opened blob owned by com.acme.music-pro")
	}
	if err := del(context.Background(), "com.acme.music", ref); err != nil {
		t.Fatal(err)
	}
	// Victim blob must still exist after attacker attempted delete.
	src, err := open(context.Background(), "com.acme.music-pro", ref)
	if err != nil {
		t.Fatalf("victim blob missing after cross-plugin delete: %v", err)
	}
	src.Close()
}
