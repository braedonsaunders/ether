package plexguid

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// ClientIdentifier is the X-Plex-Client-Identifier sent to Plex's metadata
// and discover providers: the install's Plex client id (minted on first
// run), else the server's machine identifier, else a fresh random id, so
// installs never share one fixed identifier.
func ClientIdentifier(plexClientID, machineID string) string {
	if id := strings.TrimSpace(plexClientID); id != "" {
		return id
	}
	if id := strings.TrimSpace(machineID); id != "" {
		return id
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
