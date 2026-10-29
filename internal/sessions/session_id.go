package sessions

import (
	"crypto/rand"
	"fmt"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// sessionID preserves existing IDs for other surfaces. Silo Apple requires a
// canonical random UUID; the media grant is generated independently.
func sessionID(protocol frontend.Protocol) (string, error) {
	if protocol != "silo" {
		return rand.Text(), nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}
