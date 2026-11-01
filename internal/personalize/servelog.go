package personalize

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The serve log is the learned ranker's training data (ranker.go). Unlike
// the compacted impression tables (impressions.go), it keeps one record per
// served page with what the viewer plausibly saw (the first
// RankerOptions.LogRows rows × LogItems items), each item's hand-tuned
// score, exploration treatment and a feature snapshot captured at assembly
// time, so the model trains on exactly what assembly knew when it ranked.
//
//	personalize_serves       one served page: viewer, time, surface, context.
//	personalize_serve_items  one item on it: row, positions, baseline score,
//	                         exploration propensity, features and label
//	                         (-1 pending, 0 negative, 1 played, 2 superseded
//	                         by a later serve of the same title that got the
//	                         play).
//	personalize_ranker       the trained model (JSON) and its metadata.
//
// Size is bounded three ways: an identical page re-served within
// serveRepeatWindow only bumps last_at, each viewer logs at most
// RankerOptions.MaxServesPerDay pages a day, and retention removes pages
// older than RankerOptions.Retention (60 days) and the oldest pages beyond
// RankerOptions.MaxItems logged items.
func init() {
	store.Register("personalize", 3, `
CREATE TABLE personalize_serves (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL,
  first_at INTEGER NOT NULL,
  last_at INTEGER NOT NULL,
  surface TEXT NOT NULL,
  device TEXT NOT NULL DEFAULT '',
  daypart TEXT NOT NULL DEFAULT '',
  weekend INTEGER NOT NULL DEFAULT 0,
  repeats INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX personalize_serves_user ON personalize_serves(user_id, last_at);
CREATE INDEX personalize_serves_last ON personalize_serves(last_at);
CREATE TABLE personalize_serve_items (
  serve_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  taste_key INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  row_pos INTEGER NOT NULL,
  item_pos INTEGER NOT NULL,
  baseline REAL NOT NULL DEFAULT 0,
  explore REAL NOT NULL DEFAULT 0,
  propensity REAL NOT NULL DEFAULT 0,
  fv INTEGER NOT NULL DEFAULT 0,
  features BLOB,
  label INTEGER NOT NULL DEFAULT -1,
  play_id INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (serve_id, row_pos, item_pos)
);
CREATE INDEX personalize_serve_items_user ON personalize_serve_items(user_id, taste_key);
CREATE INDEX personalize_serve_items_label ON personalize_serve_items(label);
CREATE TABLE personalize_ranker (
  id INTEGER PRIMARY KEY CHECK(id=1),
  trained_at INTEGER NOT NULL,
  model TEXT NOT NULL
);`)
}

// Serve labels.
const (
	labelPending    = -1
	labelNegative   = 0
	labelPlayed     = 1
	labelSuperseded = 2
)

// serveRepeatWindow: an identical page served again within this long is
// the same serve (clients poll /hubs every minute or so).
const serveRepeatWindow = 30 * time.Minute

// serveRec is one served page queued for the writer.
type serveRec struct {
	id      int64
	userID  int64
	at      time.Time
	surface string
	hc      core.HubContext
	items   []serveItem
}

type serveItem struct {
	key         core.RatingKey
	hubID, kind string
	rowPos      int
	itemPos     int
	snap        itemSnap
}

// serveOp is a write: a new serve, or a repeat of one (touch).
type serveOp struct {
	rec   *serveRec
	touch int64
	at    time.Time
}

// serveWriter batches serve-log writes off the request path, like the
// impression recorder: enqueue never blocks, a full buffer drops.
type serveWriter struct {
	db       *sql.DB
	log      *slog.Logger
	ch       chan serveOp
	flushReq chan chan struct{}
	every    time.Duration
	stop     <-chan struct{} // closed when the engine shuts down
	dropped  atomic.Int64
	written  atomic.Int64
	// ids maps the engine-local serve handles (serveRec.id, which repeats
	// touch) onto the ids the database assigned. Nodes of a cluster share
	// one database, so serve ids are never chosen locally. Owned by run.
	ids *serveIDs
}

// maxServeIDs bounds the handle map. The least recently used handles are
// forgotten first; a repeat of a forgotten serve is simply not counted.
const maxServeIDs = 8192

// serveIDs maps local serve handles onto database serve ids.
type serveIDs struct {
	m map[int64]serveHandle
	// top is the newest handle seen: handles are allocated in increasing
	// order, so it is the clock used to judge recency.
	top int64
}

type serveHandle struct {
	db   int64 // database serve id
	used int64 // top when the handle was last created or touched
}

func newServeIDs() *serveIDs { return &serveIDs{m: map[int64]serveHandle{}} }

func newServeWriter(db *sql.DB, log *slog.Logger, buffer int, every time.Duration, stop <-chan struct{}) *serveWriter {
	return &serveWriter{db: db, log: log, ch: make(chan serveOp, buffer), flushReq: make(chan chan struct{}), every: every, stop: stop,
		ids: newServeIDs()}
}

func (w *serveWriter) enqueue(op serveOp) bool {
	select {
	case w.ch <- op:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// flush writes everything queued so far.
func (w *serveWriter) flush(ctx context.Context) {
	done := make(chan struct{})
	select {
	case w.flushReq <- done:
	case <-ctx.Done():
		return
	case <-w.stop:
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-w.stop:
	}
}

func (w *serveWriter) run(ctx context.Context) {
	t := time.NewTicker(w.every)
	defer t.Stop()
	var batch []serveOp
	write := func() {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := writeServes(wctx, w.db, batch, w.ids); err != nil {
			w.log.Warn("personalize: writing serve log failed", "err", err, "ops", len(batch))
		} else {
			w.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	drain := func() {
		for {
			select {
			case op := <-w.ch:
				batch = append(batch, op)
			default:
				return
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			drain()
			write()
			return
		case op := <-w.ch:
			batch = append(batch, op)
			if len(batch) >= 64 {
				write()
			}
		case <-t.C:
			write()
		case done := <-w.flushReq:
			drain()
			write()
			close(done)
		}
	}
}

// writeServes writes a batch in one transaction. New serves get their id
// from the database (ids maps each local handle onto it, for later
// touches); ids is only updated once the batch commits. New serves are
// written before touches, so a repeat queued just ahead of its own serve
// (two requests racing in logServe) still finds it.
func writeServes(ctx context.Context, db *sql.DB, ops []serveOp, ids *serveIDs) error {
	if ids == nil {
		ids = newServeIDs()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	serveStmt, err := tx.PrepareContext(ctx, `INSERT INTO personalize_serves
		(user_id, first_at, last_at, surface, device, daypart, weekend) VALUES (?,?,?,?,?,?,?) RETURNING id`)
	if err != nil {
		return err
	}
	defer serveStmt.Close()
	itemStmt, err := tx.PrepareContext(ctx, `INSERT INTO personalize_serve_items
		(serve_id, user_id, item_key, taste_key, hub_id, kind, row_pos, item_pos, baseline, explore, propensity, fv, features)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (serve_id, row_pos, item_pos) DO NOTHING`)
	if err != nil {
		return err
	}
	defer itemStmt.Close()
	touchStmt, err := tx.PrepareContext(ctx, `UPDATE personalize_serves SET last_at = CASE WHEN ? > last_at THEN ? ELSE last_at END,
		repeats = repeats + 1 WHERE id = ?`)
	if err != nil {
		return err
	}
	defer touchStmt.Close()
	fresh := map[int64]int64{}
	for _, op := range ops {
		if op.rec == nil {
			continue
		}
		r := op.rec
		at := ms(r.at)
		var id int64
		if err := serveStmt.QueryRowContext(ctx, r.userID, at, at, r.surface, r.hc.Device, r.hc.Daypart, b2i(r.hc.Weekend)).Scan(&id); err != nil {
			return err
		}
		fresh[r.id] = id
		for _, it := range r.items {
			if _, err := itemStmt.ExecContext(ctx, id, r.userID, int64(it.key), int64(tasteKey(it.key)), it.hubID, it.kind,
				it.rowPos, it.itemPos, float64(it.snap.baseline), float64(it.snap.explore), float64(it.snap.propensity),
				rawFeatureVersion, encodeFeatures(it.snap.raw[:])); err != nil {
				return err
			}
		}
	}
	var touched []int64
	for _, op := range ops {
		if op.rec != nil {
			continue
		}
		id, ok := fresh[op.touch]
		if !ok {
			var h serveHandle
			h, ok = ids.m[op.touch]
			id = h.db
		}
		if !ok {
			continue
		}
		if _, err := touchStmt.ExecContext(ctx, ms(op.at), ms(op.at), id); err != nil {
			return err
		}
		touched = append(touched, op.touch)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ids.record(fresh, touched)
	return nil
}

// record remembers a committed batch's new handles and marks the touched
// ones as recently used, then evicts the least recently used half when
// the map is full. A long-lived serve (a screen left on Home, re-served
// every few minutes) keeps being touched and so is never evicted.
func (s *serveIDs) record(fresh map[int64]int64, touched []int64) {
	for h := range fresh {
		s.top = max(s.top, h)
	}
	for h, id := range fresh {
		s.m[h] = serveHandle{db: id, used: h}
	}
	for _, h := range touched {
		if e, ok := s.m[h]; ok {
			e.used = s.top
			s.m[h] = e
		}
	}
	if len(s.m) <= maxServeIDs {
		return
	}
	cut := s.top - maxServeIDs/2
	for h, e := range s.m {
		if e.used <= cut {
			delete(s.m, h)
		}
	}
	if len(s.m) > maxServeIDs {
		clear(s.m)
	}
}

// encodeFeatures packs a feature snapshot as little-endian float32s.
func encodeFeatures(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// decodeSnapshot reads a stored snapshot of any known version into the
// current layout: version 1 lacks the crowd, consensus and buzz features,
// which read as zero.
func decodeSnapshot(fv int, b []byte, out *[numRaw]float32) bool {
	switch fv {
	case rawFeatureVersion:
		return decodeFeatures(b, out[:])
	case 1:
		*out = [numRaw]float32{}
		return decodeFeatures(b, out[:v1Features])
	}
	return false
}

func decodeFeatures(b []byte, out []float32) bool {
	if len(b) != 4*len(out) {
		return false
	}
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return true
}

// ---- recording (RecordImpressions hook) -----------------------------------------

// surfaceOf names the page a hubs request served: "home", "movies" or
// "shows".
func (e *Engine) surfaceOf(hc core.HubContext, hubs []core.Hub) string {
	if hc.SectionID <= 0 {
		return "home"
	}
	if t, ok := e.opts.Sections[hc.SectionID]; ok {
		return sectionName(t)
	}
	for _, h := range hubs {
		switch {
		case strings.HasPrefix(h.ID, "movie."):
			return "movies"
		case strings.HasPrefix(h.ID, "tv."):
			return "shows"
		}
	}
	return "home"
}

// rankerRecordServe logs a served page for the learned ranker: the first
// LogRows rows × LogItems items that have a feature snapshot from the
// assembly that built them. Called from RecordImpressions; never blocks.
func (e *Engine) rankerRecordServe(user *core.User, hc core.HubContext, hubs []core.Hub, at time.Time) {
	e.rankerRecordServeNeutral(user, hc, hubs, at, nil, 0)
}

func (e *Engine) rankerRecordServeNeutral(user *core.User, hc core.HubContext, hubs []core.Hub, at time.Time, explicitKinds map[string]string, positionOffset int) {
	rs := e.rk
	if rs == nil || user == nil || len(hubs) == 0 {
		return
	}
	surface := e.surfaceOf(hc, hubs)
	snaps := rs.snapshots(user.ID, surface, hc.Bucket())
	if snaps == nil {
		return
	}
	rec := &serveRec{userID: user.ID, at: at, surface: surface, hc: hc}
	sig := fnv.New64a()
	sig.Write([]byte(hc.Bucket()))
	for rowIndex, h := range hubs {
		rowPos := rowIndex + positionOffset
		if rowPos >= rs.opts.LogRows {
			break
		}
		kind := e.kindOfHub(h.ID)
		if k := explicitKinds[h.ID]; k != "" {
			kind = k
		}
		if kind == KindContinue || kind == KindUpNext {
			continue
		}
		for itemPos, t := range h.Items {
			if itemPos >= rs.opts.LogItems {
				break
			}
			sn, ok := snaps[featKey{h.ID, t.Key}]
			if !ok {
				continue
			}
			rec.items = append(rec.items, serveItem{key: t.Key, hubID: h.ID, kind: kind, rowPos: rowPos, itemPos: itemPos, snap: *sn})
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], uint64(t.Key))
			sig.Write([]byte(h.ID))
			sig.Write(b[:])
		}
	}
	if len(rec.items) == 0 {
		return
	}
	rs.logServe(rec, sig.Sum64(), dayOf(at, e.loc))
}

// logServe dedupes repeats and enforces the daily cap, then queues.
func (rs *rankerState) logServe(rec *serveRec, sig uint64, day int) {
	sk := sigKey{rec.userID, rec.surface}
	rs.mu.Lock()
	if last, ok := rs.lastSig[sk]; ok && last.sig == sig && rec.at.Sub(last.at) < serveRepeatWindow && !rec.at.Before(last.at) {
		last.at = rec.at
		rs.lastSig[sk] = last
		rs.mu.Unlock()
		rs.writer.enqueue(serveOp{touch: last.id, at: rec.at})
		return
	}
	dk := dailyKey{rec.userID, day}
	if rs.daily[dk] >= rs.opts.MaxServesPerDay {
		rs.mu.Unlock()
		return
	}
	if len(rs.daily) > 4096 {
		for k := range rs.daily {
			if k.day != day {
				delete(rs.daily, k)
			}
		}
	}
	rs.daily[dk]++
	rec.id = rs.nextID.Add(1)
	if len(rs.lastSig) > 4096 {
		rs.lastSig = map[sigKey]lastServe{}
	}
	rs.lastSig[sk] = lastServe{id: rec.id, sig: sig, at: rec.at}
	// Exposure counts move immediately (exploration, fatigue features).
	// Copy on write: assemblies read the previous value without the lock.
	old := rs.expo[rec.userID]
	ux := &userExpo{items: map[core.RatingKey]expo{}}
	if old != nil {
		ux.serves = old.serves
		ux.items = make(map[core.RatingKey]expo, len(old.items)+len(rec.items))
		for k, v := range old.items {
			ux.items[k] = v
		}
	}
	rs.expo[rec.userID] = ux
	ux.serves++
	for _, it := range rec.items {
		x := ux.items[it.key]
		x.n++
		x.last = rec.at
		ux.items[it.key] = x
	}
	rs.mu.Unlock()
	if !rs.writer.enqueue(serveOp{rec: rec}) {
		rs.mu.Lock()
		delete(rs.lastSig, sk)
		rs.mu.Unlock()
	}
}

// ---- labels (trackProgress hook) -----------------------------------------------

// rankerLabelPlay labels the serve a new play most plausibly came from:
// among this viewer's serves of the title in the last attributionWindow,
// the one in the row the play was attributed to, else on the same
// surface, else the most recent. Earlier pending serves of the same title
// are marked superseded rather than negative: the viewer did want it.
func (e *Engine) rankerLabelPlay(ctx context.Context, p *playRecord) {
	rs := e.rk
	if rs == nil || p == nil || p.id == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	rs.writer.flush(fctx)
	cancel()
	if _, err := labelServe(ctx, rs.db, p.userID, tasteKey(p.key), p.started.Add(-attributionWindow), p.started, p.hubID, p.id); err != nil {
		e.log.Debug("personalize: labelling serve failed", "err", err)
	}
}

// labelServe implements rankerLabelPlay against the store; it reports
// whether a serve was labelled.
func labelServe(ctx context.Context, db *sql.DB, userID int64, tk core.RatingKey, from, to time.Time, hubID string, playID int64) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.serve_id, i.row_pos, i.item_pos, i.hub_id, s.surface, s.last_at, i.label
		FROM personalize_serve_items i JOIN personalize_serves s ON s.id = i.serve_id
		WHERE i.user_id = ? AND i.taste_key = ? AND s.last_at >= ? AND s.first_at <= ? AND i.label IN (?,?,?)`,
		userID, int64(tk), ms(from), ms(to), labelPending, labelNegative, labelPlayed)
	if err != nil {
		return false, err
	}
	type cand struct {
		serve        int64
		row, item    int
		hub, surface string
		last         int64
		label        int
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.serve, &c.row, &c.item, &c.hub, &c.surface, &c.last, &c.label); err != nil {
			rows.Close()
			return false, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(cs) == 0 {
		return false, nil
	}
	for _, c := range cs {
		if c.label == labelPlayed {
			return false, nil // a resumed play: the first one already labelled its serve
		}
	}
	wantSurface := surfaceOfHub(hubID)
	rank := func(c cand) int {
		switch {
		case hubID != "" && c.hub == hubID:
			return 2
		case hubID != "" && c.surface == wantSurface:
			return 1
		}
		return 0
	}
	best := 0
	for i := 1; i < len(cs); i++ {
		a, b := cs[i], cs[best]
		ra, rb := rank(a), rank(b)
		switch {
		case ra != rb:
			if ra > rb {
				best = i
			}
		case a.last != b.last:
			if a.last > b.last {
				best = i
			}
		case a.serve != b.serve:
			if a.serve > b.serve {
				best = i
			}
		case a.row*1000+a.item < b.row*1000+b.item:
			best = i
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for i, c := range cs {
		label, pid := labelSuperseded, int64(0)
		if i == best {
			label, pid = labelPlayed, playID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE personalize_serve_items SET label = ?, play_id = ? WHERE serve_id = ? AND row_pos = ? AND item_pos = ?`,
			label, pid, c.serve, c.row, c.item); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// finalizeNegatives labels pending serves whose attribution window closed
// without a play. It returns how many were labelled.
func finalizeNegatives(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE personalize_serve_items SET label = ? WHERE label = ? AND serve_id IN
		(SELECT id FROM personalize_serves WHERE last_at < ?)`, labelNegative, labelPending, ms(now.Add(-attributionWindow)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// purgeServes applies serve-log retention: pages older than retention,
// then the oldest pages beyond maxItems logged items.
func purgeServes(ctx context.Context, db *sql.DB, now time.Time, retention time.Duration, maxItems int) (int64, error) {
	var total int64
	cut := ms(now.Add(-retention))
	for _, q := range []string{
		`DELETE FROM personalize_serve_items WHERE serve_id IN (SELECT id FROM personalize_serves WHERE last_at < ?)`,
		`DELETE FROM personalize_serves WHERE last_at < ?`,
	} {
		res, err := db.ExecContext(ctx, q, cut)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if maxItems <= 0 {
		return total, nil
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_serve_items`).Scan(&n); err != nil {
		return total, err
	}
	if n <= maxItems {
		return total, nil
	}
	// The serve id below which dropping pages brings the log under the cap.
	var cutID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT serve_id FROM personalize_serve_items ORDER BY serve_id DESC LIMIT 1 OFFSET ?`, maxItems).Scan(&cutID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return total, err
	}
	if !cutID.Valid {
		return total, nil
	}
	for _, q := range []string{
		`DELETE FROM personalize_serve_items WHERE serve_id <= ?`,
		`DELETE FROM personalize_serves WHERE id <= ?`,
	} {
		res, err := db.ExecContext(ctx, q, cutID.Int64)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ---- exposure statistics ---------------------------------------------------------

// expo is how often and how recently a viewer was served a title.
type expo struct {
	n    int
	last time.Time
}

type userExpo struct {
	serves int
	items  map[core.RatingKey]expo
}

// loadExposure reads per-viewer serve counts and per-title server-wide
// play rates from the serve log since `since`.
func loadExposure(ctx context.Context, db *sql.DB, since time.Time) (map[int64]*userExpo, map[core.RatingKey]float64, error) {
	out := map[int64]*userExpo{}
	rows, err := db.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM personalize_serves WHERE last_at >= ? GROUP BY user_id`, ms(since))
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var uid int64
		var n int
		if err := rows.Scan(&uid, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		out[uid] = &userExpo{serves: n, items: map[core.RatingKey]expo{}}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT i.user_id, i.item_key, COUNT(*), MAX(s.last_at)
		FROM personalize_serve_items i JOIN personalize_serves s ON s.id = i.serve_id
		WHERE s.last_at >= ? GROUP BY i.user_id, i.item_key`, ms(since))
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var uid, key, last int64
		var n int
		if err := rows.Scan(&uid, &key, &n, &last); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ux := out[uid]
		if ux == nil {
			ux = &userExpo{items: map[core.RatingKey]expo{}}
			out[uid] = ux
		}
		ux.items[core.RatingKey(key)] = expo{n: n, last: fromMS(last)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rates := map[core.RatingKey]float64{}
	rows, err = db.QueryContext(ctx, `SELECT i.taste_key, COUNT(*), SUM(CASE WHEN i.label = 1 THEN 1 ELSE 0 END)
		FROM personalize_serve_items i JOIN personalize_serves s ON s.id = i.serve_id
		WHERE s.last_at >= ? AND i.label IN (0, 1) GROUP BY i.taste_key`, ms(since))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key int64
		var n, plays float64
		if err := rows.Scan(&key, &n, &plays); err != nil {
			return nil, nil, err
		}
		rates[core.RatingKey(key)] = smoothedRate(plays, n)
	}
	return out, rates, rows.Err()
}

// smoothedRate is a title's play rate per serve, shrunk toward a 1% prior
// so a single lucky serve does not look like a hit.
func smoothedRate(plays, serves float64) float64 {
	const prior, strength = 0.01, 50.0
	return (plays + prior*strength) / (serves + strength)
}
