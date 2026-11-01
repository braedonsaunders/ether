package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedManifestVerifiesEveryCopiedFile(t *testing.T) {
	src := t.TempDir()
	for name, content := range map[string]string{"ether.db": "database-and-model", "config.json": "fixture-config"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyFixture(src, dst); err != nil {
		t.Fatal(err)
	}
	expected, err := seedManifest(src)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := seedManifest(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSeedCopy(expected, expected, copied); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "ether.db"), []byte("different-model"), 0600); err != nil {
		t.Fatal(err)
	}
	copied, err = seedManifest(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSeedCopy(expected, expected, copied); err == nil {
		t.Fatal("changed copied database accepted")
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	changedSource, err := seedManifest(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSeedCopy(expected, changedSource, expected); err == nil {
		t.Fatal("changed source accepted")
	}
	if err := os.Symlink("ether.db", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := seedManifest(src); err == nil {
		t.Fatal("symlink accepted")
	}
}
