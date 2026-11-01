package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestClusterFixtureCopiesWorldAndRejectsExternalDatabase(t *testing.T) {
	seed := t.TempDir()
	c, err := config.Open(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update(func(c *config.Config) error { c.Server.FriendlyName = "fixture"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "ether.db"), []byte("fixture-world"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := configureClusterNode(seed, filepath.Join(root, "external"), "postgres://elsewhere/ether", "11111111111111111111111111111111", 32400); err == nil {
		t.Fatal("external PostgreSQL database accepted")
	}
	for i, id := range []string{"11111111111111111111111111111111", "22222222222222222222222222222222"} {
		dir := filepath.Join(root, id[:1])
		if err := configureClusterNode(seed, dir, "postgres://parity-postgres/fixture?sslmode=disable", id, 32400+i); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "ether.db"))
		if err != nil || string(got) != "fixture-world" {
			t.Fatalf("world copy=%q err=%v", got, err)
		}
		identity, err := cluster.LoadIdentity(dir)
		if err != nil || identity.NodeID != id {
			t.Fatalf("node identity=%+v err=%v", identity, err)
		}
		cfg, err := config.Open(dir)
		if err != nil || cfg.Get().Server.Port != 32400+i || cfg.Get().Database.Driver != config.DatabasePostgres {
			t.Fatalf("node config=%+v err=%v", cfg, err)
		}
	}
}

func TestClusterSessionWaitRequiresActualSharedID(t *testing.T) {
	count := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count < 2 {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"Session":{"id":"cluster-real"}}]}}`)
	}))
	defer s.Close()
	if err := waitClusterSession(context.Background(), s.Client(), s.URL, http.Header{}, nil, "cluster-real", true); err != nil || count < 2 {
		t.Fatalf("shared session count=%d err=%v", count, err)
	}
}
