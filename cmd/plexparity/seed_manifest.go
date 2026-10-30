package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

// seedFile records only a fixture-relative path, byte count, and digest; it
// deliberately excludes file contents and configuration values.
type seedFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func checkSeedCopy(expected, source, copied []seedFile) error {
	if len(expected) == 0 || !reflect.DeepEqual(expected, source) || !reflect.DeepEqual(expected, copied) {
		return fmt.Errorf("seed source/copy digest mismatch")
	}
	return nil
}

func seedManifest(root string) ([]seedFile, error) {
	var result []seedFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular fixture file: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, readErr := io.Copy(h, f)
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		result = append(result, seedFile{Path: filepath.ToSlash(rel), Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	if len(result) == 0 {
		return nil, fmt.Errorf("empty seed fixture")
	}
	return result, nil
}
