package config

import "testing"

// Read hands out the live configuration without a copy, so request paths
// can read it per item. That is safe only while an update replaces the
// configuration instead of changing it: a value read before an update
// keeps its slices and maps.
func TestReadSurvivesUpdate(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *Config) error {
		c.Indexers = []Indexer{{ID: "a", Name: "A", MovieCategories: []int{2000}}}
		c.Frontends = map[string]Frontend{"jellyfin": {Enabled: true, Settings: map[string]any{"quickConnect": true}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.Read()
	if err := s.Update(func(c *Config) error {
		c.Indexers[0].Name = "B"
		c.Indexers[0].MovieCategories[0] = 5000
		c.Indexers = append(c.Indexers, Indexer{ID: "c"})
		c.Frontends["jellyfin"].Settings["quickConnect"] = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(before.Indexers) != 1 || before.Indexers[0].Name != "A" || before.Indexers[0].MovieCategories[0] != 2000 || before.Frontends["jellyfin"].Settings["quickConnect"] != true {
		t.Fatalf("an update changed a configuration read before it: %+v", before.Indexers)
	}
	if after := s.Read(); after.Indexers[0].Name != "B" || len(after.Indexers) != 2 {
		t.Fatalf("Read did not see the update: %+v", after.Indexers)
	}
}

func BenchmarkGet(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Get()
	}
}

func BenchmarkRead(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Read()
	}
}
