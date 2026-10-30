package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// preUpgradeFiles reads the settings and credential key that match the
// database as it is before this start, for a pre-migrate snapshot. Missing
// files are skipped (a fresh install has neither).
func preUpgradeFiles(dataDir string) ([]store.SnapshotFile, error) {
	var failures []error
	var out []store.SnapshotFile
	for _, name := range []string{"config.json", config.CredentialKeyFile} {
		b, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("read pre-upgrade %s: %w", name, err))
			}
			continue
		}
		out = append(out, store.SnapshotFile{Name: name, Data: b})
	}
	return out, errors.Join(failures...)
}
