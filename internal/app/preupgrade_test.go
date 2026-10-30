package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestPreUpgradeFilesCapturesSettingsAndKey(t *testing.T) {
	dir := t.TempDir()
	if got, err := preUpgradeFiles(dir); err != nil || len(got) != 0 {
		t.Fatalf("fresh install: %v", got)
	}
	for name, body := range map[string]string{"config.json": `{"setupComplete":true}`, config.CredentialKeyFile: "k", "ether.db": "db"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := preUpgradeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "config.json" || string(got[0].Data) != `{"setupComplete":true}` || got[1].Name != config.CredentialKeyFile {
		t.Fatalf("captured %+v", got)
	}
}

func TestPreUpgradeFilesReportsReadFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := preUpgradeFiles(dir); err == nil {
		t.Fatal("unreadable existing file silently omitted")
	}
}
