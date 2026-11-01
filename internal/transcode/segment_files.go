package transcode

import "net/http"

type segmentAvailability uint8

const (
	segmentPending segmentAvailability = iota
	segmentFinalizing
	segmentReady
	segmentRetry
)

// segmentFilesLocked resolves the cached delivery phase independently of
// encoder scheduling. All returned files belong to one continuous timeline.
func (s *Session) segmentFilesLocked(track, n int, method string) ([]string, segmentAvailability) {
	done := s.done[track]
	if !done[n] {
		return nil, segmentPending
	}
	last := !s.isDASH() && s.total > 0 && n == s.total-1
	if last && !s.tailSettledLocked(n) {
		return nil, segmentFinalizing
	}
	paths := []string{s.trackSegPath(track, n)}
	if s.container == ContainerMatroska {
		prev, served := s.mkvDelivery[track]
		seek := !served || (prev.segment != n-1 && prev.segment != n) || (prev.segment == n && prev.seek)
		if !seek && n > 0 && s.inflight[n-1] > 0 {
			// The covering GOP is still on the wire. An empty Cluster
			// here is the first bytes a resume client would decode, and
			// Plex tvOS never leaves "seeking".
			seek = true
		}
		if seek {
			path, ok := s.decodableMKVLocked(track, n)
			if !ok {
				delete(done, n)
				return nil, segmentRetry
			}
			paths[0] = path
		}
		if method != http.MethodHead {
			if s.mkvDelivery == nil {
				s.mkvDelivery = make(map[int]mkvDelivery)
			}
			s.mkvDelivery[track] = mkvDelivery{segment: n, seek: seek}
		}
	}
	if last {
		for k := n + 1; done[k]; k++ {
			paths = append(paths, s.trackSegPath(track, k))
		}
	}
	return paths, segmentReady
}
