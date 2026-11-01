package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("release_observations", 1, `CREATE TABLE release_observations (parent_key INTEGER NOT NULL, source_hash TEXT NOT NULL, observed_ns INTEGER NOT NULL, PRIMARY KEY(parent_key,source_hash));`)
}

// FirstObservedLedger records genuine offer-observation times in the shared
// database. It neither discovers nor resolves releases. Source identities are
// hashed at rest; callers must supply a stable, source-qualified identity.
type FirstObservedLedger struct{ db *sql.DB }

func NewFirstObservedLedger(db *sql.DB) (*FirstObservedLedger, error) {
	if db == nil {
		return nil, errors.New("experience: observation database required")
	}
	return &FirstObservedLedger{db: db}, nil
}
func observationIdentity(parent core.RatingKey, source string) (string, error) {
	if !parent.Valid() || len(source) == 0 || len(source) > 4096 || strings.TrimSpace(source) == "" {
		return "", errors.New("experience: invalid observation identity")
	}
	switch parent.Kind() {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode, core.KindLocal:
	default:
		return "", errors.New("experience: invalid observation parent")
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:]), nil
}

// Observe records the first committed genuine observation atomically. The
// caller must supply the time the offer was actually observed, not its
// catalogue date, publication date or a time invented for an unknown offer.
func (l *FirstObservedLedger) Observe(ctx context.Context, parent core.RatingKey, source string, observed time.Time) (time.Time, error) {
	values, err := l.ObserveBatch(ctx, parent, []string{source}, observed)
	if err != nil {
		return time.Time{}, err
	}
	return values[source], nil
}

// FirstObservedAt returns nil when no genuine observation has been recorded.
func (l *FirstObservedLedger) FirstObservedAt(ctx context.Context, parent core.RatingKey, source string) (*time.Time, error) {
	if l == nil || l.db == nil {
		return nil, errors.New("experience: observation database required")
	}
	hash, err := observationIdentity(parent, source)
	if err != nil {
		return nil, err
	}
	var ns int64
	err = l.db.QueryRowContext(ctx, `SELECT observed_ns FROM release_observations WHERE parent_key=? AND source_hash=?`, int64(parent), hash).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	first := time.Unix(0, ns).UTC()
	return &first, nil
}

// ObserveBatch records a bounded group of source-qualified offers in one
// transaction. A failure rolls back the entire group. Empty input does not
// write or infer an observation.
func (l *FirstObservedLedger) ObserveBatch(ctx context.Context, parent core.RatingKey, sources []string, observed time.Time) (map[string]time.Time, error) {
	if l == nil || l.db == nil {
		return nil, errors.New("experience: observation database required")
	}
	if len(sources) > 500 {
		return nil, errors.New("experience: too many observations")
	}
	result := make(map[string]time.Time, len(sources))
	if len(sources) == 0 {
		return result, nil
	}
	if observed.IsZero() {
		return nil, errors.New("experience: observation time required")
	}
	ns := observed.UnixNano()
	if !time.Unix(0, ns).Equal(observed) {
		return nil, errors.New("experience: observation time out of range")
	}
	hashes := make(map[string]string, len(sources))
	for _, source := range sources {
		hash, err := observationIdentity(parent, source)
		if err != nil {
			return nil, err
		}
		hashes[source] = hash
	}
	ordered := make([]string, 0, len(hashes))
	for source := range hashes {
		ordered = append(ordered, source)
	}
	sort.Strings(ordered)
	// Existing observations are immutable. Read them in one query so a warm
	// browse never takes a write lock or creates new database tuples.
	args := make([]any, 0, len(ordered)+1)
	args = append(args, int64(parent))
	placeholders := make([]string, 0, len(ordered))
	for _, source := range ordered {
		args = append(args, hashes[source])
		placeholders = append(placeholders, "?")
	}
	rows, err := l.db.QueryContext(ctx, `SELECT source_hash,observed_ns FROM release_observations WHERE parent_key=? AND source_hash IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]int64, len(ordered))
	for rows.Next() {
		var hash string
		var first int64
		if err = rows.Scan(&hash, &first); err != nil {
			rows.Close()
			return nil, err
		}
		existing[hash] = first
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0)
	for _, source := range ordered {
		if first, ok := existing[hashes[source]]; ok {
			result[source] = time.Unix(0, first).UTC()
		} else {
			missing = append(missing, source)
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	// Sort by the actual unique key to avoid opposing lock orders across nodes.
	sort.Slice(missing, func(i, j int) bool { return hashes[missing[i]] < hashes[missing[j]] })
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, source := range missing {
		_, err = tx.ExecContext(ctx, `INSERT INTO release_observations(parent_key,source_hash,observed_ns) VALUES(?,?,?) ON CONFLICT(parent_key,source_hash) DO NOTHING`, int64(parent), hashes[source], ns)
		if err != nil {
			return nil, err
		}
		var first int64
		err = tx.QueryRowContext(ctx, `SELECT observed_ns FROM release_observations WHERE parent_key=? AND source_hash=?`, int64(parent), hashes[source]).Scan(&first)
		if err != nil {
			return nil, err
		}
		result[source] = time.Unix(0, first).UTC()
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// CandidateObservationSource names a genuine candidate release without
// exposing its raw identity. Empty, whitespace-only or unbounded IDs cannot
// identify a stable offer and have no observation timestamp.
func CandidateObservationSource(releaseID string) (string, bool) {
	if len(releaseID) == 0 || len(releaseID) > 4096 || strings.TrimSpace(releaseID) == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(releaseID))
	return "candidate:" + hex.EncodeToString(sum[:]), true
}
