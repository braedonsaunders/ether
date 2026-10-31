package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		name = f.Name
	}
	return name
}

// TestScopeClassifiesEveryField fails when a setting is added to Config
// (or to a section split between shared and local) without deciding
// whether cluster nodes share it: add it to sectionScopes or mixedSections.
func TestScopeClassifiesEveryField(t *testing.T) {
	ct := reflect.TypeOf(Config{})
	seen := map[string]bool{}
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		name := jsonName(f)
		seen[name] = true
		_, whole := sectionScopes[name]
		sub, mixed := mixedSections[name]
		switch {
		case whole && mixed:
			t.Errorf("config field %q is classified twice", name)
		case !whole && !mixed:
			t.Errorf("config field %q (Config.%s) is not classified as shared or node-local; add it to sectionScopes or mixedSections in scope.go", name, f.Name)
		case mixed:
			if f.Type.Kind() != reflect.Struct {
				t.Errorf("mixed section %q is not a struct", name)
				continue
			}
			subSeen := map[string]bool{}
			for j := 0; j < f.Type.NumField(); j++ {
				sf := f.Type.Field(j)
				sn := jsonName(sf)
				subSeen[sn] = true
				if _, ok := sub[sn]; !ok {
					t.Errorf("config field %s.%s (%s.%s) is not classified as shared or node-local in mixedSections", name, sn, f.Name, sf.Name)
				}
			}
			for sn := range sub {
				if !subSeen[sn] {
					t.Errorf("mixedSections lists %s.%s, which is not a field", name, sn)
				}
			}
		}
	}
	for name := range sectionScopes {
		if !seen[name] {
			t.Errorf("sectionScopes lists %q, which is not a Config field", name)
		}
	}
	for name := range mixedSections {
		if !seen[name] {
			t.Errorf("mixedSections lists %q, which is not a Config field", name)
		}
	}
}

func TestScopeOf(t *testing.T) {
	for path, want := range map[string]Scope{
		"server.port":                Local,
		"server.bindAddress":         Local,
		"server.machineIdentifier":   Shared,
		"plex.token":                 Shared,
		"plex.publicPort":            Local,
		"cache.dir":                  Local,
		"cache.budgetBytes":          Local,
		"cache.prefetchBytes":        Shared,
		"transcode.hwAccel":          Local,
		"database":                   Local,
		"spike":                      Local,
		"usenet":                     Shared,
		"indexers":                   Shared,
		"prowlarr":                   Shared,
		"setupComplete":              Shared,
		"debrid.providers":           Shared,
		"debrid.cachedOnly":          Shared,
		"debrid.removeUncached":      Shared,
		"debrid.chunkBytes":          Local,
		"debrid.readAheadChunks":     Local,
		"debrid.maxConnections":      Local,
		"resolver":                   Shared,
		"resolver.sourcePreference":  Shared,
		"resolver.sourceScoreMargin": Shared,
		"downloads.diskBudgetBytes":  Local,
		"extras.resolverCommand":     Local,
		"personalize":                Shared,
	} {
		if got := ScopeOf(path); got != want {
			t.Errorf("ScopeOf(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSplitAndMergeShared(t *testing.T) {
	a := Defaults("/a")
	a.Server.Port, a.Server.FriendlyName = 32401, "Cluster"
	a.Plex.Token, a.Plex.PublicPort = "owner-token", 40001
	a.Cache.BudgetBytes, a.Cache.PrefetchBytes = 1<<30, 7
	a.Indexers = []Indexer{{ID: "ix", APIKey: "k"}}
	a.Database = Database{Driver: DatabasePostgres, URL: "postgres://a"}
	a.Plex.CompatibilityVersion = "1.2.3"

	var local map[string]any
	if err := json.Unmarshal(LocalJSON(a), &local); err != nil {
		t.Fatal(err)
	}
	if _, ok := local["indexers"]; ok {
		t.Error("indexers leaked into the local part")
	}
	if local["server"].(map[string]any)["port"] != float64(32401) {
		t.Errorf("local server = %v", local["server"])
	}
	shared := SharedJSON(a)
	if strings.Contains(string(shared), "postgres://a") || strings.Contains(string(shared), "40001") {
		t.Errorf("node-local settings leaked into the shared part: %s", shared)
	}

	b := Defaults("/b")
	b.Server.Port, b.Plex.PublicPort, b.Cache.BudgetBytes = 32402, 40002, 2<<30
	b.Plex.CompatibilityVersion = "stale"
	got, err := MergeShared(b, shared)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.Port != 32402 || got.Plex.PublicPort != 40002 || got.Cache.BudgetBytes != 2<<30 || got.Cache.Dir != b.Cache.Dir {
		t.Errorf("local settings not kept: %+v %+v %+v", got.Server, got.Plex, got.Cache)
	}
	if got.Server.FriendlyName != "Cluster" || got.Plex.Token != "owner-token" || got.Cache.PrefetchBytes != 7 ||
		len(got.Indexers) != 1 || got.Server.MachineIdentifier != a.Server.MachineIdentifier || got.Plex.CompatibilityVersion != "1.2.3" {
		t.Errorf("shared settings not applied: %+v", got)
	}

	// A cleared omitempty shared value must clear the stale local mirror.
	a.Plex.CompatibilityVersion = ""
	got, err = MergeShared(b, SharedJSON(a))
	if err != nil {
		t.Fatal(err)
	}
	if got.Plex.CompatibilityVersion != "" {
		t.Errorf("cleared shared value kept stale %q", got.Plex.CompatibilityVersion)
	}

	// Shared settings a document lacks (older writer) take defaults, and
	// node-local keys inside a shared document are ignored.
	got, err = MergeShared(b, []byte(`{"server":{"port":1,"friendlyName":"Old"},"database":{"url":"postgres://evil"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.Port != 32402 || got.Database.URL != "" || got.Server.FriendlyName != "Old" {
		t.Errorf("merge of partial document = %+v %+v", got.Server, got.Database)
	}
	if got.Catalog.Language != "en-US" || got.Updates.Channel != "stable" {
		t.Errorf("missing shared sections did not take defaults: %+v %+v", got.Catalog, got.Updates)
	}
}
