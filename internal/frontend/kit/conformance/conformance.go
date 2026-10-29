// Package conformance reads release-generated, embedded protocol evidence.
package conformance

import (
	_ "embed"
	"encoding/json"
	"time"
)

//go:embed summaries.json
var embedded []byte

// Summary records the result of the recorded and live corpus checks. Client
// certification is separate from corpus comparison and must not be inferred.
type Summary struct {
	ProtocolVersion  string   `json:"protocolVersion"`
	Date             string   `json:"date"`
	Blocking         *int     `json:"blocking"`
	CertifiedClients []string `json:"certifiedClients"`
}

type Manifest struct {
	Schema    int                `json:"schema"`
	Protocols map[string]Summary `json:"protocols"`
}

// Status reports only evidence matching the currently advertised protocol
// version. A clean corpus without certified native clients remains partial.
func Status(protocol, version string) string { return StatusFrom(embedded, protocol, version) }

func StatusFrom(data []byte, protocol, version string) string {
	var manifest Manifest
	if json.Unmarshal(data, &manifest) != nil || manifest.Schema != 1 || protocol == "" || version == "" {
		return "unverified"
	}
	summary, ok := manifest.Protocols[protocol]
	if !ok || summary.ProtocolVersion != version || summary.Blocking == nil || *summary.Blocking < 0 || summary.CertifiedClients == nil {
		return "unverified"
	}
	if _, err := time.Parse(time.RFC3339, summary.Date); err != nil {
		return "unverified"
	}
	for _, client := range summary.CertifiedClients {
		if client == "" {
			return "unverified"
		}
	}
	if *summary.Blocking > 0 {
		return "failing"
	}
	if len(summary.CertifiedClients) == 0 {
		return "partial"
	}
	return "passing"
}
