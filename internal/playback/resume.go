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
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
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
}

// ReleaseID is the stable release identifier.
func (p *ResumePoint) ReleaseID() string { return p.Release.ID }

// profileKey is the stored form of a profile's selection key:
// `Name|<key>`, where key is p.SelectionKey — the hash of the
// configured definition computed pre-clamp by SelectProfile, so caps
// are not an input and the same definition keys identically for
// every client. Profiles built outside SelectProfile (tests/stubs)
// have no SelectionKey: fall back to the same canonical hash of the
// object's own fields.
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
	userID    int64
	key       core.RatingKey
	releaseID string
	release   core.Release
	nzbHash   string
	profile   string
	size      int64
	duration  time.Duration
	updatedAt time.Time
}

// resumeStore persists playback_resume rows; NZB bytes live on disk,
// content-addressed by SHA-256, so a resume never needs the indexer.
type resumeStore struct {
	db  *sql.DB
	dir string
	// held keeps resume records (and their keyframe indexes) through a
	// database outage on a clustered node; nil writes synchronously.
	held *store.Outbox[resumeKey, resumeWrite]

	// nzbMu orders NZB file writes against gcNZB's count-then-remove;
	// nzbRefs counts NZBs written (or found on disk) whose row may not be
	// committed yet, which gcNZB must not remove: another record dropping
	// the last committed reference to the same NZB would otherwise delete
	// the file under a row about to point at it.
	nzbMu   sync.Mutex
	nzbRefs map[string]int
}

type resumeKey struct {
	user int64
	key  core.RatingKey
}

// resumeWrite is one resume record whose NZB is already on disk.
type resumeWrite struct {
	rec    resumeRecord
	hash   string
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
		Apply: func(ctx context.Context, _ resumeKey, w resumeWrite) error {
			if err := s.saveRow(ctx, w.rec, w.hash); err != nil {
				return err
			}
			if err := s.saveIndex(ctx, w.rec.userID, w.rec.key, w.rec.release.ID, w.points); err != nil {
				return err
			}
			return s.saveProbe(ctx, w.rec.userID, w.rec.key, w.rec.release.ID, w.nzb, w.probe, w.rec.updatedAt)
		},
	})
}

// close writes held records (bounded by ctx).
func (s *resumeStore) close(ctx context.Context) error {
	if s.held == nil {
		return nil
	}
	return s.held.Close(ctx)
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
	if len(nzb) == 0 {
		return errors.New("playback: empty nzb")
	}
	hash, err := s.acquireNZB(nzb)
	if err != nil {
		return fmt.Errorf("playback: store nzb: %w", err)
	}
	if s.held != nil {
		queued, err := s.held.Write(ctx, resumeKey{rec.userID, rec.key}, resumeWrite{rec: rec, hash: hash, points: points, probe: probe, nzb: nzb})
		if !queued {
			s.releaseNZB(hash)
		}
		// A queued record keeps its NZB referenced for the rest of the
		// process (the startup sweep reclaims it if the row never lands).
		return err
	}
	err = s.saveRow(ctx, rec, hash)
	s.releaseNZB(hash)
	if err != nil {
		return err
	}
	if err := s.saveIndex(ctx, rec.userID, rec.key, rec.release.ID, points); err != nil {
		return err
	}
	return s.saveProbe(ctx, rec.userID, rec.key, rec.release.ID, nzb, probe, rec.updatedAt)
}

func newResumeStore(db *sql.DB, dataDir string) (*resumeStore, error) {
	dir := filepath.Join(dataDir, "playback", "nzb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("playback: nzb dir: %w", err)
	}
	s := &resumeStore{db: db, dir: dir, nzbRefs: map[string]int{}}
	s.sweep(context.Background())
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
			_ = os.Remove(filepath.Join(s.dir, name))
		case strings.HasSuffix(name, ".nzb"):
			hash := strings.TrimSuffix(name, ".nzb")
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_resume WHERE nzb_hash=?`, hash).Scan(&n); err == nil && n == 0 {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
		}
	}
}

func nzbHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *resumeStore) nzbPath(hash string) string { return filepath.Join(s.dir, hash+".nzb") }

// acquireNZB stores an NZB and holds it against gcNZB until releaseNZB.
func (s *resumeStore) acquireNZB(nzb []byte) (string, error) {
	s.nzbMu.Lock()
	defer s.nzbMu.Unlock()
	hash, err := s.writeNZB(nzb)
	if err != nil {
		return "", err
	}
	s.nzbRefs[hash]++
	return hash, nil
}

func (s *resumeStore) releaseNZB(hash string) {
	s.nzbMu.Lock()
	defer s.nzbMu.Unlock()
	if s.nzbRefs[hash]--; s.nzbRefs[hash] <= 0 {
		delete(s.nzbRefs, hash)
	}
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

func (s *resumeStore) save(ctx context.Context, rec resumeRecord, nzb []byte) error {
	if len(nzb) == 0 {
		return errors.New("playback: empty nzb")
	}
	hash, err := s.acquireNZB(nzb)
	if err != nil {
		return fmt.Errorf("playback: store nzb: %w", err)
	}
	defer s.releaseNZB(hash)
	return s.saveRow(ctx, rec, hash)
}

// saveRow upserts the record for an NZB already on disk.
func (s *resumeStore) saveRow(ctx context.Context, rec resumeRecord, hash string) error {
	rel, err := json.Marshal(rec.release)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_index WHERE user_id=? AND rating_key=? AND release_id<>?`, rec.userID, int64(rec.key), rec.release.ID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playback_probe WHERE user_id=? AND rating_key=? AND release_id<>?`, rec.userID, int64(rec.key), rec.release.ID); err != nil {
		return err
	}
	var oldHash string
	err = s.db.QueryRowContext(ctx, `SELECT nzb_hash FROM playback_resume WHERE user_id=? AND rating_key=?`,
		rec.userID, int64(rec.key)).Scan(&oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO playback_resume (user_id, rating_key, release_id, release_json, nzb_hash, profile, size, duration_ms, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, rating_key) DO UPDATE SET
	release_id=excluded.release_id, release_json=excluded.release_json, nzb_hash=excluded.nzb_hash,
	profile=excluded.profile, size=excluded.size, duration_ms=excluded.duration_ms, updated_at=excluded.updated_at`,
		rec.userID, int64(rec.key), rec.release.ID, string(rel), hash, rec.profile, rec.size,
		rec.duration.Milliseconds(), rec.updatedAt.UnixMilli())
	if err != nil {
		return err
	}
	if oldHash != "" && oldHash != hash {
		s.gcNZB(ctx, oldHash)
	}
	return nil
}

const resumeColumns = `user_id, rating_key, release_id, release_json, nzb_hash, profile, size, duration_ms, updated_at`

func scanResume(sc interface{ Scan(...any) error }) (resumeRecord, error) {
	var (
		rec          resumeRecord
		key          int64
		relJSON      string
		durMS, updMS int64
	)
	if err := sc.Scan(&rec.userID, &key, &rec.releaseID, &relJSON, &rec.nzbHash, &rec.profile, &rec.size, &durMS, &updMS); err != nil {
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
func (s *resumeStore) get(ctx context.Context, userID int64, key core.RatingKey) (*resumeRecord, error) {
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
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT nzb_hash FROM playback_resume WHERE user_id=? AND rating_key=?`,
		userID, int64(key)).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
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
	return nil
}

// invalidateTitle drops a reusable source choice after an owner changes
// release policy. This table contains source identities, not watch progress.
// Keep the content-addressed payload and index caches for ordinary pruning.
func (s *resumeStore) invalidateTitle(ctx context.Context, key core.RatingKey) error {
	if key.Kind() != core.KindShow && key.Kind() != core.KindSeason {
		_, err := s.db.ExecContext(ctx, `DELETE FROM playback_resume WHERE rating_key=?`, int64(key))
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
		}
	}
	return nil
}

// gcNZB removes an NZB file no row references any more.
func (s *resumeStore) gcNZB(ctx context.Context, hash string) {
	s.nzbMu.Lock()
	defer s.nzbMu.Unlock()
	if s.nzbRefs[hash] > 0 {
		return // a record being written is about to reference it
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_resume WHERE nzb_hash=?`, hash).Scan(&n); err != nil || n > 0 {
		return
	}
	_ = os.Remove(s.nzbPath(hash))
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

		profileKey: rec.profile,
	}
}

func (s *resumeStore) saveIndex(ctx context.Context, userID int64, key core.RatingKey, releaseID string, points []core.KeyframePoint) error {
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO playback_index(user_id,rating_key,release_id,points_json) VALUES(?,?,?,?)
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
