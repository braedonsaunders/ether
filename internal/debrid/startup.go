package debrid

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// reusablePayload uses stable release and episode identities before a tracker
// lookup. Some indexers only return a detail URL; resolving that URL again must
// not hide a complete descriptor already persisted by an earlier play.
func (s *Service) reusablePayload(ctx context.Context, rel core.Release) (Payload, bool) {
	p, ok := s.storedPayload(ctx, payloadKey(rel))
	if !ok || p.ReleaseID != rel.ID || s.entryByID(p.Provider) == nil {
		return Payload{}, false
	}
	if s.checkMediaPaused(p.Provider, p.URL) != nil {
		return Payload{}, false
	}
	if hash := strings.ToLower(rel.InfoHash); hash != "" && hash != p.InfoHash {
		return Payload{}, false
	}
	if !fileIdentityMatches(File{Path: p.Path}, rel.Target) {
		return Payload{}, false
	}
	p.Target = rel.Target
	return p, true
}

// fetchTorrent shares descriptor lookup and provider work across concurrent
// detail/Play requests. Work stays on the leading caller's goroutine/context;
// a cancelled leader releases waiters to retry rather than stranding a stream
// or starting an unowned background acquisition.
func (x *Indexer) fetchTorrent(ctx context.Context, rel core.Release) ([]byte, error) {
	if rel.ID == "" {
		return x.fetchTorrentOnce(ctx, rel)
	}
	key := payloadKey(rel) + "\x00" + strings.ToLower(rel.InfoHash) + "\x00" + rel.MagnetURL
	if rel.Target != nil {
		target, _ := json.Marshal(rel.Target)
		key += "\x00" + string(target)
	}
	data, err := x.flights.Do(ctx, key, func(ctx context.Context) ([]byte, error) {
		return x.fetchTorrentOnce(ctx, rel)
	})
	return append([]byte(nil), data...), err
}
