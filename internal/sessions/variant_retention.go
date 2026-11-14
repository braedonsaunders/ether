package sessions

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// Variant id retention. Every (title, release) ever listed as a Play
// Version kept its variants_ids row forever, and the registry loads the
// whole table into memory. Each id now records when it was last listed or
// resolved (last_seen, written at most daily and off the request path), and
// ids unseen for variantRetention are pruned from the table and from memory.
//
// Ids are a hash of (ratingKey, releaseID) with linear probing, not a
// counter: a title re-listed after its id was pruned re-derives the same
// id, so a client that still holds it resolves the same version again. Ids
// still mapped keep theirs (they are persisted), and a new pair is probed
// past every mapped id, so no listed id ever names two versions at once.
func init() {
	// variants is not pinned by the upgrade boundary tests. Existing rows
	// count as seen at upgrade, so nothing is pruned for variantRetention.
	const base = `ALTER TABLE variants_ids ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
CREATE INDEX variants_ids_last_seen ON variants_ids(last_seen);
`
	store.Register("variants", 2, base+`UPDATE variants_ids SET last_seen=CAST(strftime('%s','now') AS INTEGER);`)
	store.RegisterDialect("variants", 2, store.Postgres, store.TranslateDDL(base)+`UPDATE variants_ids SET last_seen=CAST(EXTRACT(EPOCH FROM now()) AS BIGINT);`)
}

const (
	// variantTouchEvery bounds last_seen writes to one per id per day.
	variantTouchEvery = int64(24 * time.Hour / time.Second)
	// variantRetention is how long an id nobody lists or resolves is kept.
	variantRetention = 90 * 24 * time.Hour
	// variantTouchFlush is how often queued touches are written.
	variantTouchFlush = time.Minute
	// maxVariantTouches bounds queued touches; past it a touch is dropped
	// unstamped and the next use retries.
	maxVariantTouches = 8192
	// variantTouchChunk bounds the ids in one statement.
	variantTouchChunk = 200
)

// touchLocked queues a last_seen write for id when its stamp is a day old.
// It never blocks or writes; the caller holds r.mu.
func (r *VariantIDs) touchLocked(id int64) {
	if r.db == nil {
		return
	}
	e, ok := r.byID[id]
	if !ok {
		return
	}
	now := r.now().Unix()
	if now-e.seen < variantTouchEvery {
		return
	}
	if _, queued := r.pending[id]; !queued && len(r.pending) >= maxVariantTouches {
		return
	}
	r.pending[id] = struct{}{}
	e.seen = now
	r.byID[id] = e
}

// start runs the touch flusher until ctx ends, flushing once more on the way
// out. A registry without a database has nothing to flush.
func (r *VariantIDs) Start(ctx context.Context, wg *sync.WaitGroup) {
	if r == nil || r.db == nil {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(variantTouchFlush)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = r.Flush(fctx)
				cancel()
				return
			case <-t.C:
				fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = r.Flush(fctx)
				cancel()
			}
		}
	}()
}

type variantRow struct {
	id  int64
	ref VariantRef
}

// flush writes queued touches. A row another node's prune removed while
// this node still serves it is restored, so the table never loses an id a
// registry hands out. Failed writes are queued again.
func (r *VariantIDs) Flush(ctx context.Context) error {
	r.mu.Lock()
	rows := make([]variantRow, 0, len(r.pending))
	for id := range r.pending {
		if e, ok := r.byID[id]; ok {
			rows = append(rows, variantRow{id: id, ref: e.ref})
		}
	}
	clear(r.pending)
	r.mu.Unlock()
	now := r.now().Unix()
	for i := 0; i < len(rows); i += variantTouchChunk {
		chunk := rows[i:min(i+variantTouchChunk, len(rows))]
		if err := r.writeSeen(ctx, chunk, now); err != nil {
			r.mu.Lock()
			for _, row := range rows[i:] {
				if len(r.pending) >= maxVariantTouches {
					break
				}
				r.pending[row.id] = struct{}{}
			}
			r.mu.Unlock()
			return err
		}
	}
	return nil
}

func (r *VariantIDs) writeSeen(ctx context.Context, rows []variantRow, now int64) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(rows)), ",")
	upd := make([]any, 0, len(rows)+2)
	upd = append(upd, now, now)
	ins := make([]any, 0, 4*len(rows))
	for _, row := range rows {
		upd = append(upd, row.id)
		ins = append(ins, row.id, int64(row.ref.Key), row.ref.ReleaseID, now)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE variants_ids SET last_seen=? WHERE last_seen<? AND media_id IN (`+marks+`)`, upd...); err != nil {
		return err
	}
	values := strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(rows)), ",")
	if _, err := tx.ExecContext(ctx, `INSERT INTO variants_ids(media_id, rating_key, release_id, last_seen) VALUES `+values+` ON CONFLICT DO NOTHING`, ins...); err != nil {
		return err
	}
	return tx.Commit()
}

// prune deletes ids unseen for variantRetention, except those in live
// (ids still held by a play identity: a live or admitting session, a pinned
// version list), which are touched first. It then drops pruned ids from
// memory.
func (r *VariantIDs) Prune(ctx context.Context, liveIDs []int64, liveRefs []VariantRef) (int64, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	now := r.now()
	r.mu.Lock()
	force := func(id int64) {
		if e, ok := r.byID[id]; ok {
			e.seen = now.Unix()
			r.byID[id] = e
			r.pending[id] = struct{}{}
		}
	}
	for _, id := range liveIDs {
		force(id)
	}
	for _, ref := range liveRefs {
		if id, ok := r.byPair[ref]; ok {
			force(id)
		}
	}
	r.mu.Unlock()
	// A failed touch must not let the delete below take a live id.
	if err := r.Flush(ctx); err != nil {
		return 0, err
	}
	// Rows written without last_seen (a node still on the previous release
	// during a rolling upgrade) count as seen now.
	if _, err := r.db.ExecContext(ctx, `UPDATE variants_ids SET last_seen=? WHERE last_seen=0`, now.Unix()); err != nil {
		return 0, err
	}
	cutoff := now.Add(-variantRetention).Unix()
	n, err := store.PruneBatched(ctx, r.db, `DELETE FROM variants_ids WHERE media_id IN (SELECT media_id FROM variants_ids WHERE last_seen<? ORDER BY last_seen LIMIT ?)`, cutoff)
	if err != nil {
		return n, err
	}
	return n, r.forgetPruned(ctx, cutoff)
}

// forgetPruned drops in-memory ids stale by their own stamp whose row is
// gone. A row still present (another node touched it) refreshes the stamp
// instead, and an id used since the check stays. Go maps never shrink, so
// after a large drop the maps are rebuilt at their new size.
func (r *VariantIDs) forgetPruned(ctx context.Context, cutoff int64) error {
	r.mu.Lock()
	var stale []int64
	for id, e := range r.byID {
		if e.seen < cutoff {
			stale = append(stale, id)
		}
	}
	size := len(r.byID)
	r.mu.Unlock()
	dropped := 0
	defer func() {
		if dropped == 0 || dropped*4 < size {
			return
		}
		r.mu.Lock()
		// maps.Clone keeps the old capacity; copy into right-sized maps.
		byID := make(map[int64]variantEntry, len(r.byID))
		maps.Copy(byID, r.byID)
		byPair := make(map[VariantRef]int64, len(r.byPair))
		maps.Copy(byPair, r.byPair)
		r.byID, r.byPair = byID, byPair
		r.mu.Unlock()
	}()
	for i := 0; i < len(stale); i += variantTouchChunk {
		chunk := stale[i:min(i+variantTouchChunk, len(stale))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := r.db.QueryContext(ctx, `SELECT media_id, last_seen FROM variants_ids WHERE media_id IN (`+marks+`)`, args...)
		if err != nil {
			return err
		}
		kept := map[int64]int64{}
		for rows.Next() {
			var id, seen int64
			if err := rows.Scan(&id, &seen); err != nil {
				rows.Close()
				return err
			}
			kept[id] = seen
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		r.mu.Lock()
		for _, id := range chunk {
			e, ok := r.byID[id]
			if !ok || e.seen >= cutoff {
				continue
			}
			if seen, found := kept[id]; found {
				e.seen = max(e.seen, seen)
				r.byID[id] = e
				continue
			}
			delete(r.byID, id)
			dropped++
			if r.byPair[e.ref] == id {
				delete(r.byPair, e.ref)
			}
		}
		r.mu.Unlock()
	}
	return nil
}
