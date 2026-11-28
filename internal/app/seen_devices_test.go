package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The console returns a bounded device window with exact latest-play details,
// even when friendly names collide and history contains older plays.
func TestSeenPlaybackDevicesBounded(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides = []core.ClientCapabilityOverride{{Platform: "Android", Device: "Box", Force: true, DolbyVisionProfiles: []int{5, 7, 8}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		client := fmt.Sprintf("client-%d", i)
		play := &core.PlaySession{ID: fmt.Sprintf("play-%d", i), UserID: 1, Key: 99, BytesServed: 100 << 20, Username: client, StartedAt: time.Now(), Client: core.ClientCaps{ClientID: client, DeviceName: "Living room", Device: "Box", Platform: "Android", Product: "Plex for Android (TV)"}}
		if err := a.stats.Record(ctx, play, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := a.seenPlaybackDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 100 {
		t.Fatalf("got %d devices", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Device != "Box" || row.Name != "Living room" || row.App != "Plex for Android (TV)" || row.Viewer != row.ClientIdentifier || row.OverrideIndex == nil || *row.OverrideIndex != 0 {
			t.Fatalf("latest play identity lost: %+v", row)
		}
		if seen[row.ClientIdentifier] {
			t.Fatal("duplicate device")
		}
		seen[row.ClientIdentifier] = true
	}
}

// Seen devices expose the runtime match field without impersonating DeviceName,
// agree with clientreg.MatchingOverrides on both Device and DeviceName overrides,
// and record device_match for protocols that only carry DeviceName.
func TestSeenPlaybackDevicesSharedMatcherAndPreFill(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	overrides := []core.ClientCapabilityOverride{
		{Product: "PlxNative", Device: "PlxNative (LG TV)", MaxHeight: 2160},
		{Product: "Swiftfin", Device: "Bedroom Apple TV", MaxHeight: 1080},
		{ID: "android-tv", MaxHeight: 1080},
	}
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides = overrides
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, play := range []*core.PlaySession{
		{
			ID: "plx-play", UserID: 1, Key: 1, BytesServed: 10 << 20, Username: "owner", StartedAt: now,
			Client: core.ClientCaps{ClientID: "plx-lg", Product: "PlxNative", Platform: "webOS", Device: "LG webOS TV", DeviceName: "PlxNative (LG TV)"},
		},
		{
			ID: "jf-play", UserID: 1, Key: 2, BytesServed: 10 << 20, Username: "owner", StartedAt: now,
			Client: core.ClientCaps{ClientID: "jf-atv", Product: "Swiftfin", DeviceName: "Bedroom Apple TV"},
		},
	} {
		if err := a.stats.Record(ctx, play, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-v1 play whose X-Plex-Device was never recorded and cannot be derived:
	// device_match stays empty (never faked from name), so a friendly name with
	// "TV" does not falsely trigger the built-in android-tv rule.
	if _, err := a.db.ExecContext(ctx, `INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('legacy-play',1,3,'owner','Movie','Android','Living Room TV','direct_play',100,100,'','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO stats_device_registry(id,client_identifier,name,platform,created_at,last_seen,latest_play_id) VALUES(99,'legacy-phone','Living Room TV','Android',100,100,'legacy-play')`); err != nil {
		t.Fatal(err)
	}

	rows, err := a.seenPlaybackDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]seenDevice{}
	for _, row := range rows {
		byID[row.ClientIdentifier] = row
		id := clientreg.Identity{Product: row.App, Platform: row.Platform, Device: row.Device, DeviceName: row.Name}
		matched := clientreg.MatchingOverrides(id, overrides)
		if len(matched) == 0 && row.OverrideIndex != nil {
			t.Fatalf("%s: console claimed override %d but runtime matched none", row.ClientIdentifier, *row.OverrideIndex)
		}
		if len(matched) > 0 && (row.OverrideIndex == nil || *row.OverrideIndex != matched[len(matched)-1]) {
			t.Fatalf("%s: console overrideIndex %v != runtime %v", row.ClientIdentifier, row.OverrideIndex, matched)
		}
	}
	if r := byID["plx-lg"]; r.Device != "LG webOS TV" || r.Name != "PlxNative (LG TV)" || r.OverrideIndex == nil || *r.OverrideIndex != 0 {
		t.Fatalf("plx-lg: %+v", r)
	}
	if r := byID["jf-atv"]; r.Device != "Bedroom Apple TV" || r.Name != "Bedroom Apple TV" || r.OverrideIndex == nil || *r.OverrideIndex != 1 {
		t.Fatalf("jf-atv: %+v", r)
	}
	if r := byID["legacy-phone"]; r.Device != "" || r.Name != "Living Room TV" || r.OverrideIndex != nil {
		t.Fatalf("legacy-phone should keep empty device and not match android-tv: %+v", r)
	}
}

func TestStatsSeenDevicesBackfillMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Seed plays resembling a database upgraded from stats_seen_devices v1:
	// - plx-old (pre-v1) + plx-new (post-v1 with device_match) -> plx-old gets 'LG webOS TV'
	// - jf-old (pre-v1) + jf-new (post-v1 Jellyfin with product<>'' and empty device_match) -> both get 'Bedroom Apple TV'
	// - orphan-old (pre-v1 with no newer play) -> stays ''
	for _, q := range []string{
		`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('plx-old',1,1,'u','T','webOS','PlxNative (LG TV)','direct_play',100,100,'','')`,
		`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('plx-new',1,2,'u','T','webOS','PlxNative (LG TV)','direct_play',200,200,'PlxNative','LG webOS TV')`,
		`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('jf-old',1,3,'u','T','','Bedroom Apple TV','direct_play',100,100,'','')`,
		`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('jf-new',1,4,'u','T','','Bedroom Apple TV','direct_play',200,200,'Swiftfin','')`,
		`INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,product,device_match) VALUES('orphan-old',1,5,'u','T','Android','Old Phone','direct_play',100,100,'','')`,
		`DELETE FROM schema_migrations WHERE component='stats_seen_devices' AND version=2`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	want := map[string]string{
		"plx-old":    "LG webOS TV",
		"plx-new":    "LG webOS TV",
		"jf-old":     "Bedroom Apple TV",
		"jf-new":     "Bedroom Apple TV",
		"orphan-old": "",
	}
	for id, exp := range want {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT device_match FROM stats_plays WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(id, err)
		}
		if got != exp {
			t.Errorf("%s: device_match = %q, want %q", id, got, exp)
		}
	}
}

// Seen devices show the reported hardware model, so two clients that both
// call themselves "Apple TV" can be told apart, and a later record that
// carries no model keeps the one already seen.
func TestSeenPlaybackDevicesReportModel(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	now := time.Now()
	atv := func(id, client, model string) *core.PlaySession {
		return &core.PlaySession{ID: id, UserID: 1, Key: 1, BytesServed: 10 << 20, Username: "owner", StartedAt: now,
			Client: core.ClientCaps{ClientID: client, Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV", DeviceName: "Apple TV", Model: model}}
	}
	for _, play := range []*core.PlaySession{atv("hd-play", "atv-hd", "AppleTV5,3"), atv("uhd-play", "atv-4k", "AppleTV14,1")} {
		if err := a.stats.Record(ctx, play, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	// A progress update without the model must not erase it.
	if err := a.stats.Record(ctx, atv("hd-play", "atv-hd", ""), false, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := a.seenPlaybackDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]seenDevice{}
	for _, row := range rows {
		got[row.ClientIdentifier] = row
	}
	hd, uhd := got["atv-hd"], got["atv-4k"]
	if hd.Model != "AppleTV5,3" || uhd.Model != "AppleTV14,1" {
		t.Fatalf("models lost: %+v / %+v", hd, uhd)
	}
	// The generic "Apple TV" name becomes the product name, so the two
	// boxes are distinct; the HD box points at its own profile row.
	if hd.Name != "Apple TV HD" || uhd.Name != "Apple TV 4K (3rd gen)" {
		t.Fatalf("names = %q / %q", hd.Name, uhd.Name)
	}
	if hd.Profile != "apple-tv-hd" || uhd.Profile != "" {
		t.Fatalf("profiles = %q / %q", hd.Profile, uhd.Profile)
	}
}

func TestSeenDeviceNameKeepsOwnerNames(t *testing.T) {
	atv := clientreg.Identity{Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV"}
	for _, tc := range []struct{ name, device, model, want string }{
		{"Apple TV", "Apple TV", "AppleTV5,3", "Apple TV HD"},
		{"Apple TV", "Apple TV", "5,3", "Apple TV HD"},
		{"Apple TV", "Apple TV", "14,1", "Apple TV 4K (3rd gen)"},
		{"", "Apple TV", "AppleTV11,1", "Apple TV 4K (2nd gen)"},
		{"Living room", "Apple TV", "AppleTV5,3", "Living room"},
		{"Apple TV", "Apple TV", "", "Apple TV"},
		{"AFTMM", "AFTMM", "", "Fire TV Stick 4K"},
		{"Ettiene's Fire TV 4K", "AFTMM", "", "Ettiene's Fire TV 4K"},
	} {
		if got := seenDeviceName(tc.name, tc.device, tc.model, atv); got != tc.want {
			t.Fatalf("seenDeviceName(%q, %q, %q) = %q, want %q", tc.name, tc.device, tc.model, got, tc.want)
		}
	}
}
