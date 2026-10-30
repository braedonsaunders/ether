package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// WithCurrentSource serializes a short durable observation with generation
// swaps and Stop. It never holds session.mu across the callback or source
// preparation. The callback must honor its bounded context.
func (m *Manager) WithCurrentSource(ctx context.Context, id string, fn func(context.Context, core.PlaybackSourceObservation) error) error {
	if fn == nil {
		return core.ErrNotAvailable
	}
	s := m.lookup(id)
	if s == nil {
		return core.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := lock(ctx, s.sourceCommit); err != nil {
		return err
	}
	defer unlock(s.sourceCommit)
	s.mu.Lock()
	if s.closed || s.cur == nil || s.cur.src == nil {
		s.mu.Unlock()
		return core.ErrNotFound
	}
	g := s.cur
	info := g.infoSnapshot()
	sourceID, name, size := g.src.ReleaseID(), g.src.Name(), g.src.Size()
	releaseID := ""
	if g.res != nil {
		releaseID = g.res.Release.ID
	}
	key := s.ident.key
	s.mu.Unlock()
	observation := core.PlaybackSourceObservation{}
	if sourceID != "" && !strings.HasPrefix(sourceID, "local:") && name != "" && size > 0 {
		data, _ := json.Marshal(struct {
			Key      core.RatingKey
			ID, Name string
			Size     int64
		}{key, sourceID, name, size})
		sum := sha256.Sum256(data)
		observation.Revision = "src-" + hex.EncodeToString(sum[:16])
		observation.ReleaseID = releaseID
		if !info.Estimated {
			if video := info.Video(); video != nil {
				observation.VideoCodec, observation.Height = video.Codec, video.Height
				if video.DolbyVision || video.ColorTransfer == "smpte2084" || video.ColorTransfer == "arib-std-b67" {
					value := true
					observation.HDR = &value
				} else if video.ColorTransfer == "bt709" {
					value := false
					observation.HDR = &value
				}
			}
		}
	}
	return fn(ctx, observation)
}
