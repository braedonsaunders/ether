package playback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("playback", 1, `
CREATE TABLE playback_resume (
	user_id      INTEGER NOT NULL,
	rating_key   INTEGER NOT NULL,
	release_id   TEXT    NOT NULL,
	release_json TEXT    NOT NULL,
	nzb_hash     TEXT    NOT NULL,
	profile      TEXT    NOT NULL DEFAULT '',
	size         INTEGER NOT NULL DEFAULT 0,
	duration_ms  INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (user_id, rating_key)
);
CREATE INDEX playback_resume_release ON playback_resume(release_id);
CREATE INDEX playback_resume_hash ON playback_resume(nzb_hash);
`)
	// Catalogue language is per-title context, not part of the profile
	// selection key. Rows remember the language a release was chosen
	// under so a later edit can drop that memory without changing the
	// stored `Name|<16 hex>` profile column.
	// Released main owns playback/3. Payload incarnations are playback/4.
	store.Register("playback", 3, `ALTER TABLE playback_resume ADD COLUMN original_language TEXT NOT NULL DEFAULT '';`)
	// Keep the existing playback migration immutable; indexes have their own
	// component history.
	store.Register("playback_index", 1, `CREATE TABLE playback_index (
	user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, release_id TEXT NOT NULL,
	points_json TEXT NOT NULL, PRIMARY KEY (user_id, rating_key, release_id)
);`)
}

const maxResumeKeyframes = 8192

// ResumePoint is the release that last served a user's title, so a resume
// (or a prefetched next episode) opens the same release and hits cached
// segments without asking the indexer again.
type ResumePoint struct {
	UserID    int64
	Key       core.RatingKey
	Release   core.Release
	NZB       []byte
	Profile   string        // quality profile name it was resolved for
	Size      int64         // media file size, 0 if never opened
	Duration  time.Duration // probed duration, 0 if unknown
	Keyframes []core.KeyframePoint
	UpdatedAt time.Time

	// profileKey is the stored `Name|<key>` value (a plain name on
	// legacy records); Profile above exposes the name only.
	profileKey string
	// originalLanguage is the catalogue language the release was chosen
	// under. Empty on rows written before that context was stored.
	originalLanguage string
}

// ReleaseID is the stable release identifier.
func (p *ResumePoint) ReleaseID() string { return p.Release.ID }

// profileKey is the stored form of a profile's selection key:
// `Name|<key>`, where key is p.SelectionKey — the hash of the
// configured definition computed pre-clamp by SelectProfile, so caps
// are not an input and the same definition keys identically for
// every client. Profiles built outside SelectProfile (tests/stubs)
// have no SelectionKey: fall back to the same canonical hash of the
// object's own fields. Release rules are included in that hash.
func profileKey(p core.QualityProfile) string {
	key := p.SelectionKey
	if key == "" {
		key = p.ComputeSelectionKey()
	}
	return p.Name + "|" + key
}

// profileName is the bare profile name of a stored profile value.
// New records are `Name|<16 lowercase hex>`. The suffix is stripped
// only when the last "|" introduces that key, so a legacy name or an
// owner name that itself contains "|" stays intact.
func profileName(stored string) string {
	i := strings.LastIndex(stored, "|")
	if i < 0 || !selectionKeySuffix(stored[i+1:]) {
		return stored
	}
	return stored[:i]
}

func selectionKeySuffix(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

type resumeRecord struct {
	userID           int64
	key              core.RatingKey
	releaseID        string
	release          core.Release
	nzbHash          string
	profile          string
	originalLanguage string
	size             int64
	duration         time.Duration
	updatedAt        time.Time
}

// resumeStore persists playback_resume rows; NZB bytes live on disk,
// content-addressed by SHA-256, so a resume never needs the indexer.
type resumeStore struct {
	db  *sql.DB
	dir string
	// held keeps resume records (and their keyframe indexes) through a
	// database outage on a clustered node; nil writes synchronously.
	held *store.Outbox[resumeKey, resumeWrite]

	payload *payloadStore

	nzbMu    sync.Mutex
	nzbCache map[string][]byte

	// fresh is the resume row this process last confirmed. A repeated play
	// of the same release skips the database read that only checks sameness.
	// live is the same choice kept for the next prepare, so that prepare does
	// not read the database or the NZB file while the durable write is still
	// catching up.
	freshMu sync.Mutex
	fresh   map[resumeKey]resumeFresh
	live    map[resumeKey]resumeLive
	// liveGen invalidates a save's late publish. Expiry can delete the row
	// after it commits and before the in-memory copy is stored; publishing
	// that copy would resurrect a resume the reconciler already dropped.
	liveGen map[resumeKey]uint64
	// beforeLiveNote, when set, runs after the row commits and before the
	// in-memory copy is published. Tests use it to expire the row in that window.
	beforeLiveNote func()
}

type resumeLive struct {
	profile          string
	originalLanguage string
	releaseID        string
	size             int64
	duration         time.Duration
	res              *core.Resolution
	probe            *probeCache
}

type resumeFresh struct {
	hash             string
	release          string
	profile          string
	originalLanguage string
	size             int64
	duration         time.Duration
	probe            bool
	points           int
}

type resumeKey struct {
	user int64
	key  core.RatingKey
}

// resumeWrite is one resume record whose NZB is already on disk.
type resumeWrite struct {
	rec    resumeRecord
	lease  *nzbLease
	points []core.KeyframePoint
	// probe travels in the same write as the record, so the persisted
	// probe can never land before (or outlive a queued replacement of)
	// the resume row it describes.
	probe core.MediaInfo
	nzb   []byte
}

// maxHeldResumes bounds resume records held through an outage (each may
// carry a keyframe index).
const maxHeldResumes = 1000

// hold makes record writes survive a database outage.
func (s *resumeStore) hold(avail *store.Availability, log *slog.Logger) {
	s.held = store.NewOutbox(store.OutboxOptions[resumeKey, resumeWrite]{
		Name: "playback resume", Availability: avail, Logger: log, Cap: maxHeldResumes,
		Timeout: 3 * time.Second,
		Merge: func(old, new resumeWrite) resumeWrite {
			s.payload.queueRelease(old.lease.owner)
			return new
		},
		Apply: func(ctx context.Context, _ resumeKey, w resumeWrite) error {
			if err := s.writeBundle(ctx, w.rec, w.lease, w.points, w.probe, w.nzb); err != nil {
				return err
			}
			// The bundle committed. Cleanup retains its own exact owner and
			// retry queue; its failure must not replay this committed writer.
			s.releaseNZB(w.lease)
			return nil
		},
	})
}

// close writes held records (bounded by ctx).
func (s *resumeStore) close(ctx context.Context) error {
	var err error
	if s.held != nil {
		err = s.held.Close(ctx)
	}
	s.payload.close()
	return errors.Join(err, s.payload.pendingError())
}

// put stores a record and its keyframe index: at once, or — on a clustered
// node that cannot reach the database — when the database is back. The
// NZB is written to local disk first either way.
func (s *resumeStore) put(ctx context.Context, rec resumeRecord, nzb []byte, points []core.KeyframePoint) error {
	return s.putWithProbe(ctx, rec, nzb, points, core.MediaInfo{})
}

// putWithProbe is put plus the probed facts of the serving generation,
// persisted in the same (possibly held) write as the resume row.
func (s *resumeStore) putWithProbe(ctx context.Context, rec resumeRecord, nzb []byte, points []core.KeyframePoint, probe core.MediaInfo) error {
	reqmemo.Forget(ctx, resumeMemoKey(rec.userID, rec.key))
	if len(nzb) == 0 {
		return errors.New("playback: empty nzb")
	}
	hash := nzbHash(nzb)
	if s.resumeCurrent(ctx, rec, hash, probe, len(points)) {
		s.rememberFresh(rec, hash, probe, len(points))
		return nil
	}
	lease, err := s.acquireNZB(ctx, rec, nzb)
	if err != nil {
		return fmt.Errorf("playback: store nzb: %w", err)
	}
	if s.held != nil {
		queued, err := s.held.Write(ctx, resumeKey{rec.userID, rec.key}, resumeWrite{rec: rec, lease: lease, points: points, probe: probe, nzb: nzb})
		if !queued && !errors.Is(err, errResumeCommitUnknown) {
			s.releaseNZB(lease)
		}
		return err
	}
	err = s.writeBundle(ctx, rec, lease, points, probe, nzb)
	if !errors.Is(err, errResumeCommitUnknown) {
		s.releaseNZB(lease)
	}
	if err == nil {
		s.rememberFresh(rec, hash, probe, len(points))
	}
	return err
}

func newResumeStore(db *sql.DB, dataDir string) (*resumeStore, error) {
	dir := filepath.Join(dataDir, "playback", "nzb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("playback: nzb dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload, err := openPayloadStore(ctx, db, dir)
	if err != nil {
		return nil, err
	}
	s := &resumeStore{db: db, dir: dir, payload: payload}
	s.sweep(ctx)
	return s, nil
}

// sweepMinAge keeps the startup sweep away from files a concurrent save may
// still be writing or about to reference.
const sweepMinAge = time.Hour

// sweep removes temp files left by a crash mid-write and NZB files no
// resume record references (e.g. the process died between writing the
// file and inserting its row). It runs once at startup.
func (s *resumeStore) sweep(ctx context.Context) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sweepMinAge)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		switch {
		case strings.HasPrefix(name, "nzb-") && strings.HasSuffix(name, ".tmp"):
			var owners int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_payload_owners WHERE domain=?`, s.payload.domain).Scan(&owners); err == nil && owners == 0 {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
		case strings.HasSuffix(name, ".nzb"):
			hash := strings.TrimSuffix(name, ".nzb")
			s.gcNZB(ctx, hash)
		}
	}
}

func nzbHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *resumeStore) nzbPath(hash string) string { return filepath.Join(s.dir, hash+".nzb") }

// acquireNZB commits a durable writer owner before touching the payload.
func (s *resumeStore) acquireNZB(ctx context.Context, rec resumeRecord, nzb []byte) (*nzbLease, error) {
	hash := nzbHash(nzb)
	owner, err := s.payload.reserve(ctx, rec.userID, "resume", "", fmt.Sprintf("%d/%d", rec.userID, rec.key), hash, "resume-write")
	if err != nil {
		return nil, err
	}
	if _, err := s.writeNZB(nzb); err != nil {
		s.payload.queueRelease(owner)
		return nil, err
	}
	return &nzbLease{hash: hash, owner: owner}, nil
}
func (s *resumeStore) releaseNZB(lease *nzbLease) error {
	err := s.payload.release(context.Background(), lease.owner)
	if err != nil {
		s.payload.queueRelease(lease.owner)
	}
	return err
}

func (s *resumeStore) writeNZB(nzb []byte) (string, error) {
	hash := nzbHash(nzb)
	p := s.nzbPath(hash)
	if _, err := os.Stat(p); err == nil {
		return hash, nil
	}
	f, err := os.CreateTemp(s.dir, "nzb-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(nzb); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hash, nil
}

func (s *resumeStore) readNZB(hash string) ([]byte, error) {
	b, err := os.ReadFile(s.nzbPath(hash))
	if err != nil {
		return nil, err
	}
	if nzbHash(b) != hash {
		return nil, fmt.Errorf("playback: nzb %s is corrupt", hash)
	}
	return b, nil
}

// resumeCurrent reports that this resume row, and any persistable probe or
// keyframe index, is already stored. A matching row with no probe yet must
// still be written so the next start can skip the container probe.
func (s *resumeStore) resumeCurrent(ctx context.Context, rec resumeRecord, hash string, probe core.MediaInfo, points int) bool {
	if s.freshHit(rec, hash, probe, points) {
		return true
	}
	got, err := s.get(ctx, rec.userID, rec.key)
	if err != nil || got == nil || got.release.ID == "" {
		return false
	}
	if got.nzbHash != hash || got.release.ID != rec.release.ID || got.profile != rec.profile || got.originalLanguage != rec.originalLanguage || got.size != rec.size || got.duration != rec.duration {
		return false
	}
	if _, ok := persistedProbeFacts(probe); ok {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_probe WHERE user_id=? AND rating_key=? AND release_id=? AND nzb_hash=?`, rec.userID, int64(rec.key), rec.release.ID, hash).Scan(&n); err != nil || n == 0 {
			return false
		}
	}
	if points > 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_index WHERE user_id=? AND rating_key=? AND release_id=?`, rec.userID, int64(rec.key), rec.release.ID).Scan(&n); err != nil || n == 0 {
			return false
		}
	}
	s.rememberFresh(rec, hash, probe, points)
	return true
}

func (s *resumeStore) freshHit(rec resumeRecord, hash string, probe core.MediaInfo, points int) bool {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	got, ok := s.fresh[resumeKey{rec.userID, rec.key}]
	if !ok || got.hash != hash || got.release != rec.release.ID || got.profile != rec.profile || got.originalLanguage != rec.originalLanguage || got.size != rec.size || got.duration != rec.duration {
		return false
	}
	if _, ok := persistedProbeFacts(probe); ok && !got.probe {
		return false
	}
	return points <= 0 || got.points > 0
}

func (s *resumeStore) rememberFresh(rec resumeRecord, hash string, probe core.MediaInfo, points int) {
	_, probeOK := persistedProbeFacts(probe)
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	if s.fresh == nil {
		s.fresh = map[resumeKey]resumeFresh{}
	}
	s.fresh[resumeKey{rec.userID, rec.key}] = resumeFresh{
		hash: hash, release: rec.release.ID, profile: rec.profile, originalLanguage: rec.originalLanguage,
		size: rec.size, duration: rec.duration, probe: probeOK, points: points,
	}
}

func (s *resumeStore) bumpLiveGenLocked(k resumeKey) uint64 {
	if s.liveGen == nil {
		s.liveGen = map[resumeKey]uint64{}
	}
	s.liveGen[k]++
	return s.liveGen[k]
}

func (s *resumeStore) storeLiveLocked(rec resumeRecord, nzb []byte, probe ...*probeCache) {
	res := &core.Resolution{Title: rec.key, Release: rec.release, NZB: append([]byte(nil), nzb...), ResolvedAt: rec.updatedAt}
	if s.live == nil {
		s.live = map[resumeKey]resumeLive{}
	}
	k := resumeKey{rec.userID, rec.key}
	var pc *probeCache
	if len(probe) > 0 && probe[0] != nil {
		pc = probe[0]
	} else if prev, ok := s.live[k]; ok && prev.releaseID == rec.release.ID {
		pc = prev.probe
	}
	s.live[k] = resumeLive{
		profile: rec.profile, originalLanguage: rec.originalLanguage, releaseID: rec.release.ID,
		size: rec.size, duration: rec.duration, res: res, probe: pc,
	}
}

// noteLive publishes a resume for the next prepare immediately. It supersedes
// any save that has not published yet.
func (s *resumeStore) noteLive(rec resumeRecord, nzb []byte, probe ...*probeCache) {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	s.bumpLiveGenLocked(resumeKey{rec.userID, rec.key})
	s.storeLiveLocked(rec, nzb, probe...)
}

func (s *resumeStore) liveProbe(userID int64, key core.RatingKey, releaseID string, nzb []byte) *probeCache {
	if s == nil || releaseID == "" {
		return nil
	}
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	got, ok := s.live[resumeKey{userID, key}]
	if !ok || got.probe == nil || got.releaseID != releaseID {
		return nil
	}
	if len(nzb) > 0 && got.probe.nzbHash != "" && got.probe.nzbHash != nzbHash(nzb) {
		return nil
	}
	copyInfo := core.CloneMediaInfo(got.probe.info)
	return &probeCache{
		releaseID:    got.probe.releaseID,
		info:         copyInfo,
		points:       append([]IndexPoint(nil), got.probe.points...),
		needsAugment: got.probe.needsAugment,
		nzbHash:      got.probe.nzbHash,
	}
}

// claimLive reserves the next publish. A forget between the claim and
// publishLive drops the publish, so expiry cannot be undone by the save
// that just lost the row.
func (s *resumeStore) claimLive(user int64, key core.RatingKey) uint64 {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	return s.bumpLiveGenLocked(resumeKey{user, key})
}

func (s *resumeStore) publishLive(gen uint64, rec resumeRecord, nzb []byte) {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	k := resumeKey{rec.userID, rec.key}
	if s.liveGen[k] != gen {
		return
	}
	s.storeLiveLocked(rec, nzb)
}

func (s *resumeStore) rememberedResolution(userID int64, key core.RatingKey, profile core.QualityProfile, profileKey, original string, caps core.ClientCaps) *core.Resolution {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	got, ok := s.live[resumeKey{userID, key}]
	if !ok || got.res == nil || got.profile != profileKey || got.releaseID == "" {
		return nil
	}
	if !rememberedOriginalAudioCurrent(profile, got.originalLanguage, original, got.res.Release) || !rememberedCodecCurrent(got.res.Release, caps) || !sourceFitsBitrate(got.size, got.duration, 0, caps.MaxBitrateKbps) {
		return nil
	}
	if got.probe != nil && !rememberedInfoCodecCurrent(got.probe.info, caps) {
		return nil
	}
	cp := *got.res
	cp.NZB = append([]byte(nil), got.res.NZB...)
	return &cp
}

func (s *resumeStore) livePoint(userID int64, key core.RatingKey) *ResumePoint {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	got, ok := s.live[resumeKey{userID, key}]
	if !ok || got.res == nil {
		return nil
	}
	return &ResumePoint{
		UserID: userID, Key: key, Release: got.res.Release, NZB: append([]byte(nil), got.res.NZB...),
		Profile: profileName(got.profile), Size: got.size, Duration: got.duration, UpdatedAt: got.res.ResolvedAt,
		profileKey: got.profile, originalLanguage: got.originalLanguage,
	}
}

func (s *resumeStore) forgetFresh(user int64, key core.RatingKey) {
	s.freshMu.Lock()
	k := resumeKey{user, key}
	s.bumpLiveGenLocked(k)
	delete(s.fresh, k)
	delete(s.live, k)
	s.freshMu.Unlock()
}

func (s *resumeStore) forgetFreshKey(key core.RatingKey) {
	s.freshMu.Lock()
	drop := map[resumeKey]struct{}{}
	for id := range s.fresh {
		if id.key == key {
			drop[id] = struct{}{}
		}
	}
	for id := range s.live {
		if id.key == key {
			drop[id] = struct{}{}
		}
	}
	for id := range drop {
		s.bumpLiveGenLocked(id)
		delete(s.fresh, id)
		delete(s.live, id)
	}
	s.freshMu.Unlock()
}

func (s *resumeStore) cachedNZB(hash string) ([]byte, bool) {
	s.nzbMu.Lock()
	defer s.nzbMu.Unlock()
	b, ok := s.nzbCache[hash]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

func (s *resumeStore) rememberNZB(hash string, b []byte) {
	if hash == "" || len(b) == 0 || len(b) > 1<<20 {
		return
	}
	s.nzbMu.Lock()
	defer s.nzbMu.Unlock()
	if s.nzbCache == nil {
		s.nzbCache = map[string][]byte{}
	}
	if len(s.nzbCache) >= 32 {
		for key := range s.nzbCache {
			delete(s.nzbCache, key)
			break
		}
	}
	s.nzbCache[hash] = append([]byte(nil), b...)
}

// readResumeNZB retains the actual payload across the filesystem read. A
// concurrent retention/invalidation delete cannot reclaim an admitted read.
func (s *resumeStore) readResumeNZB(ctx context.Context, rec *resumeRecord) ([]byte, error) {
	if b, ok := s.cachedNZB(rec.nzbHash); ok {
		return b, nil
	}
	owner, err := s.payload.reserve(ctx, rec.userID, "resume", "", fmt.Sprintf("%d/%d", rec.userID, rec.key), rec.nzbHash, "resume-read")
	if err != nil {
		return nil, err
	}
	b, readErr := s.readNZB(rec.nzbHash)
	if err := s.payload.release(context.Background(), owner); err != nil {
		s.payload.queueRelease(owner)
	}
	if readErr == nil {
		s.rememberNZB(rec.nzbHash, b)
	}
	return b, readErr
}

func (s *resumeStore) save(ctx context.Context, rec resumeRecord, nzb []byte) error {
	reqmemo.Forget(ctx, resumeMemoKey(rec.userID, rec.key))
	if len(nzb) == 0 {
		return errors.New("playback: empty nzb")
	}
	// Claim before the row is visible. Expiry that wins after the commit
	// bumps the generation and the publish below does not restore the copy.
	gen := s.claimLive(rec.userID, rec.key)
	lease, err := s.acquireNZB(ctx, rec, nzb)
	if err != nil {
		return err
	}
	err = s.writeBundle(ctx, rec, lease, nil, core.MediaInfo{}, nzb)
	if !errors.Is(err, errResumeCommitUnknown) {
		s.releaseNZB(lease)
	}
	if err == nil {
		if s.beforeLiveNote != nil {
			s.beforeLiveNote()
		}
		// The in-process copy is what ResumeFor reads first. A later
		// Remember has to replace the copy left by the play that just ended.
		s.publishLive(gen, rec, nzb)
	}
	return err
}

func saveResumeRowTx(ctx context.Context, tx *sql.Tx, rec resumeRecord, hash string) (string, error) {
	rel, err := json.Marshal(rec.release)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_index WHERE user_id=? AND rating_key=? AND release_id<>?`, rec.userID, int64(rec.key), rec.release.ID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_probe WHERE user_id=? AND rating_key=? AND release_id<>?`, rec.userID, int64(rec.key), rec.release.ID); err != nil {
		return "", err
	}
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT nzb_hash FROM playback_resume WHERE user_id=? AND rating_key=?`,
		rec.userID, int64(rec.key)).Scan(&oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO playback_resume (user_id, rating_key, release_id, release_json, nzb_hash, profile, original_language, size, duration_ms, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, rating_key) DO UPDATE SET
	release_id=excluded.release_id, release_json=excluded.release_json, nzb_hash=excluded.nzb_hash,
	profile=excluded.profile, original_language=excluded.original_language, size=excluded.size,
	duration_ms=excluded.duration_ms, updated_at=excluded.updated_at`,
		rec.userID, int64(rec.key), rec.release.ID, string(rel), hash, rec.profile, rec.originalLanguage, rec.size,
		rec.duration.Milliseconds(), rec.updatedAt.UnixMilli())
	if err != nil {
		return "", err
	}
	return oldHash, nil
}

const resumeColumns = `user_id, rating_key, release_id, release_json, nzb_hash, profile, original_language, size, duration_ms, updated_at`

func scanResume(sc interface{ Scan(...any) error }) (resumeRecord, error) {
	var (
		rec          resumeRecord
		key          int64
		relJSON      string
		durMS, updMS int64
	)
	if err := sc.Scan(&rec.userID, &key, &rec.releaseID, &relJSON, &rec.nzbHash, &rec.profile, &rec.originalLanguage, &rec.size, &durMS, &updMS); err != nil {
		return rec, err
	}
	rec.key = core.RatingKey(key)
	rec.duration = time.Duration(durMS) * time.Millisecond
	rec.updatedAt = time.UnixMilli(updMS)
	if err := json.Unmarshal([]byte(relJSON), &rec.release); err != nil {
		return rec, fmt.Errorf("playback: resume release json: %w", err)
	}
	if rec.release.ID == "" {
		rec.release.ID = rec.releaseID
	}
	return rec, nil
}

// get returns the record for a user and title, or nil when there is none.
func resumeMemoKey(userID int64, key core.RatingKey) string {
	return "playback:resume:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(int64(key), 10)
}

// A list builds several stream views per item, each asking for its resume
// record, so a read request reads each record once.
func (s *resumeStore) get(ctx context.Context, userID int64, key core.RatingKey) (*resumeRecord, error) {
	rec, err := reqmemo.Do(ctx, resumeMemoKey(userID, key), func() (*resumeRecord, error) {
		row := s.db.QueryRowContext(ctx, `SELECT `+resumeColumns+` FROM playback_resume WHERE user_id=? AND rating_key=?`,
			userID, int64(key))
		rec, err := scanResume(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &rec, nil
	})
	if err != nil || rec == nil {
		return nil, err
	}
	cp := *rec
	return &cp, nil
}

func (s *resumeStore) all(ctx context.Context) ([]resumeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+resumeColumns+` FROM playback_resume ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resumeRecord
	for rows.Next() {
		rec, err := scanResume(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *resumeStore) delete(ctx context.Context, userID int64, key core.RatingKey) error {
	reqmemo.Forget(ctx, resumeMemoKey(userID, key))
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT nzb_hash FROM playback_resume WHERE user_id=? AND rating_key=?`,
		userID, int64(key)).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		s.forgetFresh(userID, key)
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_resume WHERE user_id=? AND rating_key=?`, userID, int64(key)); err != nil {
		return err
	}
	// The record expired: nothing prunes its keyframe index any more (a
	// later save only replaces other releases' indexes for a live record).
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_index WHERE user_id=? AND rating_key=?`, userID, int64(key)); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_probe WHERE user_id=? AND rating_key=?`, userID, int64(key)); err != nil {
		return err
	}
	s.gcNZB(ctx, hash)
	s.forgetFresh(userID, key)
	return nil
}

// invalidateTitle drops a reusable source choice after an owner changes
// release policy. This table contains source identities, not watch progress.
// Keep the content-addressed payload and index caches for ordinary pruning.
func (s *resumeStore) invalidateTitle(ctx context.Context, key core.RatingKey) error {
	reqmemo.ForgetPrefix(ctx, "playback:resume:")
	if key.Kind() != core.KindShow && key.Kind() != core.KindSeason {
		_, err := s.db.ExecContext(ctx, `DELETE FROM playback_resume WHERE rating_key=?`, int64(key))
		if err == nil {
			s.forgetFreshKey(key)
		}
		return err
	}
	records, err := s.all(ctx)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if titleIncludes(key, rec.key) {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_resume WHERE user_id=? AND rating_key=?`, rec.userID, int64(rec.key)); err != nil {
				return err
			}
			s.forgetFresh(rec.userID, rec.key)
		}
	}
	return nil
}

// gcNZB removes an NZB file no row references any more.
func (s *resumeStore) gcNZB(ctx context.Context, hash string) {
	if err := s.payload.gc(ctx, hash); err != nil {
		return
	}
}

func (rec *resumeRecord) point(nzb []byte) *ResumePoint {
	return &ResumePoint{
		UserID:    rec.userID,
		Key:       rec.key,
		Release:   rec.release,
		NZB:       nzb,
		Profile:   profileName(rec.profile),
		Size:      rec.size,
		Duration:  rec.duration,
		UpdatedAt: rec.updatedAt,

		profileKey:       rec.profile,
		originalLanguage: rec.originalLanguage,
	}
}

func (s *resumeStore) saveIndex(ctx context.Context, userID int64, key core.RatingKey, releaseID string, points []core.KeyframePoint) error {
	return saveIndexWith(ctx, s.db, userID, key, releaseID, points)
}
func saveIndexWith(ctx context.Context, exec resumeExecer, userID int64, key core.RatingKey, releaseID string, points []core.KeyframePoint) error {
	if len(points) == 0 || releaseID == "" {
		return nil
	}
	if len(points) > maxResumeKeyframes {
		points = points[:maxResumeKeyframes]
	}
	b, err := json.Marshal(points)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO playback_index(user_id,rating_key,release_id,points_json) VALUES(?,?,?,?)
ON CONFLICT(user_id,rating_key,release_id) DO UPDATE SET points_json=excluded.points_json`, userID, int64(key), releaseID, string(b))
	return err
}

func (s *resumeStore) loadIndex(ctx context.Context, userID int64, key core.RatingKey, releaseID string) ([]core.KeyframePoint, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT points_json FROM playback_index WHERE user_id=? AND rating_key=? AND release_id=?`, userID, int64(key), releaseID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var points []core.KeyframePoint
	if err := json.Unmarshal([]byte(raw), &points); err != nil {
		return nil, err
	}
	if len(points) > maxResumeKeyframes {
		points = points[:maxResumeKeyframes]
	}
	return points, nil
}
