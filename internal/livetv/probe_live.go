package livetv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type liveProber interface {
	ProbeLive(context.Context, string) (*core.MediaInfo, error)
}

type liveProbeEntry struct {
	info    core.MediaInfo
	expires time.Time
}

type liveProbeFlight struct {
	done chan struct{}
	info *core.MediaInfo
	err  error
}

// liveProbeTTL keeps a successful codec read across channel changes in one
// viewing session. Errors are never cached.
const liveProbeTTL = 15 * time.Minute

// A short cache reuses capability-independent codec analysis on channel switches.
// Errors are never cached; URL changes naturally invalidate signed/rotated feeds.
func (s *Service) probeLiveInput(ctx context.Context, input string) (*core.MediaInfo, error) {
	return s.probeLiveCached(ctx, input, input)
}

// probeSessionInput probes the session's input through its relay grant. The
// cache stays keyed by the upstream URL and headers, not the per-tune token.
func (s *Service) probeSessionInput(ctx context.Context, ss *session) (*core.MediaInfo, error) {
	input, err := s.relayInput(ss)
	if err != nil {
		return nil, err
	}
	key := ss.inputURL
	if len(ss.inputHeaders) != 0 {
		encoded, _ := json.Marshal(ss.inputHeaders)
		key = stableID(ss.inputURL, string(encoded))
	}
	return s.probeLiveCached(ctx, key, input)
}

func (s *Service) probeLiveCached(ctx context.Context, key, input string) (*core.MediaInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.probeMu.Lock()
	if entry, found := s.probeCache[key]; found && s.o.Now().Before(entry.expires) {
		s.probeMu.Unlock()
		info := entry.info
		return &info, nil
	}
	if flight, ok := s.probeWait[key]; ok {
		s.probeMu.Unlock()
		select {
		case <-flight.done:
			if flight.err != nil || flight.info == nil {
				return flight.info, flight.err
			}
			info := *flight.info
			return &info, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &liveProbeFlight{done: make(chan struct{})}
	if s.probeWait == nil {
		s.probeWait = map[string]*liveProbeFlight{}
	}
	s.probeWait[key] = flight
	s.probeMu.Unlock()

	info, err := s.probeLiveUncached(ctx, input)
	s.probeMu.Lock()
	if err == nil && info != nil && info.Video() != nil {
		s.storeProbeLocked(key, *info)
	}
	flight.info, flight.err = info, err
	close(flight.done)
	delete(s.probeWait, key)
	s.probeMu.Unlock()
	return info, err
}

func (s *Service) probeLiveUncached(ctx context.Context, input string) (*core.MediaInfo, error) {
	if p, ok := s.o.Transcoder.(liveProber); ok {
		return p.ProbeLive(ctx, input)
	}
	return s.o.Transcoder.Probe(ctx, input)
}

func (s *Service) storeProbeLocked(key string, info core.MediaInfo) {
	if s.probeCache == nil {
		s.probeCache = map[string]liveProbeEntry{}
	}
	if len(s.probeCache) >= 256 {
		for key, e := range s.probeCache {
			if !s.o.Now().Before(e.expires) {
				delete(s.probeCache, key)
			}
		}
		if len(s.probeCache) >= 256 {
			var oldest string
			var expires time.Time
			for key, e := range s.probeCache {
				if oldest == "" || e.expires.Before(expires) {
					oldest, expires = key, e.expires
				}
			}
			delete(s.probeCache, oldest)
		}
	}
	s.probeCache[key] = liveProbeEntry{info: info, expires: s.o.Now().Add(liveProbeTTL)}
}
