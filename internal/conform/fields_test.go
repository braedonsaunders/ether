package conform

import (
	"strings"
	"testing"
)

func TestLoadBearingAndVolatileListsAreExplicit(t *testing.T) {
	required := []string{
		"viewMode", "viewGroup", "leafCount", "viewedLeafCount", "childCount",
		"size", "totalSize", "type", "nocache", "allowSync", "librarySectionID",
		"hubIdentifier", "context", "style", "more", "generalDecisionCode",
		"directPlayDecisionCode", "transcodeDecisionCode", "mdeDecisionCode",
	}
	have := newSet(LoadBearingFields)
	for _, n := range required {
		if !have[n] {
			t.Errorf("missing load-bearing field %q", n)
		}
	}
	if !keyShapeSet["key"] || !keyShapeSet["ratingKey"] {
		t.Fatal("key/ratingKey must be shape-compared")
	}
	seen := map[string]bool{}
	for _, n := range LoadBearingFields {
		if n == "" || strings.Contains(n, "*") {
			t.Errorf("load-bearing name %q must be exact", n)
		}
		if seen[n] {
			t.Errorf("duplicate load-bearing field %q", n)
		}
		seen[n] = true
	}
	if len(VolatileFields) < 40 {
		t.Fatalf("volatile list is too small to be the explicit per-server set: %d", len(VolatileFields))
	}
	for _, n := range VolatileFields {
		if n == "" {
			t.Fatal("empty volatile pattern")
		}
		if have[n] {
			t.Errorf("volatile list must not repeat load-bearing %q", n)
		}
	}
}

func TestLoadBearingPaths(t *testing.T) {
	for _, path := range []string{
		"/MediaContainer/@viewMode",
		"/MediaContainer/@viewGroup",
		"/MediaContainer/@size",
		"/MediaContainer/Hub/@hubIdentifier",
		"$/MediaContainer/Metadata[]/leafCount",
		"$/MediaContainer/Metadata[]/librarySectionID",
		"/MediaContainer/@generalDecisionCode",
		"/MediaContainer/Video/@key",
		"$/MediaContainer/Metadata[]/ratingKey",
	} {
		if !LoadBearing(path) {
			t.Errorf("%s should be load-bearing", path)
		}
	}
	for _, path := range []string{
		"/MediaContainer/Video/@title",
		"/MediaContainer/@machineIdentifier",
		"$/MediaContainer/Metadata[]/addedAt",
	} {
		if LoadBearing(path) {
			t.Errorf("%s should not be load-bearing", path)
		}
		if !volatile(path) {
			t.Errorf("%s should be volatile", path)
		}
	}
}

func TestIdentifierShape(t *testing.T) {
	if IdentifierShape("/library/metadata/11974/children") != "/library/metadata/{id}/children" {
		t.Fatal(IdentifierShape("/library/metadata/11974/children"))
	}
	if IdentifierShape("11974") != "{id}" || IdentifierShape("abc") != "abc" {
		t.Fatalf("%q %q", IdentifierShape("11974"), IdentifierShape("abc"))
	}
}
