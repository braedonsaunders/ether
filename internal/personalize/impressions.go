package personalize

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// impression is one served hubs response, compacted for the recorder.
type impression struct {
	userID int64
	at     time.Time
	hc     core.HubContext
	rows   []servedRow
}

type servedRow struct {
	hubID, kind, title string
	position           int
	itemsOnly          bool
	itemOffset         int
	keys               []core.RatingKey
}

// recorder batches impressions in memory and writes them to SQLite in the
// background. Record never blocks: when the buffer is full the impression
// is dropped and counted, because a Plex response must never wait on
// bookkeeping.
type recorder struct {
	store    *sqlStore
	log      *slog.Logger
	ch       chan impression
	flushReq chan chan struct{}
	every    time.Duration
	maxBatch int
	dropped  atomic.Int64
	written  atomic.Int64
}

func newRecorder(st *sqlStore, log *slog.Logger, buffer int, every time.Duration) *recorder {
	return &recorder{store: st, log: log, ch: make(chan impression, buffer), flushReq: make(chan chan struct{}),
		every: every, maxBatch: 256}
}

// record queues an impression without blocking.
func (r *recorder) record(imp impression) {
	select {
	case r.ch <- imp:
	default:
		r.dropped.Add(1)
	}
}

// flush writes everything queued so far and returns when done (tests,
// shutdown, and admin reads that want fresh numbers).
func (r *recorder) flush(ctx context.Context) {
	done := make(chan struct{})
	select {
	case r.flushReq <- done:
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// run is the writer loop; it drains and writes the remaining batch when
// ctx ends.
func (r *recorder) run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	var batch []impression
	write := func() {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		rows, items := aggregate(batch, r.store.loc)
		if err := r.store.writeImpressions(wctx, rows, items); err != nil {
			r.log.Warn("personalize: writing impressions failed", "err", err, "served", len(batch))
		} else {
			r.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	drain := func() {
		for {
			select {
			case imp := <-r.ch:
				batch = append(batch, imp)
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
		case imp := <-r.ch:
			batch = append(batch, imp)
			if len(batch) >= r.maxBatch {
				write()
			}
		case <-t.C:
			write()
		case done := <-r.flushReq:
			drain()
			write()
			close(done)
		}
	}
}

// aggregate compacts a batch: one row per (user, day, hub) and per (user,
// day, item, hub), however many times the client polled.
func aggregate(batch []impression, loc *time.Location) ([]rowAgg, []itemAgg) {
	type rk struct {
		user int64
		day  int
		hub  string
	}
	type ik struct {
		user int64
		day  int
		key  core.RatingKey
		hub  string
	}
	rowIdx := map[rk]int{}
	itemIdx := map[ik]int{}
	var rows []rowAgg
	var items []itemAgg
	for _, imp := range batch {
		day := dayOf(imp.at, loc)
		at := ms(imp.at)
		for _, sr := range imp.rows {
			if !sr.itemsOnly {
				k := rk{imp.userID, day, sr.hubID}
				if i, ok := rowIdx[k]; ok {
					r := &rows[i]
					r.serves++
					r.bestPosition = min(r.bestPosition, sr.position)
					r.first, r.last = min(r.first, at), max(r.last, at)
					r.hc, r.title = imp.hc, sr.title
				} else {
					rowIdx[k] = len(rows)
					rows = append(rows, rowAgg{userID: imp.userID, day: day, hubID: sr.hubID, kind: sr.kind, title: sr.title,
						serves: 1, bestPosition: sr.position, first: at, last: at, hc: imp.hc})
				}
			}
			for idx, key := range sr.keys {
				rank := sr.position*1000 + sr.itemOffset + idx
				k := ik{imp.userID, day, key, sr.hubID}
				if i, ok := itemIdx[k]; ok {
					it := &items[i]
					it.shows++
					it.bestRank = min(it.bestRank, rank)
					it.first, it.last = min(it.first, at), max(it.last, at)
				} else {
					itemIdx[k] = len(items)
					items = append(items, itemAgg{userID: imp.userID, day: day, key: key, hubID: sr.hubID, kind: sr.kind,
						shows: 1, bestRank: rank, first: at, last: at})
				}
			}
		}
	}
	return rows, items
}

// kindOfHub maps a served hub id to its row kind. Rows the engine built
// are registered at assembly; the Plex front end's own rows and rows from
// before a restart are recognised by their id scheme.
func (e *Engine) kindOfHub(hubID string) string {
	e.hubMu.RLock()
	k, ok := e.hubKinds[hubID]
	e.hubMu.RUnlock()
	if ok {
		return k
	}
	return parseHubKind(hubID)
}

func parseHubKind(hubID string) string {
	switch {
	case hubID == "home.continue" || hubID == "continueWatching" || strings.Contains(hubID, ".inprogress"):
		return KindContinue
	case strings.Contains(hubID, ".recentlyadded"):
		return KindRecentlyAdded
	case hubID == "home.playlists":
		return KindPlaylists
	}
	stem := hubID
	for _, p := range []string{"home.", "movie.", "tv."} {
		if s, ok := strings.CutPrefix(hubID, p); ok {
			stem = s
			break
		}
	}
	best := ""
	for _, k := range kinds {
		if stem == k.slug || (k.dynamic && strings.HasPrefix(stem, k.slug+".")) {
			if len(k.slug) > len(best) {
				best = k.kind
			}
		}
	}
	if best != "" {
		return best
	}
	if strings.HasPrefix(stem, "collection.") {
		return KindCollectionPrefix + strings.TrimPrefix(stem, "collection.")
	}
	return "other"
}

func (e *Engine) registerHub(hubID, kind string) {
	e.hubMu.Lock()
	if len(e.hubKinds) > 20000 {
		e.hubKinds = map[string]string{}
	}
	e.hubKinds[hubID] = kind
	e.hubMu.Unlock()
}

// RecordImpressions implements core.ImpressionSink: it records what a user
// was shown, asynchronously. It never blocks.
func (e *Engine) RecordImpressions(user *core.User, hc core.HubContext, hubs []core.Hub) {
	if user == nil || len(hubs) == 0 {
		return
	}
	at := hc.Now
	if at.IsZero() {
		at = e.now()
	}
	imp := impression{userID: user.ID, at: at, hc: hc, rows: make([]servedRow, 0, len(hubs))}
	for i, h := range hubs {
		keys := make([]core.RatingKey, 0, len(h.Items))
		for _, t := range h.Items {
			keys = append(keys, t.Key)
		}
		imp.rows = append(imp.rows, servedRow{hubID: h.ID, kind: e.kindOfHub(h.ID), title: h.Title, position: i, keys: keys})
	}
	e.mu.Lock()
	e.lastCtx[user.ID] = hc
	if _, ok := e.users[user.ID]; !ok {
		e.users[user.ID] = *user
	}
	e.mu.Unlock()
	e.rec.record(imp)
	e.rankerRecordServe(user, hc, hubs, at) // serve log for the learned ranker (servelog.go)
}
