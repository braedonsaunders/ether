package plexguid

import "strings"

// ClientIdentifier is the X-Plex-Client-Identifier sent to
// metadata.provider.plex.tv. Prefer the per-install Plex client id (minted
// on first run), then the server machine identifier. The shared
// "ether-metadata" fallback exists only for a config that has neither.
func ClientIdentifier(plexClientID, machineID string) string {
	if id := strings.TrimSpace(plexClientID); id != "" {
		return id
	}
	if id := strings.TrimSpace(machineID); id != "" {
		return id
	}
	return "ether-metadata"
}
