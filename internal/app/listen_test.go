package app

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

// Next to a Plex Media Server, the default port is taken: a fresh install
// moves to a fallback port and saves it, so Ether works out of the box.
func TestListenMovesOffBusyDefaultPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(config.DefaultPort))
	if err != nil {
		t.Skipf("port %d is not free on this machine: %v", config.DefaultPort, err)
	}
	defer busy.Close()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error { c.Server.BindAddress = "127.0.0.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ln, err := a.listen(cfg.Get())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if port == config.DefaultPort || cfg.Get().Server.Port != port {
		t.Fatalf("listening on %d, saved port %d", port, cfg.Get().Server.Port)
	}

	// A port the owner chose is never moved.
	if err := cfg.Update(func(c *config.Config) error { c.Server.Port = port; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.listen(cfg.Get()); err == nil || cfg.Get().Server.Port != port {
		t.Fatalf("explicit busy port: err %v, saved %d", err, cfg.Get().Server.Port)
	}
}

func TestAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = net.Listen("tcp", ln.Addr().String())
	if !addrInUse(err) {
		t.Fatalf("not recognised as in use: %v", err)
	}
	if addrInUse(nil) || addrInUse(io.EOF) {
		t.Fatal("false positive")
	}
}

func TestBusyWarningUsesTheLANAddress(t *testing.T) {
	urls := []string{
		"http://[fd79::1]:8099",
		"http://127.0.0.1:8099",
		"http://192.168.68.65:8099",
	}
	got := busyPortWarning("8096", urls)
	want := "Another server on this computer is using port 8096; Ether still works at http://192.168.68.65:8099"
	if got != want {
		t.Fatal(got)
	}
	if discoveryBusyWarning(urls) != "Another server on this computer is using discovery; Ether still works at http://192.168.68.65:8099" {
		t.Fatal(discoveryBusyWarning(urls))
	}
}

func TestStartupNextStep(t *testing.T) {
	var cfg config.Config
	ui := "http://localhost:32400/ether/"
	if got := startupNextStep(cfg, ui); got != "Open http://localhost:32400/ether/ to finish setup" {
		t.Fatalf("unfinished setup: got %q", got)
	}
	cfg.SetupComplete = true
	if got := startupNextStep(cfg, ui); got != "Add a TMDB API key in Integrations" {
		t.Fatalf("missing TMDB key: got %q", got)
	}
	cfg.Catalog.TMDBKey = "test-key"
	if got := startupNextStep(cfg, ui); got != "Add a debrid or Usenet account in Sources" {
		t.Fatalf("missing source: got %q", got)
	}
	cfg.Debrid.Providers = []config.DebridProvider{{Enabled: true, APIKey: "rd-token"}}
	if got := startupNextStep(cfg, ui); got != "" {
		t.Fatalf("ready config: got %q", got)
	}
}
