package conform

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// maxFixtureName bounds fixture file names (without ".json"). Recorded
// decision URLs run to ~240 characters, which breaks checkouts on Windows
// (MAX_PATH); long names keep a readable prefix plus a hash of the full name.
const maxFixtureName = 80

var fixtureNameReplacer = strings.NewReplacer(
	"/", "_", ":", "", "?", "_", "=", "-",
	// Never valid in Windows file names.
	"*", "_", "|", "_", "<", "_", ">", "_", `"`, "_", `\`, "_",
)

// FixtureName is the file name (without ".json") of the fixture for a
// request made with a client profile in XML or JSON.
func FixtureName(profile, path string, jsonMode bool) string {
	ext := "xml"
	if jsonMode {
		ext = "json"
	}
	n := filepath.Base(fixtureNameReplacer.Replace(profile + "_" + ext + path))
	return shortenFixtureName(n)
}

func shortenFixtureName(n string) string {
	if len(n) <= maxFixtureName {
		return n
	}
	sum := sha1.Sum([]byte(n))
	return n[:maxFixtureName-11] + "~" + hex.EncodeToString(sum[:5])
}
