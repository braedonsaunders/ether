package admin_test

import (
	"github.com/braedonsaunders/ether/internal/admin"
	"net/http"
	"testing"
)

func TestPlexServersGlobalConfigSwitch(t *testing.T) {
	store := newStore(t, seededConfig)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	for _, enabled := range []string{"false", "true"} {
		r := req(http.MethodPut, "/ether/api/config/plexservers", `{"enabled":`+enabled+`}`)
		r.Header.Set("X-Plex-Token", ownerToken)
		if res := serve(h, r); res.Code != http.StatusOK {
			t.Fatalf("%d: %s", res.Code, res.Body)
		}
		if store.Get().PlexServers.Enabled != (enabled == "true") {
			t.Fatal("flag not persisted")
		}
	}
}
