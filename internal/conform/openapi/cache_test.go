package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCachedSpecChecksDigest(t *testing.T) {
	dir := t.TempDir()
	spec := []byte(`{"openapi":"3.0.0"}`)
	sum := sha256.Sum256(spec)
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifest, []byte(fmt.Sprintf(`{"specs":[{"protocol":"silo","sha256":"%s"}]}`, hex.EncodeToString(sum[:]))), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "silo-openapi.json")
	if err := os.WriteFile(path, spec, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedSpec(manifest, "silo", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedSpec(manifest, "silo", dir); err == nil {
		t.Fatal("invalid checksum accepted")
	}
}
