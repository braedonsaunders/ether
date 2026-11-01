package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSessionEventOmitsGrantAndCredentials(t *testing.T) {
	s := frontend.Session{ID: "session", ViewerID: 3, Protocol: "jellyfin", MediaPath: "/grant-secret", Device: core.Device{ID: "phone", Client: "account-secret", DeviceName: "TV"}}
	e := sessionEvent("SessionStarted", s, nil)
	if e.Session == nil || e.Session.MediaPath != "" || e.Session.Device.Client != "" || e.Session.Device.ID != "phone" || e.Session.Device.DeviceName != "TV" || e.Session.Protocol != "jellyfin" {
		t.Fatalf("unsafe lifecycle event: %+v", e.Session)
	}
	if s.MediaPath == "" || s.Device.Client == "" {
		t.Fatal("event mutation changed original session")
	}
}
