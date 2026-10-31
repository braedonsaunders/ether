package app

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestDeliveredMainMigrationBoundary records bounded handle adoption on b69.
// That candidate includes experience_receipts v5, frontend_tickets v2,
// frontend_image_grants v6, and capability_headers v3. Account sync is
// plexwatch v4 and state_plexwatch v3. rules and watchsync stay
// unregistered. auth_sessions still applies after auth.
func TestDeliveredMainMigrationBoundary(t *testing.T) {
	known := store.KnownMigrations()
	want := map[string]int{
		"auth":                   7,
		"auth_sessions":          1,
		"experience_receipts":    5,
		"experience_row_views":   3,
		"release_observations":   1,
		"frontend_tickets":       3,
		"frontend_image_grants":  7,
		"capability_headers":     3,
		"playback":               4,
		"capability_tickets":     1,
		"sessions_source_offers": 1,
		"handle_reservations":    2,
		"viewer_alerts":          2,
		"viewer_push":            2,
		"watchlist_episodes":     1,
		"notify_once":            1,
		"plexwatch":              4,
		"state_plexwatch":        3,
	}
	for component, version := range want {
		if known[component] != version {
			t.Errorf("%s version = %d, want %d", component, known[component], version)
		}
	}
	for _, absent := range []string{"rules", "watchsync"} {
		if _, ok := known[absent]; ok {
			t.Errorf("excluded component %s is registered at v%d", absent, known[absent])
		}
	}
	if !store.HasDependency("auth_sessions", "auth") {
		t.Fatal("auth_sessions must RegisterAfter(auth)")
	}
	for _, dep := range []struct{ component, after string }{
		{"identity_grants", "auth"},
		{"playback", "capability_headers"},
		{"frontend_tickets", "auth"},
		{"frontend_tickets", "identity_grants"},
		{"frontend_image_grants", "auth"},
		{"frontend_image_grants", "identity_grants"},
		{"capability_tickets", "identity_grants"},
		{"capability_tickets", "capability_headers"},
		{"sessions_source_offers", "capability_headers"},
		{"sessions_source_offers", "sessions"},
	} {
		if !store.HasDependency(dep.component, dep.after) {
			t.Errorf("%s missing RegisterAfter %s", dep.component, dep.after)
		}
	}
	order := store.ComponentOrder()
	index := map[string]int{}
	for i, component := range order {
		index[component] = i
	}
	for _, pair := range [][2]string{
		{"auth", "auth_sessions"},
		{"auth", "identity_grants"},
		{"identity_grants", "frontend_tickets"},
		{"identity_grants", "frontend_image_grants"},
		{"identity_grants", "capability_tickets"},
		{"capability_headers", "capability_tickets"},
	} {
		if index[pair[0]] > index[pair[1]] {
			t.Errorf("%s (at %d) applied after %s (at %d)", pair[0], index[pair[0]], pair[1], index[pair[1]])
		}
	}
}

func TestConfiguredLibrariesFollowRenameWithoutChangingIDs(t *testing.T) {
	dir := t.TempDir()
	st, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: st}
	before := a.configuredLibraries()
	if len(before) != 2 || before[0].ID != 1 || before[1].ID != 2 || before[0].Name != "Movies" || before[1].Name != "TV Shows" || !before[0].Virtual || !before[1].Virtual {
		t.Fatalf("defaults = %+v", before)
	}
	if err := st.Update(func(c *config.Config) error {
		c.Server.MoviesLibraryName = "Basement Films"
		c.Server.TVShowsLibraryName = "Basement Series"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := a.configuredLibraries()
	if after[0].ID != 1 || after[1].ID != 2 || after[0].Name != "Basement Films" || after[1].Name != "Basement Series" || !after[0].Virtual || !after[1].Virtual {
		t.Fatalf("live names = %+v", after)
	}
}
