// Package specs exposes the pinned conformance manifest to offline tools.
package specs

import _ "embed"

// Manifest is the checksum-pinned protocol spec manifest.
//
//go:embed manifest.json
var Manifest []byte
