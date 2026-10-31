package playback

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ResumedTracks is the probe a detail page may show for the release Prepare
// would still open. The key is the viewer plus the profile selection key
// ResumeFor already stores, not a client id and not a clock.
//
// A hit requires all of: this viewer's resume row, the same profile
// selection key, the source preference still allowing that protocol, the
// release not blocked, and a probe row for that same release whose NZB
// hash and file size still match. Saving a different release deletes the
// previous probe, so track ids from the old file cannot be served.
// Another viewer's row is never read. External subtitle URLs are stripped
// on save. There is no separate TTL; when the resume row stops being the
// next Prepare, the tracks stop too.
func (m *Manager) ResumedTracks(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) (core.MediaInfo, string, bool) {
	if user == nil || user.ID <= 0 {
		return core.MediaInfo{}, "", false
	}
	rp, err := m.ResumeFor(ctx, user, key, caps)
	if rp == nil && err == nil && !caps.Remote && m.deps.SelectProfile != nil {
		// A client behind a Docker bridge or LAN subnet may send location=wan
		// on /decision while /library/metadata carries no location query param.
		rcaps := caps
		rcaps.Remote = true
		if m.cfg.Transcode.RemoteMaxBitrateKbps > 0 && (rcaps.MaxBitrateKbps == 0 || m.cfg.Transcode.RemoteMaxBitrateKbps < rcaps.MaxBitrateKbps) {
			rcaps.MaxBitrateKbps = m.cfg.Transcode.RemoteMaxBitrateKbps
		}
		if profileKey(m.deps.SelectProfile(rcaps, user)) != profileKey(m.deps.SelectProfile(caps, user)) {
			rp, err = m.ResumeFor(ctx, user, key, rcaps)
		}
	}
	if err != nil || rp == nil || rp.Release.ID == "" || len(rp.NZB) == 0 {
		return core.MediaInfo{}, "", false
	}
	facts := m.persistedProbe(ctx, user.ID, key, rp.Release.ID, rp.NZB)
	if facts == nil || facts.releaseID != rp.Release.ID || facts.info.Estimated || facts.info.Video() == nil || len(facts.info.Streams) == 0 {
		return core.MediaInfo{}, "", false
	}
	info := facts.info
	info.Keyframes = nil
	info.Chapters = nil
	return info, rp.Release.ID, true
}

// ObservedMedia is historical evidence, not a promise about the next selected release.
// No provider, probe, source opening or keyframe loading is performed by this lookup.
type ObservedMedia struct {
	Info core.MediaInfo
	At   time.Time
}

// ObservedMediaInfo returns this viewer's most recent exact probe only while it
// still matches their current resume release, payload hash and media size. Facts
// expire after 30 days and never cross viewer boundaries.
func (m *Manager) ObservedMediaInfo(ctx context.Context, userID int64, key core.RatingKey) (*ObservedMedia, error) {
	if userID <= 0 {
		return nil, nil
	}
	// Avoid allocating/sorting full public session snapshots for every poster.
	m.mu.Lock()
	var matching []*session
	for _, session := range m.sessions {
		if session.ident.userID == userID && session.ident.key == key {
			matching = append(matching, session)
		}
	}
	m.mu.Unlock()
	var latest *ObservedMedia
	for _, session := range matching {
		session.mu.Lock()
		g, closed, started := session.cur, session.closed, session.startedAt
		session.mu.Unlock()
		if g == nil || closed {
			continue
		}
		info, ok := persistedProbeFacts(g.infoSnapshot())
		if ok && (latest == nil || started.After(latest.At)) {
			info.Chapters = nil
			latest = &ObservedMedia{Info: info, At: started}
		}
	}
	if latest != nil {
		return latest, nil
	}
	if m.resume == nil {
		return nil, nil
	}
	return m.resume.observedMedia(ctx, userID, key, m.now())
}

func (s *resumeStore) observedMedia(ctx context.Context, userID int64, key core.RatingKey, now time.Time) (*ObservedMedia, error) {
	var raw string
	var size, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT p.info_json,p.size,p.updated_at FROM playback_probe p
 JOIN playback_resume r ON r.user_id=p.user_id AND r.rating_key=p.rating_key
 AND r.release_id=p.release_id AND r.nzb_hash=p.nzb_hash AND r.size=p.size
 WHERE p.user_id=? AND p.rating_key=? AND p.nzb_hash<>'' AND p.updated_at BETWEEN ? AND ?`,
		userID, key, now.Add(-30*24*time.Hour).UnixMilli(), now.UnixMilli()).Scan(&raw, &size, &updated)
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
		return nil, nil
	}
	facts, ok := persistedProbeFacts(info)
	if !ok || facts.Size != size {
		return nil, nil
	}
	// Artwork consumers only need stream facts, never chapters or external paths.
	facts.Chapters = nil
	return &ObservedMedia{Info: facts, At: time.UnixMilli(updated)}, nil
}
