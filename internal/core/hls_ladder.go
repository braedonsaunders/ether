package core

import "net/http"

// HLSRung is one variant of an adaptive HLS/DASH ladder. All rungs of a
// session share deterministic segment numbering over one segment timeline:
// segment N covers the same media on every rung.
type HLSRung struct {
	// ID is the rung's URL component: session/{id}/{ID}/index.m3u8. The
	// primary rung is always "base" (served by the HLSSession methods).
	ID string
	// Bandwidth is the peak bitrate in bits per second (#EXT-X-STREAM-INF
	// BANDWIDTH / DASH Representation bandwidth).
	Bandwidth int
	// AverageBandwidth is the average bitrate in bits per second.
	AverageBandwidth int
	Width            int
	Height           int
	// Codecs is an RFC 6381 codecs string ("avc1...,mp4a...").
	Codecs    string
	FrameRate float64
}

// HLSLadderSession is an HLSSession that can serve a multi-rung ABR
// ladder. Only the primary rung's encoder runs until a client requests a
// segment on another rung; at most one rung encodes at a time.
type HLSLadderSession interface {
	HLSSession
	// Rungs lists the ladder, highest first; Rungs()[0] is "base".
	Rungs() []HLSRung
	// ServeRungPlaylist writes rung's media playlist.
	ServeRungPlaylist(w http.ResponseWriter, r *http.Request, rung string)
	// ServeRungSegment writes segment n of rung (InitSegment numbering as
	// in ServeSegment: the caller passes the parsed segment number).
	ServeRungSegment(w http.ResponseWriter, r *http.Request, rung string, n int)
}

// GoodputSession is a session that measures client delivery goodput.
type GoodputSession interface {
	// GoodputKbps is the rolling segment-delivery estimate; 0 unknown.
	GoodputKbps() int
}

// HLSIndependentSegments is an optional, session-wide guarantee that every
// segment in every advertised rendition is independently decodable. A
// session must not implement it as true unless it can prove that guarantee for
// copied media, all encoder paths, and every rung it actually advertises.
type HLSIndependentSegments interface {
	IndependentSegments() bool
}
