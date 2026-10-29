package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CachedSpec reads a checksum-pinned API description. The file is populated by
// internal/conform/specs/fetch.sh; it is never fetched during an offline test.
func CachedSpec(manifest, protocol, cacheDir string) ([]byte, error) {
	b, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var data struct {
		Specs []struct {
			Protocol string `json:"protocol"`
			SHA      string `json:"sha256"`
			Version  string `json:"version"`
		} `json:"specs"`
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	names := map[string]string{"jellyfin": "jellyfin-openapi-12.4.json", "emby": "emby-openapi-v2.json", "silo": "silo-openapi.json"}
	name, ok := names[protocol]
	if !ok {
		return nil, fmt.Errorf("unknown protocol %q", protocol)
	}
	var want, version string
	for _, s := range data.Specs {
		if s.Protocol == protocol {
			want = s.SHA
			version = s.Version
			break
		}
	}
	if want == "" {
		return nil, fmt.Errorf("no pinned spec for %s", protocol)
	}
	if protocol == "jellyfin" && version != "" {
		if strings.ContainsAny(version, "/\\") {
			return nil, fmt.Errorf("invalid spec version")
		}
		name = "jellyfin-openapi-" + version + ".json"
	}
	if cacheDir == "" {
		cacheDir = os.Getenv("XDG_CACHE_HOME")
		if protocol == "jellyfin" && version != "" {
			if strings.ContainsAny(version, "/\\") {
				return nil, fmt.Errorf("invalid spec version")
			}
			name = "jellyfin-openapi-" + version + ".json"
		}
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			cacheDir = filepath.Join(home, ".cache")
		}
		cacheDir = filepath.Join(cacheDir, "ether", "conform", "specs")
	}
	b, err = os.ReadFile(filepath.Join(cacheDir, name))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return nil, fmt.Errorf("%s cached spec checksum mismatch", protocol)
	}
	return b, nil
}
