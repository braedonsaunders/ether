package resolver

import (
	"math"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

type deliverySample struct {
	rate  float64
	at    time.Time
	count int
}
type deliveryState struct {
	mu      sync.Mutex
	samples map[string]deliverySample
}

// Measured delivery is a short-lived routing hint, never a permanent rejection.
// Usenet listings share the configured NNTP pool; torrent evidence is scoped
// to the release because different debrid links may use different CDNs.
func deliveryKey(r core.Release) string {
	if r.Protocol == core.ProtocolTorrent || r.Protocol == core.ProtocolHTTP {
		return r.Protocol + ":" + r.ID
	}
	return "usenet"
}

func (s *Service) ObserveSourceDelivery(r core.Release, bytesPerSecond float64) {
	if bytesPerSecond <= 0 || math.IsNaN(bytesPerSecond) || math.IsInf(bytesPerSecond, 0) {
		return
	}
	s.delivery.mu.Lock()
	defer s.delivery.mu.Unlock()
	if s.delivery.samples == nil {
		s.delivery.samples = map[string]deliverySample{}
	}
	now := s.now()
	key := deliveryKey(r)
	old := s.delivery.samples[key]
	if !old.at.IsZero() && now.Sub(old.at) < 2*time.Second {
		return
	}
	if now.Sub(old.at) > 2*time.Minute {
		old = deliverySample{}
	}
	rate := bytesPerSecond
	if old.count > 0 {
		rate = math.Min(rate, old.rate*.8+rate*.2)
	}
	if len(s.delivery.samples) >= 256 {
		clear(s.delivery.samples)
	}
	s.delivery.samples[key] = deliverySample{rate: rate, at: now, count: old.count + 1}
}

func (s *Service) deliveryFit(r core.Release, caps core.ClientCaps, title *core.Title) int {
	fit := sourceBandwidthFit(r, caps, title)
	if r.Size <= 0 || title == nil || title.Runtime <= 0 {
		return fit
	}
	if source, ok := s.indexer.(interface{ RecentSourceThroughput(core.Release) float64 }); ok {
		if rate := source.RecentSourceThroughput(r); rate > 0 {
			if float64(r.Size)/title.Runtime.Seconds() > rate*.75 {
				return -1
			}
			if fit >= 0 {
				return 1
			}
		}
	}
	s.delivery.mu.Lock()
	e := s.delivery.samples[deliveryKey(r)]
	s.delivery.mu.Unlock()
	// Multiple observations prevent one startup header read from governing
	// source choice. Leave 25% headroom for variable bitrate and uncertainty.
	if e.count < 3 || s.now().Sub(e.at) > 2*time.Minute {
		return fit
	}
	if float64(r.Size)/title.Runtime.Seconds() > e.rate*.75 {
		return -1
	}
	if fit < 0 {
		return fit
	}
	return 1
}

// ChannelSourceAllowed applies the hard channel source envelope first.
// Measurements may reject a slow encode, never admit a heavier source.
func (s *Service) ChannelSourceAllowed(r core.Release, caps core.ClientCaps, title *core.Title) bool {
	if caps.BroadcastBitrateKbps <= 0 {
		return true
	}
	if title == nil || title.Runtime <= 0 || !releasepolicy.ChannelSourceAllowed(r, title.Runtime, caps.BroadcastBitrateKbps) {
		return false
	}
	// Pack and direct-link listings may not have an individual file size.
	// Playback rechecks this gate with the selected file before admission.
	if r.Size <= 0 {
		return true
	}
	required := float64(r.Size) / title.Runtime.Seconds()
	rate := float64(0)
	if source, ok := s.indexer.(interface{ RecentSourceThroughput(core.Release) float64 }); ok {
		rate = source.RecentSourceThroughput(r)
	}
	s.delivery.mu.Lock()
	e := s.delivery.samples[deliveryKey(r)]
	s.delivery.mu.Unlock()
	if e.count >= 3 && s.now().Sub(e.at) <= 2*time.Minute {
		if rate <= 0 || e.rate < rate {
			rate = e.rate
		}
	}
	// Known slow delivery rejects even a small encode.
	if rate > 0 {
		return required <= rate*.75
	}
	return true
}
