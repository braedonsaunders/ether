//go:build windows

package service

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestLocalURLRegistryRoundTrip(t *testing.T) {
	// Exercise the production registry operations in an isolated user key;
	// offline tests must not write a real installation's machine endpoint.
	path := fmt.Sprintf(`SOFTWARE\EtherLocalURLTest-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { registry.DeleteKey(registry.CURRENT_USER, path) })
	if _, err := readLocalURL(registry.CURRENT_USER, path); err == nil {
		t.Fatal("missing endpoint accepted")
	}
	for _, base := range []string{"http://127.0.0.1:32400", "http://127.0.0.1:32450", "http://[::1]:32500"} {
		if err := writeLocalURL(registry.CURRENT_USER, path, base); err != nil {
			t.Fatal(err)
		}
		if got, err := readLocalURL(registry.CURRENT_USER, path); err != nil || got != base {
			t.Fatalf("got %q %v", got, err)
		}
	}
	for _, bad := range []string{"https://localhost:32450", "file:///C:/private", "http://user:secret@localhost:32450", "http://localhost:32450/?token=secret", "http://localhost:0", "http://localhost:70000"} {
		if err := writeLocalURL(registry.CURRENT_USER, path, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	if err := key.SetStringValue("URL", "not a URL"); err != nil {
		t.Fatal(err)
	}
	if _, err := readLocalURL(registry.CURRENT_USER, path); err == nil {
		t.Fatal("accepted corrupt endpoint")
	}
}

func TestLocalURLKeySeparatesInstallations(t *testing.T) {
	a, err := localURLKey(`C:\ProgramData\Ether`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := localURLKey(`c:\PROGRAMDATA\Ether\.`)
	if err != nil {
		t.Fatal(err)
	}
	c, err := localURLKey(`C:\OtherEther`)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == c {
		t.Fatalf("keys %s %s %s", a, b, c)
	}
	if strings.Contains(strings.ToLower(a), "programdata") {
		t.Fatal("published private directory path")
	}
}
