package admin_test

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestIdentityEndpointShowsEffectiveValues(t *testing.T) {
	store := newStore(t, func(c *config.Config) {
		c.Plex.Token = ownerToken
		c.Plex.Claimed = true
		c.ClientIdentity = config.ClientIdentity{
			SearchTV: "sonarr", SearchMovies: "radarr",
			GrabTV: config.IdentitySame, GrabMovies: config.IdentitySame,
			UserAgent: "Custom/1.0", Headers: map[string]string{"Authorization": "Bearer super-secret"},
		}
		c.Indexers = []config.Indexer{
			{ID: "a", Name: "A", URL: "https://a.example", APIKey: "key-a", Enabled: true},
			{ID: "b", Name: "B", URL: "https://b.example", APIKey: "key-b", Enabled: true, SearchIdentity: "prowlarr"},
		}
	})
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	rec := serve(h, req("GET", "/ether/api/config/identity", ""))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var view admin.IdentityView
	decode(t, rec, &view)
	if view.TVSearch != "sonarr" || view.MovieSearch != "radarr" || view.TVGrab != "sonarr" || view.MovieGrab != "radarr" {
		t.Fatalf("global view = %+v", view)
	}
	if len(view.Indexers) != 2 {
		t.Fatalf("indexers = %+v", view.Indexers)
	}
	inherited, overridden := view.Indexers[0], view.Indexers[1]
	if inherited.ID != "a" || inherited.SearchOverride != "" ||
		inherited.EffectiveTVSearch != "sonarr" || inherited.EffectiveMovieSearch != "radarr" ||
		inherited.EffectiveTVGrab != "sonarr" || inherited.EffectiveMovieGrab != "radarr" {
		t.Fatalf("inherited indexer = %+v", inherited)
	}
	if overridden.SearchOverride != "prowlarr" ||
		overridden.EffectiveTVSearch != "prowlarr" || overridden.EffectiveMovieSearch != "prowlarr" ||
		overridden.EffectiveTVGrab != "sonarr" || overridden.EffectiveMovieGrab != "radarr" {
		t.Fatalf("overridden indexer = %+v", overridden)
	}
	body := rec.Body.String()
	for _, secret := range []string{"super-secret", "key-a", "key-b"} {
		if strings.Contains(body, secret) {
			t.Fatalf("identity response leaks %q: %s", secret, body)
		}
	}
	if view.UserAgents["ether"] != "Ether/dev" {
		t.Fatalf("ether user agent = %q", view.UserAgents["ether"])
	}
	if view.UserAgents["sonarr"] != "Sonarr/4.0.20.3014 (alpine 3.24.3)" {
		t.Fatalf("sonarr user agent = %q", view.UserAgents["sonarr"])
	}
	if view.UserAgents["prowlarr"] != "Prowlarr/2.7.0.5723 (alpine 3.24.3)" {
		t.Fatalf("prowlarr user agent = %q", view.UserAgents["prowlarr"])
	}
	if view.UserAgents["nzbhydra2"] != "NZBHydra2 9.2.0" {
		t.Fatalf("nzbhydra2 user agent = %q", view.UserAgents["nzbhydra2"])
	}
	if _, ok := view.UserAgents["custom"]; ok {
		t.Fatal("custom has no fixed user agent")
	}
}
