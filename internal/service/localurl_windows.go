//go:build windows

package service

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// HKLM\Software inherits read access for ordinary users, but only privileged
// processes can write it. Publish only a non-secret URL, never config or tokens.
// Separate installations/data directories must not redirect each other's tray.
func localURLKey(dataDir string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return fmt.Sprintf(`SOFTWARE\Ether\LocalEndpoints\%x`, digest), nil
}

// PublishLocalURL records the actual bound address after the listener opens.
func PublishLocalURL(dataDir, baseURL string) error {
	key, err := localURLKey(dataDir)
	if err != nil {
		return err
	}
	return writeLocalURL(registry.LOCAL_MACHINE, key, baseURL)
}

// ReadLocalURL does not require access to the private service data directory.
func ReadLocalURL(dataDir string) (string, error) {
	key, err := localURLKey(dataDir)
	if err != nil {
		return "", err
	}
	return readLocalURL(registry.LOCAL_MACHINE, key)
}

func validLocalURL(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535 && u.Scheme == "http" && u.Hostname() != "" &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func writeLocalURL(root registry.Key, path, base string) error {
	if !validLocalURL(base) {
		return fmt.Errorf("invalid local service URL")
	}
	k, _, err := registry.CreateKey(root, path, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("URL", base)
}

func readLocalURL(root registry.Key, path string) (string, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()
	base, _, err := k.GetStringValue("URL")
	if err != nil {
		return "", err
	}
	if !validLocalURL(base) {
		return "", fmt.Errorf("invalid published local service URL")
	}
	return base, nil
}
