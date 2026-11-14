package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/livetv"
)

func TestOwnerGuideFilterDiscovery(t *testing.T) {
	f, service, ch := liveFixture(t)
	guides := []livetv.OwnerGuide{{ID: "family", Name: "Family", All: true}, {ID: "hidden", Name: "Hidden", All: true, Hidden: true}}
	if err := service.SaveOwnerGuides(context.Background(), guides); err != nil {
		t.Fatal(err)
	}
	for _, accept := range []string{"application/json", "application/xml"} {
		r := f.request(t, "GET", "/media/providers", map[string]string{"Accept": accept})
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "GridChannelFilter") || !strings.Contains(r.Body.String(), "ether.guide.family") || strings.Contains(r.Body.String(), "ether.guide.hidden") {
			t.Fatalf("discovery %s: %d %s", accept, r.Code, r.Body.String())
		}
	}
	r := f.request(t, "GET", liveProviderPath+"/guides/family/channels", map[string]string{"Accept": "application/json"})
	var out struct {
		MediaContainer struct {
			Channel []struct {
				ID string `json:"id"`
			} `json:"Channel"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if r.Code != 200 || len(out.MediaContainer.Channel) != 1 || out.MediaContainer.Channel[0].ID != ch.ID {
		t.Fatalf("filter result %d: %s", r.Code, r.Body.String())
	}
	r = f.request(t, "GET", liveProviderPath+"/guides/hidden/channels", nil)
	if r.Code != 404 {
		t.Fatalf("hidden guide status %d", r.Code)
	}
	guides[0].Name = "Renamed"
	if err := service.SaveOwnerGuides(context.Background(), guides); err != nil {
		t.Fatal(err)
	}
	r = f.request(t, "GET", "/media/providers", map[string]string{"Accept": "application/json"})
	if !strings.Contains(r.Body.String(), "Renamed") || !strings.Contains(r.Body.String(), "ether.guide.family") {
		t.Fatal("rename changed identity or lost title")
	}
}
