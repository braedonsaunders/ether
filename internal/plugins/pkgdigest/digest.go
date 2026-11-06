// Package pkgdigest defines the canonical Ether plugin package integrity digest.
package pkgdigest

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Sum hashes sorted file-name/content-digest pairs, excluding the digest entry.
func Sum(files map[string][]byte) string {
	names := []string{}
	for n := range files {
		if n != "digest" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	lines := []string{}
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		lines = append(lines, n+"="+hex.EncodeToString(sum[:]))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
