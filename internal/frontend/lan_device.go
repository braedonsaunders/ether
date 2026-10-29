package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
)

// LANDevice derives a stable DLNA device identity from the verified network
// peer, never from a client-supplied forwarding header or device identifier.
func LANDevice(peer netip.Addr) Device {
	if !peer.IsValid() {
		return Device{}
	}
	sum := sha256.Sum256([]byte(peer.Unmap().String()))
	return Device{ID: "dlna-" + hex.EncodeToString(sum[:16]), Client: "DLNA", Product: "DLNA"}
}
