package playback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// Probed media facts for a resume record's release, so a restart does
	// not put a container probe back on the first Play of a title already
	// played. Keyed like playback_index; a row is only ever read for the
	// release a live resume record names.
	store.Register("playback_probe", 1, `CREATE TABLE playback_probe (
	user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, release_id TEXT NOT NULL,
	nzb_hash TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0,
	info_json TEXT NOT NULL, updated_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, rating_key, release_id)
);`)
}

// probeFactsRecord is the on-disk shape of playback_probe.info_json.
// Rows written before facts were versioned do not carry FactsVersion and
// are treated as misses so the next open probes again.
type probeFactsRecord struct {
	FactsVersion int            `json:"factsVersion"`
	Info         core.MediaInfo `json:"info"`
}

func decodePersistedProbe(raw string) (core.MediaInfo, bool) {
	var rec probeFactsRecord
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.FactsVersion != core.ProbeFactsVersion {
		return core.MediaInfo{}, false
	}
	return rec.Info, true
}

// Bounds on a persisted probe. Stream/chapter counts of real containers
// are far below these; anything larger is not a probe worth trusting.
const (
	maxPersistedProbeBytes    = 64 << 10
	maxPersistedProbeStreams  = 256
	maxPersistedProbeChapters = 1024
)

// persistedProbeFacts returns the sanitized, persistable form of a probed
// MediaInfo, or ok=false when the info is not an exact probe. External
// (sidecar subtitle) streams are dropped entirely: they are per-viewer
// enrichment, and StreamInfo.ExternalURL can carry an expiring or
// credential-bearing URL that must never reach disk. Keyframes travel in
// playback_index, not here (MediaInfo.Keyframes is `json:"-"` regardless).
func persistedProbeFacts(info core.MediaInfo) (core.MediaInfo, bool) {
	if info.Estimated || info.Size <= 0 || info.Duration <= 0 || len(info.Streams) == 0 {
		return core.MediaInfo{}, false
	}
	if len(info.Streams) > maxPersistedProbeStreams || len(info.Chapters) > maxPersistedProbeChapters {
		return core.MediaInfo{}, false
	}
	out := info
	out.Keyframes = nil
	out.Streams = make([]core.StreamInfo, 0, len(info.Streams))
	for _, s := range info.Streams {
		if s.External {
			continue
		}
		s.ExternalURL = ""
		out.Streams = append(out.Streams, s)
	}
	if len(out.Streams) == 0 || out.Video() == nil {
		return core.MediaInfo{}, false
	}
	out.Chapters = append([]core.Chapter(nil), info.Chapters...)
	return out, true
}

// saveProbe persists probed facts next to a resume record, bound to the
// digest of the NZB payload they were probed from. Without a payload
// there is nothing to bind to, so nothing is stored.
func (s *resumeStore) saveProbe(ctx context.Context, userID int64, key core.RatingKey, releaseID string, nzb []byte, info core.MediaInfo, at time.Time) error {
	return saveProbeWith(ctx, s.db, userID, key, releaseID, nzb, info, at)
}
func saveProbeWith(ctx context.Context, exec resumeExecer, userID int64, key core.RatingKey, releaseID string, nzb []byte, info core.MediaInfo, at time.Time) error {
	facts, ok := persistedProbeFacts(info)
	if !ok || releaseID == "" || len(nzb) == 0 {
		return nil
	}
	b, err := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: facts})
	if err != nil {
		return err
	}
	if len(b) > maxPersistedProbeBytes {
		return nil
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO playback_probe(user_id,rating_key,release_id,nzb_hash,size,info_json,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(user_id,rating_key,release_id) DO UPDATE SET nzb_hash=excluded.nzb_hash, size=excluded.size, info_json=excluded.info_json, updated_at=excluded.updated_at`,
		userID, int64(key), releaseID, nzbHash(nzb), facts.Size, string(b), at.UnixMilli())
	return err
}

// loadProbe rebuilds the warm probeCache for a resume release: the
// persisted probe plus the persisted keyframe index. A row is a hit
// only for the exact NZB payload it was probed from (digest match), and
// probeGeneration additionally refuses the facts when the opened
// source's identity or size disagrees. Sidecar enrichment was stripped
// on save, so the rebuilt entry asks for AugmentMedia to run again
// (needsAugment).
func (s *resumeStore) loadProbe(ctx context.Context, userID int64, key core.RatingKey, releaseID string, nzb []byte) (*probeCache, error) {
	if len(nzb) == 0 {
		return nil, nil
	}
	hash := nzbHash(nzb)
	var (
		raw  string
		size int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT size, info_json FROM playback_probe WHERE user_id=? AND rating_key=? AND release_id=? AND nzb_hash=?`,
		userID, int64(key), releaseID, hash).Scan(&size, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPersistedProbeBytes {
		return nil, nil
	}
	info, ok := decodePersistedProbe(raw)
	if !ok {
		return nil, nil // a corrupt or old-generation row is a cache miss
	}
	facts, ok := persistedProbeFacts(info)
	if !ok || facts.Size != size {
		return nil, nil
	}
	points, err := s.loadIndex(ctx, userID, key, releaseID)
	if err != nil {
		return nil, err
	}
	return &probeCache{
		releaseID:    releaseID,
		info:         facts,
		points:       indexPointsOf(points),
		needsAugment: true,
		nzbHash:      hash,
	}, nil
}

// persistedProbe is loadProbe as a cache lookup: any failure is a miss.
func (m *Manager) persistedProbe(ctx context.Context, userID int64, key core.RatingKey, releaseID string, nzb []byte) *probeCache {
	if m.resume == nil || releaseID == "" {
		return nil
	}
	facts, err := m.resume.loadProbe(ctx, userID, key, releaseID, nzb)
	if err != nil {
		m.log.Debug("persisted probe lookup failed", "key", key, "release", releaseID, "err", err)
		return nil
	}
	if facts != nil {
		m.log.Debug("reusing persisted probe", "key", key, "release", releaseID, "keyframes", len(facts.points))
	}
	return facts
}
