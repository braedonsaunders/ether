package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// maxAcceptKey is the longest correlation id stored as itself.
	// Longer ids are hashed in full up to maxHashKey, never truncated.
	maxAcceptKey = 256
	maxHashKey   = 8 << 10
)

// boundKey keeps a short id unchanged, hashes a longer acceptable id so a
// shared prefix cannot collide, and rejects anything longer as uncorrelated.
// Rejection does not read past the length check's need: the hash path hashes
// the whole id it accepts.
func boundKey(s string) (key string, uncorrelated bool) {
	if s == "" {
		return "", false
	}
	if len(s) <= maxAcceptKey {
		return strings.Clone(s), false
	}
	if len(s) > maxHashKey {
		return "", true
	}
	sum := sha256.Sum256([]byte(s))
	return "h:" + hex.EncodeToString(sum[:]), false
}

// boundEvent clips fields before they are retained. Correlation keys are
// never prefix-truncated. Free text is redacted inside a bounded copy.
func boundEvent(e Event, uses []secretUse) Event {
	var devDrop, sesDrop bool
	e.DeviceKey, devDrop = boundKey(e.DeviceKey)
	e.SessionKey, sesDrop = boundKey(e.SessionKey)
	e.AttemptKey, _ = boundKey(e.AttemptKey)
	if devDrop || (e.DeviceKey == "" && sesDrop) {
		e.Uncorrelated = true
	}
	e.Route = clipFree(stripQuery(e.Route), maxString, uses)
	e.Protocol = clipFree(e.Protocol, 32, uses)
	e.Method = clipFree(e.Method, 16, uses)
	e.Range = clipFree(e.Range, 64, uses)
	e.Product = clipFree(e.Product, 80, uses)
	e.Platform = clipFree(e.Platform, 80, uses)
	e.Model = clipFree(e.Model, 80, uses)
	e.DeviceName = clipFree(e.DeviceName, 80, uses)
	e.AppVersion = clipFree(e.AppVersion, 40, uses)
	e.QualityRequested = clipFree(e.QualityRequested, 40, uses)
	e.QualitySelected = clipFree(e.QualitySelected, 40, uses)
	e.Profile = clipFree(e.Profile, 80, uses)
	e.Decision = clipFree(e.Decision, 40, uses)
	e.Reason = clipFree(e.Reason, maxExcerpt, uses)
	e.Rejection = clipFree(e.Rejection, maxExcerpt, uses)
	e.ErrorExcerpt = clipFree(e.ErrorExcerpt, maxExcerpt, uses)
	e.FFmpegExcerpt = clipFree(e.FFmpegExcerpt, maxExcerpt, uses)
	e.Title = clipFree(e.Title, maxString, uses)
	e.SourceName = clipFree(e.SourceName, maxString, uses)
	e.Username = clipFree(e.Username, 80, uses)
	e.Peer = clipFree(e.Peer, 80, uses)
	if e.Media != nil {
		m := *e.Media
		m.Container = clipFree(m.Container, 40, uses)
		m.VideoCodec = clipFree(m.VideoCodec, 40, uses)
		m.AudioCodec = clipFree(m.AudioCodec, 40, uses)
		m.Channels = clipFree(m.Channels, 16, uses)
		e.Media = &m
	}
	if len(e.Candidates) > maxCandidates {
		e.Candidates = append([]Candidate(nil), e.Candidates[:maxCandidates]...)
	} else if len(e.Candidates) > 0 {
		e.Candidates = append([]Candidate(nil), e.Candidates...)
	}
	for i := range e.Candidates {
		c := &e.Candidates[i]
		c.Protocol = clipFree(c.Protocol, 32, uses)
		c.VideoCodec = clipFree(c.VideoCodec, 40, uses)
		c.AudioCodec = clipFree(c.AudioCodec, 40, uses)
		c.Reason = clipFree(c.Reason, maxString, uses)
		c.Name = clipFree(c.Name, maxString, uses)
	}
	if e.Bytes < 0 {
		e.Bytes = 0
	}
	if e.Repeat < 0 {
		e.Repeat = 0
	}
	return e
}

func coarseRoute(route string) string {
	route = stripQuery(route)
	i := strings.LastIndex(route, "/")
	if i <= 0 {
		return route
	}
	last := route[i+1:]
	if last == "" {
		return route
	}
	digits := true
	for _, c := range last {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits || strings.Contains(last, ".ts") || strings.Contains(last, ".m4s") || strings.HasPrefix(last, "seg") {
		return route[:i]
	}
	return route
}

func coalesceKey(e Event) string {
	switch e.Kind {
	case KindPoll, KindMediaRequest, KindMediaBytes:
		return string(e.Kind) + "|" + e.DeviceKey + "|" + coarseRoute(e.Route) + "|" + itoa(e.Status)
	default:
		return ""
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
