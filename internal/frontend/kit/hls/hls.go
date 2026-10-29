// Package hls writes token-free playlists under random session paths.
package hls

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid HLS playlist")

// NewPath creates an unguessable path component for a media session.
func NewPath() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validPath(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Master writes a single-variant master playlist. Segment and playlist URLs
// are generated from the session path, never supplied by an account holder.
func Master(w io.Writer, session string, bandwidth int) error {
	if !validPath(session) || bandwidth <= 0 {
		return ErrInvalid
	}
	_, err := fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=%d\n/%s/index.m3u8\n", bandwidth, session)
	return err
}

// Media writes a finite playlist of segments addressed by index. Durations
// must be positive and finite; no caller-provided URI is interpolated.
func Media(w io.Writer, session string, durations []time.Duration) error {
	if !validPath(session) || len(durations) == 0 {
		return ErrInvalid
	}
	var max time.Duration
	for _, d := range durations {
		if d <= 0 {
			return ErrInvalid
		}
		if d > max {
			max = d
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%.0f\n#EXT-X-MEDIA-SEQUENCE:0\n", math.Ceil(max.Seconds()))
	for i, d := range durations {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n/%s/%d.ts\n", d.Seconds(), session, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	_, err := io.WriteString(w, b.String())
	return err
}
