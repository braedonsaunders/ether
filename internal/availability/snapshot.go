package availability

import (
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// discoverySnapshot owns the inputs for one discovery operation, including its
// nested episode and retained-release reads. It is never cached across calls.
// Config must return an isolated value (as config.Store.Get does). Keep background
// admission on its live settings path so owner pauses still take effect promptly.
type discoverySnapshot struct {
	now             time.Time
	availability    config.Availability
	policy          string
	feeds           []core.ReleaseFeed
	usenet, torrent bool
}

func (s *Service) discoverySnapshot() discoverySnapshot {
	c := s.cfg()
	feeds := slices.Clone(s.d.Feeds.FeedSources())
	for i := range feeds {
		feeds[i].Categories = slices.Clone(feeds[i].Categories)
	}
	a := c.Availability
	a.IndexerIDs = slices.Clone(a.IndexerIDs)
	return discoverySnapshot{
		now: s.now(), availability: a, policy: settingsPolicy(c), feeds: feeds,
		usenet: settingsUsable(c, ""), torrent: settingsUsable(c, core.ProtocolTorrent),
	}
}

func (input discoverySnapshot) usable(protocol string) bool {
	if protocol == core.ProtocolTorrent {
		return input.torrent
	}
	return input.usenet
}

func (input discoverySnapshot) retentionCutoff() time.Time {
	return input.now.Add(-367 * 24 * time.Hour)
}
