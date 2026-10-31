package diagnostics

import "strings"

// ClassifyRequest maps a request to an observation.
//
// A poll, including HTTP 404, is only KindPoll. A playlist request is only
// KindStreamRequested. KindMediaBytes requires a successful media response,
// not merely bytes written: a 404 or 500 body is KindMediaRequest.
// /:/timeline is KindTimelineReceived until the handler records
// KindPlaybackReported itself. This function never returns
// KindPlaybackReported, KindSourceRejected, KindSourceChosen,
// KindEncoderStarted, or KindEncoderFailed.
func ClassifyRequest(path string, status int, contentType string, bytesWritten int64) Kind {
	p := strings.ToLower(stripQuery(path))
	switch {
	case strings.Contains(p, "/player/proxy") || strings.HasSuffix(p, "/poll"):
		return KindPoll
	case strings.Contains(p, "/transcode/universal/decision"):
		return KindPlaybackRequested
	case strings.Contains(p, "/transcode/universal/start."):
		return KindStreamRequested
	case strings.Contains(p, "/transcode/universal/session/") || strings.Contains(p, "/library/parts/"):
		if mediaDelivered(status, contentType, bytesWritten) {
			return KindMediaBytes
		}
		return KindMediaRequest
	case strings.Contains(p, "/:/timeline") || strings.Contains(p, "/:/progress"):
		return KindTimelineReceived
	default:
		return KindDevice
	}
}

// mediaDelivered reports whether the response is media bytes rather than an
// error or document body that happened to be written on a media path.
func mediaDelivered(status int, contentType string, bytesWritten int64) bool {
	if bytesWritten <= 0 || (status != 200 && status != 206) {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "", "application/json", "text/html", "text/plain", "text/xml", "application/xml", "application/problem+json":
		return false
	default:
		return strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") ||
			ct == "application/octet-stream" || ct == "application/mp4" ||
			ct == "application/vnd.apple.mpegurl" || ct == "application/x-mpegurl" ||
			ct == "application/dash+xml"
	}
}
