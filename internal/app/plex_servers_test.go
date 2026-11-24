package app

import (
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestPlexDirectHash(t *testing.T) {
	for host, want := range map[string]string{
		"1-2-3-4.abc123.plex.direct":        "abc123",
		"1-2-3-4.ABC123.plex.direct:32400":  "abc123",
		"1-2-3-4.abc123.plex.direct.":       "abc123",
		"abc123.plex.direct":                "",
		"example.com":                       "",
		"[::1]:32400":                       "",
		"1-2-3-4.abc123.plex.direct.evil.x": "",
	} {
		if got := plexDirectHash(host); got != want {
			t.Errorf("plexDirectHash(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestCountSharesSkipsOwnerAndHome(t *testing.T) {
	n := countShares([]plextv.Friend{
		{PlexID: 1, Owned: true},
		{PlexID: 2, Home: true},
		{PlexID: 3},
		{PlexID: 4},
		{PlexID: 0},
	})
	if n != 2 {
		t.Fatalf("shares = %d, want 2", n)
	}
}

func TestPlexFillOrderIsOldestFirst(t *testing.T) {
	var c config.Config
	c.PlexExtraServers = []config.PlexExtraServer{
		{ID: "p4", State: config.PlexServerStandby, AddedAt: 40},
		{ID: "p3", State: config.PlexServerActive, AddedAt: 30},
		{ID: "p2", State: config.PlexServerActive, AddedAt: 20},
		{ID: "p5", State: config.PlexServerStandby, AddedAt: 35},
	}
	ids := func(in []config.PlexExtraServer) (out []string) {
		for _, s := range in {
			out = append(out, s.ID)
		}
		return out
	}
	if got := ids(activePlexOrder(c)); len(got) != 2 || got[0] != "p2" || got[1] != "p3" {
		t.Fatalf("active order = %v", got)
	}
	if got := ids(standbyPlexOrder(c)); len(got) != 2 || got[0] != "p5" || got[1] != "p4" {
		t.Fatalf("standby order = %v", got)
	}
}

func TestRequestTokenReadsHeaderAndQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/library/sections?X-Plex-Token=q", nil)
	if got := requestToken(r); got != "q" {
		t.Fatalf("query token = %q", got)
	}
	r.Header.Set("X-Plex-Token", "h")
	if got := requestToken(r); got != "h" {
		t.Fatalf("header token = %q", got)
	}
}
