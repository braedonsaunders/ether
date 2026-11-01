package admin_test

import (
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"image/png"
	"strings"
	"testing"
)

func TestFrontendQR(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	call := func(query string) int {
		r := req("GET", "/ether/api/frontends/qr?url="+query, "")
		r.Header.Set("X-Plex-Token", ownerToken)
		w := serve(h, r)
		if w.Code == 200 {
			if _, err := png.Decode(strings.NewReader(w.Body.String())); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code
	}
	if got := call("http%3A%2F%2F192.168.1.2%3A8096"); got != 200 {
		t.Fatal(got)
	}
	if got := call("https%3A%2F%2Fuser%3Apass%40host%2F"); got != 400 {
		t.Fatal(got)
	}
}
