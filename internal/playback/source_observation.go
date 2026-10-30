package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// CurrentSource snapshots one generation without holding a lock across I/O.
func (m *Manager) CurrentSource(ctx context.Context, id string) (core.PlaybackSourceObservation, error) {
	if err := ctx.Err(); err != nil {
		return core.PlaybackSourceObservation{}, err
	}
	s := m.lookup(id)
	if s == nil {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	s.mu.Lock()
	if s.closed || s.cur == nil || s.cur.src == nil {
		s.mu.Unlock()
		return core.PlaybackSourceObservation{}, core.ErrNotFound
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
	return observation, nil
}
