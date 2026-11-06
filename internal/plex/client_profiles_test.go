package plex

import (
	"net/http/httptest"
	"testing"
)

func TestClientProfileCatalogAndResolution(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, tc := range []struct {
		query         string
		status, count int
	}{
		{"", 200, 78}, {"?type=0", 200, 78}, {"?type=00", 200, 78}, {"?type=42", 200, 7}, {"?type=042", 200, 7}, {"?type=", 400, 0}, {"?type=43", 400, 0}, {"?type=no", 400, 0},
	} {
		rec := f.request(t, "GET", "/clients/profiles"+tc.query, map[string]string{"Accept": "application/json"})
		if rec.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, rec.Code, rec.Body.String())
		}
		if tc.status != 200 {
			continue
		}
		mc := decodeJSON(t, rec)
		profiles := mc["Profile"].([]any)
		if len(profiles) != tc.count {
			t.Fatalf("%s count=%d", tc.query, len(profiles))
		}
		if tc.count == 7 {
			found := 0
			for _, raw := range profiles {
				p := raw.(map[string]any)
				if p["selected"] == true {
					found++
					if p["title"] != "Universal TV" {
						t.Fatalf("selected %v", p)
					}
				}
			}
			if found != 1 {
				t.Fatalf("selected %d", found)
			}
		}
	}
	for _, tc := range []struct{ explicit, platform, device, want string }{
		{"Chrome", "Firefox", "Roku", "Web"}, {"unknown", "Firefox", "Roku", "Firefox"}, {"", "unknown", "Roku", "Roku-7.x"}, {"Roku-6.x", "", "", "Roku-7.x"}, {"Plex Home Theater-RaspberryPi", "", "", "Raspberry PI"}, {"bad", "bad", "bad", ""},
	} {
		if got := resolvedPlayerProfile(&Request{ProfileName: tc.explicit, Platform: tc.platform, Device: tc.device}); got != tc.want {
			t.Fatalf("%+v got %q", tc, got)
		}
	}
	r := httptest.NewRequest("GET", "/?X-Plex-Client-Profile-Name=Chrome&X-Plex-Platform=Firefox", nil)
	r.Header["X-Plex-Client-Profile-Name"] = []string{""}
	pr := ParseRequest(r)
	if got := resolvedPlayerProfile(pr); got != "Firefox" {
		t.Fatalf("empty header fell through to query: %q", got)
	}
}
