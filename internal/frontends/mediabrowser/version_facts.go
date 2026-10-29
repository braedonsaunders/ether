package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"strings"
)

// versionSourceFacts uses only source-provided facts; names that resemble
// paths are rejected even though the host contract promises a basename.
func (i *instance) versionSourceFacts(source map[string]any, version frontend.Version) {
	if version.FileName != "" && !strings.ContainsAny(version.FileName, "/\\\r\n") && version.FileName != "." && version.FileName != ".." {
		source["Name"] = version.FileName
	}
	if i.protocol == "jellyfin" && version.ETag != "" {
		source["ETag"] = version.ETag
	}
}
func (i *instance) versionItemFacts(dto map[string]any, versions []frontend.Version) {
	if i.protocol == "jellyfin" && len(versions) == 1 && versions[0].PartCount != nil {
		dto["PartCount"] = *versions[0].PartCount
	}
}
