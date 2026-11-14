package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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
	if e.Kind == KindRequest || e.Kind == KindRequestStarted {
		e.Route = redactNetworkText(requestRoute(e.Route))
	}
	e.Query = clipFree(e.Query, 4096, uses)
	if e.Response != nil {
		v := *e.Response
		if v.TotalSize != nil {
			n := *v.TotalSize
			v.TotalSize = &n
		}
		if v.More != nil {
			more := *v.More
			v.More = &more
		}
		v.Manifest = boundManifestFacts(v.Manifest, uses)
		v.Decision = clipNetwork(v.Decision, 80, uses)
		v.ErrorCode = clipNetwork(v.ErrorCode, 80, uses)
		v.Error = clipNetwork(v.Error, maxExcerpt, uses)
		remaining := 64
		v.Items = boundTitles(v.Items, uses, &remaining, &v.Truncated)
		if len(v.Hubs) > 64 {
			v.Hubs = v.Hubs[:64]
			v.Truncated = true
		}
		v.Hubs = append([]HubSummary(nil), v.Hubs...)
		for i := range v.Hubs {
			h := &v.Hubs[i]
			h.Identifier = clipFree(redactHubID(networkInput(h.Identifier)), maxString, uses)
			h.Title = clipNetwork(h.Title, maxString, uses)
			h.Style = clipNetwork(h.Style, 80, uses)
			h.Type = clipNetwork(h.Type, 80, uses)
			h.Items = boundTitles(h.Items, uses, &remaining, &v.Truncated)
		}
		e.Response = &v
	}
	e.Protocol = clipFree(e.Protocol, 32, uses)
	e.Method = clipFree(e.Method, 16, uses)
	e.Range = clipFree(e.Range, 64, uses)
	e.Product = clipFree(e.Product, 80, uses)
	e.Platform = clipFree(e.Platform, 80, uses)
	e.Model = clipFree(e.Model, 80, uses)
	e.DeviceName = clipFree(e.DeviceName, 80, uses)
	e.Device = clipFree(e.Device, 80, uses)
	e.AppVersion = clipFree(e.AppVersion, 40, uses)
	if e.Kind == KindRequest || e.Kind == KindRequestStarted || e.Client != nil {
		e.Product = redactNetworkText(e.Product)
		e.Platform = redactNetworkText(e.Platform)
		e.Model = redactNetworkText(e.Model)
		e.Device = redactNetworkText(e.Device)
		e.AppVersion = redactNetworkText(e.AppVersion)
	}
	e.QualityRequested = clipFree(e.QualityRequested, 40, uses)
	e.QualitySelected = clipFree(e.QualitySelected, 40, uses)
	e.Profile = clipFree(e.Profile, 80, uses)
	e.Decision = clipFree(e.Decision, 40, uses)
	e.Reason = clipNetwork(e.Reason, maxExcerpt, uses)
	e.Rejection = clipNetwork(e.Rejection, maxExcerpt, uses)
	e.ErrorExcerpt = clipNetwork(e.ErrorExcerpt, maxExcerpt, uses)
	e.FFmpegExcerpt = clipNetwork(e.FFmpegExcerpt, maxExcerpt, uses)
	e.Title = clipNetwork(e.Title, maxString, uses)
	e.SourceName = clipNetwork(e.SourceName, maxString, uses)
	e.Username = clipNetwork(e.Username, 80, uses)
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
		c.Reason = clipNetwork(c.Reason, maxString, uses)
		c.Name = clipNetwork(c.Name, maxString, uses)
	}
	e.Playback = boundPlayback(e.Playback, uses)
	e.Client = boundClient(e.Client, uses)
	if e.Bytes < 0 {
		e.Bytes = 0
	}
	if e.Repeat < 0 {
		e.Repeat = 0
	}
	return e
}

func boundTitles(in []string, uses []secretUse, remaining *int, truncated *bool) []string {
	if len(in) > *remaining {
		in = in[:*remaining]
		*truncated = true
	}
	*remaining -= len(in)
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, clipNetwork(s, maxString, uses))
	}
	return out
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
		return string(e.Kind) + "|" + strconv.Quote(e.DeviceKey) + "|" + strconv.FormatInt(e.ViewerID, 10) + "|" + strconv.Quote(e.SessionKey) + "|" + strconv.Quote(e.AttemptKey) + "|" + strconv.Quote(coarseRoute(e.Route)) + "|" + itoa(e.Status)
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

func boundManifestFacts(in []string, uses []secretUse) []string {
	if len(in) > 64 {
		in = in[:64]
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = clipNetwork(v, maxExcerpt, uses)
	}
	return out
}
