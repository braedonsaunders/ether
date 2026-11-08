package config

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestAttachSharedUpgradesLegacyProfilesAndRetriesConflict(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := store.Get()
	legacy.Quality.Profiles = []core.QualityProfile{
		{Name: "Shared", Import: &core.ProfileImport{Source: "trash", App: "radarr", Profile: "example"}},
		{Name: "Already TV", MediaType: core.MediaTV},
	}
	var raw map[string]any
	if err := json.Unmarshal(SharedJSON(legacy), &raw); err != nil {
		t.Fatal(err)
	}
	profiles := raw["quality"].(map[string]any)["profiles"].([]any)
	profiles[0].(map[string]any)["futureField"] = "preserved"
	profiles[0].(map[string]any)["futureInteger"] = json.Number("9007199254740993")
	encode := func() string {
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := store.sealShared(b)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	backend := &memShared{found: true, doc: SharedDoc{Doc: encode(), Version: 1, KeyFingerprint: store.KeyFingerprint()}}
	backend.conflict = func() {
		profiles[0].(map[string]any)["name"] = "Concurrent rename"
		backend.mu.Lock()
		backend.doc.Doc = encode()
		backend.doc.Version++
		backend.mu.Unlock()
	}
	if err := store.AttachShared(context.Background(), backend, SharedOptions{PollInterval: -1}); err != nil {
		t.Fatal(err)
	}
	got := store.Get().Quality.Profiles
	if len(got) != 3 || got[0].Name != "Concurrent rename" || got[0].MediaType != core.MediaMovie || got[1].Name != got[0].Name || got[1].MediaType != core.MediaTV || got[2].Name != "Already TV" || got[2].MediaType != core.MediaTV || got[0].Import == nil || got[1].Import == nil {
		t.Fatalf("unexpected upgraded profiles: %+v", got)
	}
	plain, _, err := openDocument([]byte(backend.doc.Doc), store.credentialKey)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	profiles = raw["quality"].(map[string]any)["profiles"].([]any)
	for _, p := range profiles[:2] {
		if p.(map[string]any)["futureField"] != "preserved" {
			t.Fatal("upgrade dropped an unknown profile field")
		}
		if p.(map[string]any)["futureInteger"] != json.Number("9007199254740993") {
			t.Fatal("upgrade changed an unknown large integer")
		}
	}
	if backend.doc.Version != 3 {
		t.Fatalf("shared version = %d, want 3", backend.doc.Version)
	}
	peer, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	peer.credentialKey = store.credentialKey
	if err := peer.AttachShared(context.Background(), backend, SharedOptions{PollInterval: -1}); err != nil {
		t.Fatal(err)
	}
	if len(peer.Get().Quality.Profiles) != 3 || backend.doc.Version != 3 {
		t.Fatal("a second node repeated or failed to load the migration")
	}
}

func TestCredentialDocumentKeepsLegacyNumericDestination(t *testing.T) {
	key := make([]byte, 32)
	input := []byte(`{"provider":{"host":"example.invalid","port":1234,"apiKey":"test-value"},"large":9007199254740993}`)
	sealed, err := sealDocument(input, key)
	if err != nil {
		t.Fatal(err)
	}
	// Older documents bind to float64's canonical representation of a port.
	sealed = bytes.Replace(sealed, []byte(`"port": 1234`), []byte(`"port": 1234.0`), 1)
	plain, _, err := openDocument(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["large"] != json.Number("9007199254740993") || got["provider"].(map[string]any)["apiKey"] != "test-value" {
		t.Fatal("document lost a value during the round trip")
	}
}
