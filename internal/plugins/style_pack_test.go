package plugins

import "testing"

func TestStylePackManifest(t *testing.T) {
	m := baseManifest()
	m.Ether.Runtime = RuntimeDecl{Kind: "data"}
	m.Ether.Capabilities = []Capability{CapLiveTVPack}
	m.Ether.Contributes = Contributes{Looks: []PackStyle{{ID: "style", File: "looks/style.json"}}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Ether.Runtime.Entry = "code.js"
	if err := m.Validate(); err == nil {
		t.Fatal("data runtime accepted executable entry")
	}
	m.Ether.Runtime.Entry = ""
	m.Ether.Contributes.Looks[0].File = "../style.json"
	if err := m.Validate(); err == nil {
		t.Fatal("data runtime accepted escaping file")
	}
}
