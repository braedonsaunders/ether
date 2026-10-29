// Package debrid streams torrent releases through owner-configured debrid
// services (Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link,
// Offcloud), and Stremio addons' own HTTP stream links (direct.go). A
// torrent the service already holds ("cached") is added to the owner's
// account and its main video file is read over HTTPS range requests;
// nothing is downloaded in full and Ether never seeds.
//
// The rest of Ether treats a release payload as opaque bytes (normally an
// NZB). A debrid release's payload is a small descriptor (see Payload), and
// the Indexer, Engine and health-check wrappers in this package dispatch on
// it, delegating everything else to the Usenet implementations.
package debrid

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// payloadMagic prefixes every encoded Payload.
const payloadMagic = "ETHER-DEBRID/1\n"

// Payload identifies one file of a torrent held by a debrid provider. It is
// persisted by playback for resume, so it stores stable identifiers (info
// hash, magnet, provider torrent and file ids, path and size) from which a
// fresh link can be generated after the last one expires. It never holds
// an API key.
type Payload struct {
	Key       string              `json:"key,omitempty"`       // payload cache key (release + target)
	ReleaseID string              `json:"releaseId,omitempty"` // core.Release.ID (segment cache key)
	Provider  string              `json:"provider"`            // config.DebridProvider.ID
	Kind      string              `json:"kind"`                // config.DebridKinds
	InfoHash  string              `json:"infoHash"`            // lower-case hex v1 info hash
	Magnet    string              `json:"magnet,omitempty"`    // magnet URI used to (re)add the torrent
	TorrentID string              `json:"torrentId,omitempty"` // provider torrent / magnet id
	FileID    string              `json:"fileId,omitempty"`    // provider file id
	Path      string              `json:"path"`                // file path inside the torrent
	Size      int64               `json:"size"`                // file size in bytes
	Link      string              `json:"link,omitempty"`      // provider-side file link (RD/AllDebrid), unrestricted on demand
	URL       string              `json:"url,omitempty"`       // last direct download URL
	Expires   time.Time           `json:"expires,omitempty"`   // when URL should be regenerated
	Target    *core.ReleaseTarget `json:"target,omitempty"`
	Release   string              `json:"release,omitempty"` // release name, for logs
	// Direct HTTP streams (Kind KindHTTP, direct.go): Link is the addon's
	// stream link, Headers the request headers it requires and Origin the
	// owner-configured addon origin. Link and Headers can carry the
	// addon's credentials.
	Headers map[string]string `json:"headers,omitempty"`
	Origin  string            `json:"origin,omitempty"`
}

// IsPayload reports whether b is an encoded debrid Payload.
func IsPayload(b []byte) bool { return bytes.HasPrefix(b, []byte(payloadMagic)) }

// Encode serializes p with the payload prefix.
func (p Payload) Encode() []byte {
	body, _ := json.Marshal(p)
	return append([]byte(payloadMagic), body...)
}

// DecodePayload parses an encoded Payload.
func DecodePayload(b []byte) (Payload, error) {
	if !IsPayload(b) {
		return Payload{}, errors.New("debrid: not a debrid payload")
	}
	var p Payload
	if err := json.Unmarshal(b[len(payloadMagic):], &p); err != nil {
		return Payload{}, fmt.Errorf("debrid: corrupt payload: %w", err)
	}
	if p.Size <= 0 || (p.InfoHash == "" && !(p.IsDirect() && p.Link != "")) {
		return Payload{}, errors.New("debrid: incomplete payload")
	}
	return p, nil
}

// Name is the file's base name.
func (p Payload) Name() string { return path.Base(strings.ReplaceAll(p.Path, "\\", "/")) }

// handle is the provider-facing identity of the payload's file.
func (p Payload) handle() Handle {
	return Handle{TorrentID: p.TorrentID, FileID: p.FileID, Path: p.Path, Size: p.Size, Link: p.Link, Magnet: p.Magnet, InfoHash: p.InfoHash}
}

// fileTag identifies the file independently of the provider (base name and
// size); cached chunks carry it so a chunk of another file is never served.
func (p Payload) fileTag() [8]byte {
	sum := sha1.Sum([]byte(strings.ToLower(p.Name()) + "|" + fmt.Sprint(p.Size)))
	var t [8]byte
	copy(t[:], sum[:8])
	return t
}
