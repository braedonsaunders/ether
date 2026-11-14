package sessions

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("variants", 1, `
CREATE TABLE variants_ids (
  media_id INTEGER PRIMARY KEY,
  rating_key INTEGER NOT NULL,
  release_id TEXT NOT NULL
);
CREATE UNIQUE INDEX variants_ids_pair ON variants_ids(rating_key, release_id);`)
}

const (
	variantIDBase  = frontend.VariantIDBase
	variantIDSpace = frontend.VariantIDSpace

	maxVariantIDs = 200_000
)

// ---------------------------------------------------------------------------
// Variant Media/Part ids

// variantEntry is a registry id's mapping and its last_seen as last read
// or touched (Unix seconds; see variants_retention.go).
type variantEntry struct {
	ref  VariantRef
	seen int64
}

type VariantIDs struct {
	mu     sync.Mutex
	db     *sql.DB
	byID   map[int64]variantEntry
	byPair map[VariantRef]int64
	// pending holds ids whose last_seen write is queued for the flusher.
	pending map[int64]struct{}
	now     func() time.Time
}

func NewVariantIDs(db *sql.DB, now func() time.Time) *VariantIDs {
	if now == nil {
		now = time.Now
	}
	return &VariantIDs{db: db, byID: map[int64]variantEntry{}, byPair: map[VariantRef]int64{}, pending: map[int64]struct{}{}, now: now}
}

// Persisted mappings are fetched by indexed key on demand. Browsing one
// page must never load the registry for the entire library.
func (r *VariantIDs) loadIDLocked(id int64) {
	if _, ok := r.byID[id]; ok || r.db == nil {
		return
	}
	var rk, seen int64
	var release string
	if err := r.db.QueryRow(`SELECT rating_key,release_id,last_seen FROM variants_ids WHERE media_id=?`, id).Scan(&rk, &release, &seen); err == nil {
		ref := VariantRef{Key: core.RatingKey(rk), ReleaseID: release}
		r.byID[id] = variantEntry{ref: ref, seen: seen}
		r.byPair[ref] = id
	}
}

// VariantHash lives in internal/frontend (host_sessions_experience.go).
var VariantHash = frontend.VariantHash

// idFor returns the stable Media/Part id of a non-Auto variant.
func (r *VariantIDs) IDFor(key core.RatingKey, releaseID string) int64 {
	ref := VariantRef{Key: key, ReleaseID: releaseID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		if _, cached := r.byPair[ref]; !cached {
			var id int64
			if err := r.db.QueryRow(`SELECT media_id FROM variants_ids WHERE rating_key=? AND release_id=?`, int64(key), releaseID).Scan(&id); err == nil {
				r.loadIDLocked(id)
			}
		}
	}
	if id, ok := r.byPair[ref]; ok {
		r.touchLocked(id)
		return id
	}
	if len(r.byID) >= maxVariantIDs && r.db == nil {
		r.byID = map[int64]variantEntry{}
		r.byPair = map[VariantRef]int64{}
	}
	id := VariantHash(key, releaseID)
	for {
		r.loadIDLocked(id)
		existing, ok := r.byID[id]
		if !ok || existing.ref == ref {
			break
		}
		id++ // collision: probe (the space is ~10^15 wide, so this terminates)
		if id >= variantIDBase+variantIDSpace {
			id = variantIDBase + 1
		}
	}
	seen := r.now().Unix()
	r.byID[id] = variantEntry{ref: ref, seen: seen}
	r.byPair[ref] = id
	if r.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = r.db.ExecContext(ctx,
			`INSERT INTO variants_ids(media_id, rating_key, release_id, last_seen) VALUES (?,?,?,?) ON CONFLICT DO NOTHING`,
			id, int64(key), releaseID, seen)
		cancel()
	}
	return id
}

func (r *VariantIDs) Lookup(id int64) (VariantRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadIDLocked(id)
	e, ok := r.byID[id]
	if ok {
		r.touchLocked(id)
	}
	return e.ref, ok
}

type VariantRef = frontend.VariantRef
