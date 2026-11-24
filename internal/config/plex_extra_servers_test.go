package config

import "testing"

func TestPlexSeatDefaults(t *testing.T) {
	var c Config
	if got := c.PlexSeatsPerServer(); got != 99 {
		t.Fatalf("seats = %d, want 99", got)
	}
	if got := c.PlexSeatWarning(); got != 89 {
		t.Fatalf("warning = %d, want 89", got)
	}
	c.Plex.SeatsPerServer = 2
	if got := c.PlexSeatWarning(); got != 1 {
		t.Fatalf("warning for 2 seats = %d, want 1", got)
	}
	c.Plex.SeatWarning = 2
	if got := c.PlexSeatWarning(); got != 2 {
		t.Fatalf("explicit warning = %d, want 2", got)
	}
	c.Plex.SeatWarning = 5 // above the seat count: falls back to 90%
	if got := c.PlexSeatWarning(); got != 1 {
		t.Fatalf("out of range warning = %d, want 1", got)
	}
}

func TestWithPlexServerOverlaysIdentityOnly(t *testing.T) {
	var c Config
	c.Server.FriendlyName = "Ether"
	c.Server.MachineIdentifier = "main-machine"
	c.Server.Port = 32400
	c.Plex.ClientIdentifier = "main-client"
	c.Plex.Token = "main-token"
	c.Plex.OwnerPlexID = 1
	c.Plex.Claimed = true
	c.Plex.CertHash = "mainhash"
	c.Plex.CertFile, c.Plex.KeyFile = "/c.pem", "/k.pem"
	c.Plex.RemoteAccess = true

	s := NewPlexExtraServer(c.NextPlexServerID(), PlexServerAccount{ID: 2, Username: "second", Token: "acct"}, 10)
	if s.ID != "p2" || s.State != PlexServerStandby || s.MachineIdentifier == "" || s.ClientIdentifier == "" {
		t.Fatalf("new server = %+v", s)
	}
	view := c.WithPlexServer(s)
	if view.Server.MachineIdentifier != s.MachineIdentifier || view.Plex.ClientIdentifier != s.ClientIdentifier {
		t.Fatal("identity not replaced")
	}
	if view.Server.FriendlyName != "Ether" {
		t.Fatalf("name = %q, want inherited Ether", view.Server.FriendlyName)
	}
	if view.Plex.Token != "" || view.Plex.Claimed || view.Plex.CertHash != "" || view.Plex.CertFile != "" {
		t.Fatalf("main account leaked into the extra view: %+v", view.Plex)
	}
	if view.Server.Port != 32400 || !view.Plex.RemoteAccess {
		t.Fatal("shared settings must stay the main server's")
	}
	if c.Server.MachineIdentifier != "main-machine" || c.Plex.Token != "main-token" {
		t.Fatal("overlay changed the original")
	}

	s.Name = "  Second  "
	if got := c.WithPlexServer(s).Server.FriendlyName; got != "Second" {
		t.Fatalf("renamed = %q", got)
	}

	c.PlexExtraServers = []PlexExtraServer{s}
	view.Plex.Token, view.Plex.OwnerPlexID, view.Plex.Claimed, view.Plex.CertHash = "srv", 2, true, "hash2"
	c.StorePlexServer("p2", view)
	got, ok := c.PlexExtraServer("p2")
	if !ok || got.Token != "srv" || got.OwnerPlexID != 2 || !got.Claimed || got.CertHash != "hash2" {
		t.Fatalf("stored = %+v", got)
	}
	if c.Plex.Token != "main-token" || c.Plex.CertHash != "mainhash" {
		t.Fatal("storing an extra server touched the main identity")
	}
	if id := c.NextPlexServerID(); id != "p3" {
		t.Fatalf("next id = %q, want p3", id)
	}
}

func TestPlexClientIdentifierInUse(t *testing.T) {
	var c Config
	c.Server.MachineIdentifier = "main-machine"
	c.Plex.ClientIdentifier = "main-client"
	c.PlexExtraServers = []PlexExtraServer{{ID: "p2", MachineIdentifier: "p2-machine", ClientIdentifier: "p2-client"}}
	for _, id := range []string{"", "main-machine", "main-client", "p2-machine", "p2-client"} {
		if !c.PlexClientIdentifierInUse(id) {
			t.Errorf("%q reported free", id)
		}
	}
	a, b := NewPlexClientIdentifier(), NewPlexClientIdentifier()
	if len(a) != 32 || a == b || c.PlexClientIdentifierInUse(a) {
		t.Fatalf("fresh identifiers %q %q", a, b)
	}
}
